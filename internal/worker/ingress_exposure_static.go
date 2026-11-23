package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func ingressExposureStaticPath(id string) string { return "/data/cove-listener-static-" + id + ".yaml" }

func stageIngressExposureStatic(ctx context.Context, client *http.Client, prepared preparedIngressExposure) error {
	if !identifier.MatchString(prepared.Plan.ChangeID) || len(prepared.Static) == 0 || len(prepared.Static) > 1<<20 {
		return errors.New("private listener static configuration is invalid")
	}
	var config struct{ User string }
	if json.Unmarshal(prepared.Snapshot.Config, &config) != nil || config.User != "" && config.User != "0" && config.User != "0:0" && config.User != "root" {
		return errors.New("managed static-file replacement requires the stock root proxy user")
	}
	path := ingressExposureStaticPath(prepared.Plan.ChangeID)
	var previous bytes.Buffer
	err := copyIngressProxyFile(ctx, client, prepared.Snapshot.ID, path, &previous, 1<<20)
	if err == nil {
		if !bytes.Equal(previous.Bytes(), prepared.Static) {
			return errors.New("private listener static path already contains different data")
		}
		return nil
	}
	if !errors.Is(err, errIngressDockerMissing) {
		return err
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	if err := archive.WriteHeader(&tar.Header{Name: strings.TrimPrefix(path, "/data/"), Mode: 0600, Size: int64(len(prepared.Static)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := archive.Write(prepared.Static); err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "PUT", "http://docker/containers/"+prepared.Snapshot.ID+"/archive?path="+url.QueryEscape("/data")+"&noOverwriteDirNonDir=1", &buffer)
	if err != nil {
		return errors.New("could not prepare private listener static write")
	}
	request.Header.Set("Content-Type", "application/x-tar")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Docker did not confirm the private listener static write")
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return errors.New("Docker refused the private listener static write")
	}
	var confirmed bytes.Buffer
	if err := copyIngressProxyFile(ctx, client, prepared.Snapshot.ID, path, &confirmed, 1<<20); err != nil || !bytes.Equal(confirmed.Bytes(), prepared.Static) {
		return errors.New("private listener static content was not confirmed")
	}
	return nil
}
