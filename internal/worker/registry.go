package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Registry struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func ValidateRegistry(r *Registry) error {
	if r == nil {
		return nil
	}
	u, err := url.Parse("https://" + r.Server)
	if err != nil || u.Host != r.Server || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(r.Server, "\\\r\n\t ") || len(r.Server) > 253 || r.Username == "" || r.Password == "" || len(r.Username) > 256 || len(r.Password) > 8192 || strings.ContainsAny(r.Username+r.Password, "\x00\r\n") {
		return errors.New("registry host, username, and password are required")
	}
	return nil
}

func registryConfig(dir string, r *Registry) (string, error) {
	if err := ValidateRegistry(r); err != nil {
		return "", err
	}
	path, err := os.MkdirTemp(dir, "registry-")
	if err != nil {
		return "", err
	}
	server := r.Server
	if server == "docker.io" || server == "index.docker.io" {
		server = "https://index.docker.io/v1/"
	}
	data, err := json.Marshal(map[string]any{"auths": map[string]any{server: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(r.Username + ":" + r.Password))}}})
	if err == nil {
		err = os.WriteFile(filepath.Join(path, "config.json"), data, 0600)
	}
	if err != nil {
		_ = os.RemoveAll(path)
		return "", err
	}
	return path, nil
}

func (e *Engine) TestRegistry(ctx context.Context, r *Registry) error {
	if r == nil {
		return errors.New("registry required")
	}
	if err := ValidateRegistry(r); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(e.Root, "registry-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	command := exec.CommandContext(ctx, "docker", "--config", dir, "login", r.Server, "--username", r.Username, "--password-stdin")
	command.Stdin = strings.NewReader(r.Password)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err = command.Run(); err != nil {
		return errors.New("registry authentication failed")
	}
	return nil
}
