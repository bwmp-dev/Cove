package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPreviewRegistryImagesRequireExplicitHostAndDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, image := range []string{"registry.example.test/team/app@" + digest, "registry.example.test:5000/team/app@" + digest, "registry:5000/team/app@" + digest, "localhost/team/app@" + digest, "[::1]:5000/team/app@" + digest, "REGISTRY/team/app@" + digest} {
		server, _, _ := strings.Cut(image, "/")
		if err := ValidatePreviewRegistryImages(map[string]string{"web": image}, server); err != nil {
			t.Fatal(err)
		}
	}
	for _, image := range []string{digest, "team/app@" + digest, "registry.example.test/team/app:latest", "registry.example.test/team/app:stable@" + digest, "other.example.test/team/app@" + digest, "registry.example.test/team/../app@" + digest} {
		if err := ValidatePreviewRegistryImages(map[string]string{"web": image}, "registry.example.test"); err == nil {
			t.Fatalf("accepted %s", image)
		}
	}
	for _, server := range []string{"registry", "team"} {
		if err := ValidatePreviewRegistryImages(map[string]string{"web": server + "/team/app@" + digest}, server); err == nil {
			t.Fatalf("accepted ambiguous Docker Hub namespace %s", server)
		}
	}
}

func TestPreviewRegistryImagesPullOnlyPreserveRecipeAndCompleteMap(t *testing.T) {
	digest := "registry.example.test/team/app@sha256:" + strings.Repeat("b", 64)
	r := Request{ID: "operation", Workload: previewWorkload, Action: "deploy", Kind: "service", ImagePins: map[string]string{"web": digest}, Registry: &Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture"}, EnvironmentPreview: &EnvironmentPreview{Preview: "preview1", ImageRegistry: "registry.example.test"}}
	e := &Engine{Root: t.TempDir()}
	e.Run = func(_ context.Context, _ string, _ []string, _ []string, out io.Writer) error {
		_, err := fmt.Fprint(out, `{"services":{"web":{"image":"old:tag","build":{"context":"."},"environment":{"MODE":"preview"},"volumes":["preview-owned:/data"]}}}`)
		return err
	}
	file, err := e.preparePromotion(context.Background(), r, e.Root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if json.Unmarshal(raw, &config) != nil {
		t.Fatal("invalid generated Compose")
	}
	service := config["services"].(map[string]any)["web"].(map[string]any)
	if service["image"] != digest || service["pull_policy"] != "always" || service["build"] != nil || service["environment"].(map[string]any)["MODE"] != "preview" {
		t.Fatal(service)
	}
	r.ImagePins["extra"] = digest
	if _, err = e.preparePromotion(context.Background(), r, e.Root, nil, nil); err == nil {
		t.Fatal("unknown service accepted")
	}
	delete(r.ImagePins, "web")
	if _, err = e.preparePromotion(context.Background(), r, e.Root, nil, nil); err == nil {
		t.Fatal("partial service map accepted")
	}
	r.Registry = nil
	called := false
	e.Run = func(context.Context, string, []string, []string, io.Writer) error { called = true; return nil }
	if err = e.perform(context.Background(), r); err == nil || called {
		t.Fatal("missing CI credentials reached Docker", err, called)
	}
}

func TestPreviewRegistryImagesCannotCheckoutOrRunJobs(t *testing.T) {
	r := Request{ID: "operation", Workload: previewWorkload, Action: "deploy", Kind: "service", ImagePins: map[string]string{"web": "registry.example.test/app@sha256:" + strings.Repeat("a", 64)}, EnvironmentPreview: &EnvironmentPreview{Preview: "preview1", ImageRegistry: "registry.example.test"}}
	if err := validateEnvironmentPreview(r); err != nil {
		t.Fatal(err)
	}
	r.Source = &GitSource{Repository: "https://git.example.test/app.git"}
	if err := validateEnvironmentPreview(r); err == nil {
		t.Fatal("Git checkout accepted")
	}
	r.Source = nil
	r.Kind = "job"
	if err := validateEnvironmentPreview(r); err == nil {
		t.Fatal("CI job accepted")
	}
}
