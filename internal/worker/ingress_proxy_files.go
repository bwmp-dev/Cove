package worker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (e *Engine) cleanupIngressExposureStaging(change *ingressProxyChange) error {
	if change.Staging == "" {
		return nil
	}
	if !identifier.MatchString(change.ID) || change.Staging != ".ingress-listener-check-"+change.ID {
		return errors.New("private validation staging ownership is invalid")
	}
	path := filepath.Join(e.Root, change.Staging)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private validation staging is not an owned directory")
	}
	marker := filepath.Join(path, ".owner")
	owner, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(path)
		if readErr != nil || len(entries) != 0 {
			return errors.New("private validation staging has no ownership proof; it was left untouched")
		}
		return nil
	}
	if err != nil || !owner.Mode().IsRegular() || owner.Size() < 1 || owner.Size() > 256 {
		return errors.New("private validation staging ownership proof is unreadable")
	}
	data, err := os.ReadFile(marker)
	var identity struct {
		ChangeID string `json:"change_id"`
	}
	if err != nil || json.Unmarshal(data, &identity) != nil || identity.ChangeID != change.ID {
		return errors.New("private validation staging ownership changed; it was left untouched")
	}
	if err := os.RemoveAll(path); err != nil {
		return errors.New("private validation staging cleanup did not finish")
	}
	return nil
}

func copyIngressProxyFile(ctx context.Context, client *http.Client, container, path string, output io.Writer, limit int64) error {
	if len(container) != 64 || !containerIdentity.MatchString(container) || !strings.HasPrefix(path, "/") || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") || limit < 1 {
		return errors.New("invalid private proxy file request")
	}
	request, err := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/"+container+"/archive?path="+url.QueryEscape(path), nil)
	if err != nil {
		return errors.New("could not prepare private proxy file request")
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Docker did not complete the private proxy file read")
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return errIngressDockerMissing
	}
	if response.StatusCode != 200 {
		return errors.New("Docker refused the private proxy file read")
	}
	archive := tar.NewReader(io.LimitReader(response.Body, limit+(64<<10)))
	header, err := archive.Next()
	if err != nil || header.Size < 0 || header.Size > limit || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
		return errors.New("private proxy file is not a bounded regular file")
	}
	if _, err := io.CopyN(output, archive, header.Size); err != nil {
		return errors.New("private proxy file could not be copied completely")
	}
	if _, err := archive.Next(); err != io.EOF {
		return errors.New("private proxy file response contains unexpected archive entries")
	}
	return nil
}
