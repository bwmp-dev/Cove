package worker

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestImageLifecycleDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_IMAGES_DOCKER") != "1" {
		t.Skip("set COVE_TEST_IMAGES_DOCKER=1 for a disposable localhost registry")
	}
	if runtime.GOOS != "linux" {
		t.Skip("run this acceptance in a Linux fixture where the worker and Docker daemon share loopback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, err := e.output(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(e.Root, "docker-test")
	if err = os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		var diagnostic bytes.Buffer
		err := RunCommand(ctx, dir, args, append(append([]string{}, env...), "DOCKER_CONFIG="+config, "DOCKER_HOST="+host), io.MultiWriter(out, &diagnostic))
		if err != nil {
			t.Logf("fixture command %q failed: %s", args, diagnostic.String())
		}
		return err
	}
	id := "images-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	registry := "cove-" + id + "-registry"
	reference := ""
	candidateReference := ""
	ownedImage := ""
	composePath := filepath.Join(e.Root, "test.compose.yaml")
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		commands := [][]string{{"docker", "rm", "-f", "-v", registry}}
		if _, err := os.Stat(composePath); err == nil {
			commands = append([][]string{{"docker", "compose", "--project-name", "cove-" + id, "--file", composePath, "down", "--remove-orphans"}}, commands...)
		}
		for _, args := range commands {
			if err := e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
		tags, err := e.output(cleanup, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-release/"+id+":*")
		if err != nil {
			t.Error(err)
		}
		for _, tag := range append(strings.Fields(tags), reference, candidateReference) {
			if tag != "" {
				if err := e.Run(cleanup, e.Root, []string{"docker", "image", "rm", tag}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			}
		}
	})
	if err = e.Run(ctx, e.Root, []string{"docker", "run", "--detach", "--name", registry, "--label", "cove.test=true", "-p", "127.0.0.1::5000", "registry:2.8.3"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	published, err := e.output(ctx, "docker", "port", registry, "5000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	port := published[strings.LastIndex(published, ":")+1:]
	reference = "localhost:" + port + "/cove-test/app:latest"
	candidateReference = "localhost:" + port + "/cove-test/app:candidate"
	client := &http.Client{Timeout: 5 * time.Second}
	for deadline := time.Now().Add(15 * time.Second); ; {
		response, err := client.Get("http://127.0.0.1:" + port + "/v2/")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture registry not ready", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, args := range [][]string{{"docker", "tag", "busybox:1.37", reference}, {"docker", "push", reference}, {"docker", "pull", reference}} {
		var output bytes.Buffer
		if err = e.Run(ctx, e.Root, args, nil, &output); err != nil {
			t.Fatal(args, err, output.String())
		}
	}
	compose := "services:\n  app:\n    image: " + reference + "\n    command: [sh, -c, 'sleep 300']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n"
	if err = os.WriteFile(composePath, []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(request Request) Operation {
		t.Helper()
		request.Workload = id
		request.Kind = "service"
		if _, err := e.Submit(request); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err := e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		operation, err := e.Status(request.ID)
		if err != nil || operation.State != "succeeded" {
			logs, _ := e.Logs(request.ID)
			t.Fatal(operation, err, logs)
		}
		return operation
	}
	run(Request{ID: "initial", Action: "deploy", Compose: compose})
	first := run(Request{ID: "check-current", Action: "image_check"})
	if first.ImageCheck == nil || len(first.ImageCheck.Images) != 1 || first.ImageCheck.Images[0].Status != "current" {
		t.Fatal(first.ImageCheck)
	}
	containers, err := e.Containers(ctx, id)
	if err != nil || len(containers) != 1 {
		t.Fatal(containers, err)
	}
	originalID := first.ImageCheck.Images[0].ImageID
	ownedImage, err = e.output(ctx, "docker", "commit", "--change", "LABEL org.opencontainers.image.version=v2.0.0", containers[0].ID, candidateReference)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "push", candidateReference}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	manifestURL := "http://127.0.0.1:" + port + "/v2/cove-test/app/manifests/"
	get, err := http.NewRequestWithContext(ctx, "GET", manifestURL+"candidate", nil)
	if err != nil {
		t.Fatal(err)
	}
	get.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json")
	response, err := client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	manifest, readErr := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	response.Body.Close()
	if readErr != nil || response.StatusCode != 200 || len(manifest) > 2*1024*1024 {
		t.Fatal("candidate manifest unavailable", response.StatusCode, readErr)
	}
	put, err := http.NewRequestWithContext(ctx, "PUT", manifestURL+"latest", bytes.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	put.Header.Set("Content-Type", response.Header.Get("Content-Type"))
	response, err = client.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatal("could not advance registry tag", response.StatusCode)
	}
	updated := run(Request{ID: "check-update", Action: "image_check"})
	if updated.ImageCheck == nil || updated.ImageCheck.Images[0].Status != "update_available" {
		t.Fatal(updated.ImageCheck)
	}
	before, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", containers[0].ID)
	if err != nil || before != originalID {
		t.Fatal("checking changed the app", before, err)
	}
	run(Request{ID: "refresh", Action: "deploy", Compose: compose, PullLatest: true})
	current, _, err := readCurrent(filepath.Join(e.Root, "workloads", id, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current.ComposePath, "refresh") {
		t.Fatal(current)
	}
	refreshed := run(Request{ID: "check-refreshed", Action: "image_check"})
	if refreshed.ImageCheck.Images[0].ImageID != ownedImage || refreshed.ImageCheck.Images[0].Status != "current" {
		t.Fatal(refreshed.ImageCheck)
	}
	run(Request{ID: "rollback", Action: "rollback", Release: "initial"})
	rolledBack := run(Request{ID: "check-rollback", Action: "image_check"})
	if rolledBack.ImageCheck.Images[0].ImageID != originalID || rolledBack.ImageCheck.Images[0].Status != "pinned" {
		t.Fatal(rolledBack.ImageCheck)
	}
}
