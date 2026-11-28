package worker

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const remoteBackupImage = "cove-backup-remote:restic0.19.1-rclone1.76.0"

const remoteBackupDockerfile = "FROM rclone/rclone:1.76.0 AS rclone\nFROM restic/restic:0.19.1\nCOPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone\nCOPY entrypoint.sh /cove-entrypoint.sh\nENTRYPOINT [\"/bin/sh\",\"/cove-entrypoint.sh\"]\n"
const remoteBackupEntrypoint = "#!/bin/sh\nset -eu\nif [ -n \"${COVE_REMOTE_PASSWORD:-}\" ]; then export RCLONE_CONFIG_COVE_PASS=\"$(printf '%s' \"$COVE_REMOTE_PASSWORD\" | rclone obscure -)\"; unset COVE_REMOTE_PASSWORD; fi\nif [ \"${1:-}\" = rclone ]; then shift; exec rclone \"$@\"; fi\nexec restic \"$@\"\n"

type StorageLocation struct {
	CACertificate string `json:"ca_certificate,omitempty"`
	Protocol      string `json:"protocol"`
	Endpoint      string `json:"endpoint"`
	Path          string `json:"path"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	HostKey       string `json:"host_key,omitempty"`
	AccessKey     string `json:"access_key,omitempty"`
	SecretKey     string `json:"secret_key,omitempty"`
	Region        string `json:"region,omitempty"`
	TLSMode       string `json:"tls_mode,omitempty"`
}

func ValidateStorage(s *StorageLocation) error {
	if s == nil {
		return errors.New("storage location required")
	}
	if s.CACertificate != "" {
		pool := x509.NewCertPool()
		if len(s.CACertificate) > 65536 || !pool.AppendCertsFromPEM([]byte(s.CACertificate)) {
			return errors.New("invalid storage CA certificate")
		}
	}
	for _, v := range []string{s.Endpoint, s.Path, s.Username, s.Password, s.HostKey, s.AccessKey, s.SecretKey, s.Region, s.TLSMode} {
		if strings.ContainsAny(v, "\x00\r\n") || len(v) > 8192 {
			return errors.New("invalid storage setting")
		}
	}
	if s.Path == "" || strings.Contains(s.Path, "..") || strings.ContainsAny(s.Path, "\\:") {
		return errors.New("a storage path without parent traversal is required")
	}
	if s.Protocol == "b2" {
		if s.Endpoint != "" || s.AccessKey == "" || s.SecretKey == "" || strings.HasPrefix(s.Path, "/") || s.CACertificate != "" {
			return errors.New("Backblaze B2 requires an application key ID, application key and bucket/path; endpoint and private CA must be empty")
		}
		return nil
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid storage endpoint")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("invalid storage port")
		}
	}
	switch s.Protocol {
	case "s3":
		if u.Scheme != "https" || u.Path != "" && u.Path != "/" || s.AccessKey == "" || s.SecretKey == "" {
			return errors.New("S3 requires an HTTPS endpoint and access credentials")
		}
	case "webdav":
		if u.Scheme != "https" || s.Username == "" || s.Password == "" {
			return errors.New("WebDAV requires HTTPS and credentials")
		}
	case "sftp":
		if u.Scheme != "sftp" || u.Path != "" && u.Path != "/" || s.Username == "" || s.Password == "" {
			return errors.New("SFTP requires an sftp://host endpoint and credentials")
		}
		if _, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(s.HostKey)); err != nil || len(strings.TrimSpace(string(rest))) > 0 {
			return errors.New("SFTP requires the server's SSH public host key")
		}
	case "ftps":
		if u.Scheme != "ftps" || u.Path != "" && u.Path != "/" || s.Username == "" || s.Password == "" || s.TLSMode != "explicit" && s.TLSMode != "implicit" {
			return errors.New("FTPS requires credentials and explicit or implicit TLS")
		}
	default:
		return errors.New("storage protocol must be s3, b2, sftp, ftps, or webdav")
	}
	return nil
}

func (e *Engine) remoteBackupHelper(ctx context.Context, out io.Writer) error {
	e.storageMu.Lock()
	defer e.storageMu.Unlock()
	if _, err := e.output(ctx, "docker", "image", "inspect", remoteBackupImage); err == nil {
		return nil
	}
	dir, err := os.MkdirTemp(e.Root, "backup-helper-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(remoteBackupDockerfile), 0600); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "entrypoint.sh"), []byte(remoteBackupEntrypoint), 0600); err != nil {
		return err
	}
	return e.Run(ctx, dir, []string{"docker", "build", "-t", remoteBackupImage, "."}, nil, out)
}

func storageEnvironment(s *StorageLocation) ([]string, error) {
	if err := ValidateStorage(s); err != nil {
		return nil, err
	}
	u, _ := url.Parse(s.Endpoint)
	env := []string{"RCLONE_CONFIG=/dev/null", "RCLONE_CONFIG_COVE_TYPE=" + s.Protocol, "RCLONE_CONFIG_COVE_USER=" + s.Username, "COVE_REMOTE_PASSWORD=" + s.Password}
	if s.CACertificate != "" {
		env = append(env, "RCLONE_CA_CERT=/storage-ca.pem")
	}
	switch s.Protocol {
	case "b2":
		env = append(env, "RCLONE_CONFIG_COVE_ACCOUNT="+s.AccessKey, "RCLONE_CONFIG_COVE_KEY="+s.SecretKey)
	case "s3":
		env = append(env, "RCLONE_CONFIG_COVE_PROVIDER=Other", "RCLONE_CONFIG_COVE_NO_CHECK_BUCKET=true", "RCLONE_CONFIG_COVE_ENDPOINT="+s.Endpoint, "RCLONE_CONFIG_COVE_ACCESS_KEY_ID="+s.AccessKey, "RCLONE_CONFIG_COVE_SECRET_ACCESS_KEY="+s.SecretKey, "RCLONE_CONFIG_COVE_REGION="+s.Region)
	case "webdav":
		env = append(env, "RCLONE_CONFIG_COVE_URL="+s.Endpoint, "RCLONE_CONFIG_COVE_VENDOR=other")
	case "sftp":
		port := u.Port()
		if port == "" {
			port = "22"
		}
		env = append(env, "RCLONE_CONFIG_COVE_HOST="+u.Hostname(), "RCLONE_CONFIG_COVE_PORT="+port, "RCLONE_CONFIG_COVE_KNOWN_HOSTS_FILE=/storage-known-hosts")
	case "ftps":
		port := u.Port()
		if port == "" {
			if s.TLSMode == "implicit" {
				port = "990"
			} else {
				port = "21"
			}
		}
		env[1] = "RCLONE_CONFIG_COVE_TYPE=ftp"
		env = append(env, "RCLONE_CONFIG_COVE_HOST="+u.Hostname(), "RCLONE_CONFIG_COVE_PORT="+port, "RCLONE_CONFIG_COVE_TLS="+strconv.FormatBool(s.TLSMode == "implicit"), "RCLONE_CONFIG_COVE_EXPLICIT_TLS="+strconv.FormatBool(s.TLSMode == "explicit"))
	}
	return env, nil
}

func storageMount(dir string, s *StorageLocation) ([]string, error) {
	mounts := []string{}
	if s.CACertificate != "" {
		file := filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(file, []byte(s.CACertificate), 0600); err != nil {
			return nil, err
		}
		mounts = append(mounts, "--mount", "type=bind,source="+file+",target=/storage-ca.pem,readonly")
	}
	if s.Protocol != "sftp" {
		return mounts, nil
	}
	knownHosts, err := storageKnownHosts(s)
	if err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "known_hosts")
	if err = os.WriteFile(file, []byte(knownHosts), 0600); err != nil {
		return nil, err
	}
	return append(mounts, "--mount", "type=bind,source="+file+",target=/storage-known-hosts,readonly"), nil
}

func (e *Engine) TestStorage(ctx context.Context, s *StorageLocation) error {
	env, err := e.storageEnvironment(s, nil)
	if err != nil {
		return err
	}
	if err = e.remoteBackupHelper(ctx, io.Discard); err != nil {
		return errors.New("could not prepare storage helper")
	}
	dir, err := os.MkdirTemp(e.Root, "storage-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	mounts, err := storageMount(dir, s)
	if err != nil {
		return err
	}
	name := "cove-storage-test-" + filepath.Base(dir)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", name}, nil, io.Discard)
	}()
	args := []string{"docker", "run", "--rm", "--name", name, "--network", "host", "--read-only", "--tmpfs", "/tmp"}
	args = append(args, mounts...)
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		args = append(args, "-e", k)
	}
	args = append(args, remoteBackupImage, "rclone", "lsf", "cove:"+s.Path, "--max-depth", "1", "--contimeout", "10s", "--timeout", "15s", "--retries", "1")
	if err = e.Run(ctx, e.Root, args, env, io.Discard); err != nil {
		return errors.New("storage connection failed; check the path, credentials, TLS, and host key")
	}
	return nil
}

func storageKnownHosts(s *StorageLocation) (string, error) {
	u, _ := url.Parse(s.Endpoint)
	host := u.Hostname()
	if u.Port() != "" && u.Port() != "22" {
		host = "[" + host + "]:" + u.Port()
	} else if net.ParseIP(host) != nil && strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.HostKey))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s", host, ssh.MarshalAuthorizedKey(key)), nil
}

type PanelStorageTransport struct {
	Image         string   `json:"image"`
	Dockerfile    string   `json:"dockerfile,omitempty"`
	Entrypoint    string   `json:"entrypoint,omitempty"`
	Environment   []string `json:"environment,omitempty"`
	CACertificate string   `json:"ca_certificate,omitempty"`
	KnownHosts    string   `json:"known_hosts,omitempty"`
	Repository    string   `json:"repository,omitempty"`
}

func PanelBackupTransport(s *StorageLocation) (PanelStorageTransport, error) {
	if s == nil {
		return PanelStorageTransport{Image: "restic/restic:0.19.1"}, nil
	}
	env, err := storageEnvironment(s)
	if err != nil {
		return PanelStorageTransport{}, err
	}
	knownHosts := ""
	if s.Protocol == "sftp" {
		knownHosts, err = storageKnownHosts(s)
		if err != nil {
			return PanelStorageTransport{}, err
		}
	}
	return PanelStorageTransport{Image: remoteBackupImage, Dockerfile: remoteBackupDockerfile, Entrypoint: remoteBackupEntrypoint, Environment: env, CACertificate: s.CACertificate, KnownHosts: knownHosts, Repository: "rclone:cove:" + s.Path}, nil
}
