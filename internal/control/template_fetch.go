package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"cove/internal/catalog"
)

var templateGitReference = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9/._-]{0,199}$`)

func validTemplateGitReference(value string) bool {
	return templateGitReference.MatchString(value) && !strings.Contains(value, "..") && !strings.HasSuffix(value, "/")
}

func templateSourceURL(value string) bool {
	u, e := url.Parse(value)
	if e == nil {
		if ip := net.ParseIP(u.Hostname()); ip != nil && !publicWebhookIP(ip) {
			return false
		}
		if strings.EqualFold(u.Hostname(), "localhost") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".localhost") {
			return false
		}
	}
	return e == nil && catalog.HTTPS(value) && u.RawQuery == "" && (u.Port() == "" || u.Port() == "443")
}

func (s *Server) fetchTemplateIndex(ctx context.Context, kind, address, ref string) ([]byte, string, error) {
	if !templateSourceURL(address) {
		return nil, "", errors.New("public HTTPS source without credentials or query required")
	}
	config, err := s.outboundConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	transport := publicWebhookTransport(config)
	defer transport.CloseIdleConnections()
	if kind == "index" {
		client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
		if err != nil {
			return nil, "", err
		}
		request.Header.Set("User-Agent", "Cove-template-catalogue")
		response, err := client.Do(request)
		if err != nil {
			return nil, "", errors.New("catalogue source could not be fetched")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, "", errors.New("catalogue source must return HTTP 200 without redirects")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
		if err != nil || len(data) > 8*1024*1024 {
			return nil, "", errors.New("catalogue source exceeds limits")
		}
		hash := sha256.Sum256(data)
		return data, hex.EncodeToString(hash[:]), nil
	}
	if kind != "git" {
		return nil, "", errors.New("unknown template source kind")
	}
	return s.fetchTemplateGitFile(ctx, address, ref, "cove-templates.json", "", transport)
}

func (s *Server) fetchTemplateGitFile(ctx context.Context, address, ref, file, token string, transport *http.Transport) ([]byte, string, error) {
	if !templateSourceURL(address) || len(file) > 512 || strings.HasPrefix(file, "/") || strings.Contains(file, "..") || strings.ContainsAny(file, "\\:\x00\r\n") {
		return nil, "", errors.New("valid public Git source and repository-relative file required")
	}
	if !validTemplateGitReference(ref) {
		return nil, "", errors.New("Git source requires an explicit branch, tag or commit reference")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "cove-template-git-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		timer := time.NewTicker(250 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			var size int64
			filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if entry.Type().IsRegular() {
					if info, e := entry.Info(); e == nil {
						size += info.Size()
					}
				}
				if size > 128*1024*1024 {
					cancel()
					return fs.SkipAll
				}
				return nil
			})
		}
	}()
	defer func() { cancel(); <-watchDone }()
	templatesDir := filepath.Join(dir, "empty-template")
	if err = os.Mkdir(templatesDir, 0700); err != nil {
		return nil, "", err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	u, _ := url.Parse(address)
	target := net.JoinHostPort(u.Hostname(), "443")
	var connections sync.Map
	proxy := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != target {
			http.Error(w, "destination forbidden", 403)
			return
		}
		upstream, e := transport.DialContext(ctx, "tcp", target)
		if e != nil {
			http.Error(w, "destination unavailable", 502)
			return
		}
		downstream, buffer, e := w.(http.Hijacker).Hijack()
		if e != nil {
			upstream.Close()
			return
		}
		connections.Store(downstream, upstream)
		defer connections.Delete(downstream)
		defer downstream.Close()
		defer upstream.Close()
		deadline := time.Now().Add(60 * time.Second)
		downstream.SetDeadline(deadline)
		upstream.SetDeadline(deadline)
		if _, e = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
			return
		}
		if buffer.Flush() != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(upstream, buffer); upstream.Close(); close(done) }()
		io.Copy(downstream, upstream)
		downstream.Close()
		<-done
	})}
	go proxy.Serve(listener)
	defer func() {
		proxy.Close()
		connections.Range(func(key, value any) bool { key.(net.Conn).Close(); value.(net.Conn).Close(); return true })
	}()
	proxyURL := "http://" + listener.Addr().String()
	environment := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "no-global-config"), "GIT_TERMINAL_PROMPT=0", "GIT_PROTOCOL_FROM_USER=0", "GIT_ALLOW_PROTOCOL=https", "GIT_CONFIG_COUNT=0", "GIT_TEMPLATE_DIR=" + templatesDir, "PATH=" + os.Getenv("PATH"), "SystemRoot=" + os.Getenv("SystemRoot"), "TEMP=" + dir, "TMP=" + dir, "NO_PROXY=", "no_proxy="}
	for _, key := range []string{"GIT_SSL_CAINFO", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	if token != "" {
		environment = append(environment, "COVE_GIT_TOKEN="+token)
	}
	run := func(args ...string) ([]byte, error) {
		base := []string{"-c", "http.proxy=" + proxyURL, "-c", "http.followRedirects=false", "-c", "core.hooksPath=" + filepath.Join(dir, "no-hooks"), "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-C", dir}
		if token != "" {
			base = append(base, "-c", `credential.helper=!f() { printf 'username=x-access-token\npassword=%s\n' "$COVE_GIT_TOKEN"; }; f`)
		}
		command := exec.CommandContext(ctx, "git", append(base, args...)...)
		command.Env = environment
		output := &templateOutput{limit: 8*1024*1024 + 1}
		command.Stdout = output
		command.Stderr = io.Discard
		if command.Run() != nil || output.exceeded {
			return nil, errors.New("Git catalogue fetch failed; check its public URL, reference and size")
		}
		return output.data, nil
	}
	if _, err = run("init", "--bare", "--quiet"); err != nil {
		return nil, "", err
	}
	if _, err = run("-c", "fetch.unpackLimit=1", "-c", "transfer.fsckObjects=true", "fetch", "--depth=1", "--no-tags", "--no-recurse-submodules", "--", address, ref); err != nil {
		return nil, "", err
	}
	revision, err := run("rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return nil, "", err
	}
	commit := strings.TrimSpace(string(revision))
	if !regexp.MustCompile(`^[a-f0-9]{40,64}$`).MatchString(commit) {
		return nil, "", errors.New("Git catalogue revision unavailable")
	}
	data, err := run("show", commit+":"+file)
	return data, commit, err
}

type templateOutput struct {
	data     []byte
	limit    int
	exceeded bool
}

func (b *templateOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data)+n > b.limit {
		b.exceeded = true
		return 0, errors.New("catalogue exceeds limits")
	}
	b.data = append(b.data, p...)
	return n, nil
}
