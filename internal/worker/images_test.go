package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestImageDigestCheckAndPlatformComparison(t *testing.T) {
	current := "sha256:" + strings.Repeat("a", 64)
	old := "sha256:" + strings.Repeat("b", 64)
	latest := "sha256:" + strings.Repeat("c", 64)
	remoteConfig := current
	e, _ := NewEngine(t.TempDir())
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		switch {
		case args[1] == "image":
			return json.NewEncoder(out).Encode(imageMetadata{ID: current, Digests: []string{"docker.io/library/app@" + old}, OS: "linux", Architecture: "amd64", Labels: map[string]string{"org.opencontainers.image.source": "https://github.com/example/app.git", "org.opencontainers.image.version": "v1.2.3"}})
		case strings.Contains(strings.Join(args, " "), "--raw"):
			return ioWrite(out, `{"config":{"digest":"`+remoteConfig+`"}}`)
		default:
			return ioWrite(out, `{"digest":"`+latest+`","manifests":[{"digest":"`+latest+`","platform":{"os":"linux","architecture":"amd64"}}]}`)
		}
	}
	status := ImageStatus{Service: "app", Reference: "app:latest", ImageID: current}
	got := e.checkImage(context.Background(), status, nil)
	if got.Status != "current" || got.RegistryDigest != latest || got.ReleaseNotes != "https://github.com/example/app/releases/tag/v1.2.3" {
		t.Fatal(got)
	}
	remoteConfig = old
	if got = e.checkImage(context.Background(), status, nil); got.Status != "update_available" {
		t.Fatal(got)
	}
	current = latest
	status.ImageID = current
	if got = e.checkImage(context.Background(), status, nil); got.Status != "current" {
		t.Fatal("containerd manifest identity was not recognized", got)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		return errors.New("private-registry-token")
	}
	if got = e.checkImage(context.Background(), status, nil); got.Status != "unknown" || strings.Contains(got.Message, "private-registry-token") {
		t.Fatal(got)
	}
}

func ioWrite(out io.Writer, value string) error { _, err := io.WriteString(out, value); return err }

func TestImageReferenceAndOperationValidation(t *testing.T) {
	for ref, want := range map[string]string{"busybox:1.37": "docker.io/library/busybox", "docker.io/busybox:latest": "docker.io/library/busybox", "acme/app:1": "docker.io/acme/app", "localhost:5000/acme/app:latest": "localhost:5000/acme/app", "ghcr.io/acme/app@sha256:abc": "ghcr.io/acme/app"} {
		if got := imageRepository(ref); got != want {
			t.Fatal(ref, got, want)
		}
	}
	for _, request := range []Request{
		{ID: "op", Workload: "app", Action: "stop", PullLatest: true},
		{ID: "op", Workload: "app", Action: "image_check", Kind: "job"},
		{ID: "op", Workload: "app", Action: "deploy", Compose: "services: {}", PullLatest: true, ImagePins: map[string]string{"app": "sha256:" + strings.Repeat("a", 64)}},
	} {
		if ValidateRequest(request) == nil {
			t.Fatal("invalid image operation accepted", request)
		}
	}
	if err := ValidateRequest(Request{ID: "op", Workload: "app", Action: "image_check", Kind: "service"}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"http://github.com/example/app", "https://user:pass@github.com/example/app", "https://example.com/app", "https://github.com/example/app?token=x"} {
		if imageReleaseNotes(map[string]string{"org.opencontainers.image.source": source, "org.opencontainers.image.version": "v1.2.3"}) != "" {
			t.Fatal(source)
		}
	}
}
