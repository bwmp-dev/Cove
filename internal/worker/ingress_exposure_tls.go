package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

func (e *Engine) probeIngressExposureTLS(ctx context.Context, client *http.Client, container, changeID string, probes []routeCertificateProbe) error {
	if len(probes) == 0 {
		return nil
	}
	if !identifier.MatchString(changeID) || len(container) != 64 || !containerIdentity.MatchString(container) {
		return errors.New("listener TLS probe identity is invalid")
	}
	encoded, err := json.Marshal(probes)
	if err != nil || len(encoded) > 3<<20 {
		return errors.New("listener TLS probes exceed the public input limit")
	}
	base := "/tmp/cove-listener-probe-" + changeID
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	if err := archive.WriteHeader(&tar.Header{Name: strings.TrimPrefix(base, "/tmp/") + ".json", Mode: 0644, Size: int64(len(encoded)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := archive.Write(encoded); err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "PUT", "http://docker/containers/"+container+"/archive?path=/tmp&noOverwriteDirNonDir=1", &payload)
	if err != nil {
		return errors.New("could not prepare public TLS probe input")
	}
	request.Header.Set("Content-Type", "application/x-tar")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Docker did not accept public TLS probe input")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Docker refused public TLS probe input")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	out := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", executable, container + ":" + base}, nil, out); err != nil {
		return errors.New("could not copy the worker's TLS verifier into the managed proxy")
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", container, base, "certificate-probe", base + ".json"}, nil, out); err != nil {
		return errors.New("managed proxy did not confirm its selected certificate identities over verified TLS")
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", container, "rm", "-f", base, base + ".json"}, nil, io.Discard); err != nil {
		return errors.New("public listener TLS probe cleanup did not finish")
	}
	return nil
}
