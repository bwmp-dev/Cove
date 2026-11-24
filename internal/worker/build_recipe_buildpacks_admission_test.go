package worker

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestBuildpacksDerivedMemoryAndRecordedExecutionLimit(t *testing.T) {
	for _, item := range []struct{ available, limit uint64 }{{4 << 30, 2 << 30}, {(5 << 30) + 123, 3 << 30}, {8 << 30, 4 << 30}} {
		admission, err := DeriveBuildpacksAdmission(item.available)
		if err != nil || admission.PhaseMemoryBytes != item.limit || admission.Validate() != nil {
			t.Fatal(admission, err)
		}
	}
	if _, err := DeriveBuildpacksAdmission((4 << 30) - 1); err == nil {
		t.Fatal("insufficient available memory was accepted")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "cnb-memory", Workload: "cnb-workload", Action: "build", Kind: "service", Compose: "services:\n  app:\n    build: .\n", Source: &GitSource{Repository: "https://git.example.test/app.git", Branch: "main", InlineCompose: true}, Build: &BuildOptions{Repository: "fixture/app"}, Registry: &Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture-only"}, BuildRecipe: &BuildRecipe{Engine: "buildpacks", Service: "app", Directory: "."}}
	admission, _ := DeriveBuildpacksAdmission(6 << 30)
	r.BuildpacksAdmission = &admission
	if _, err := e.submitBuildpacksLocked(r, &admission); err != nil {
		t.Fatal(err)
	}
	before, _ := e.read(r.ID)
	if err := e.admitBuildpacksPhases(r, 4<<30); err != nil {
		t.Fatal(err)
	}
	after, _ := e.read(r.ID)
	if !reflect.DeepEqual(before.Request, after.Request) || before.Digest != after.Digest || after.Operation.BuildRecipe.PhaseMemoryBytes != 2<<30 || after.Operation.BuildRecipe.AdmissionAvailableMemoryBytes != 4<<30 {
		t.Fatal("execution admission changed frozen inputs or lost the actual memory limit")
	}
	if err := e.admitBuildpacksPhases(r, 8<<30); err != nil {
		t.Fatal(err)
	}
	actual, err := e.recordedBuildpacksAdmission(r)
	if err != nil || actual.PhaseMemoryBytes != 2<<30 {
		t.Fatal("execution limit grew beyond its recorded allocation", actual, err)
	}
	if _, err := e.Submit(r); err != nil {
		t.Fatal("uncertain submission retried resource admission instead of reconciling its existing journal", err)
	}
	changed := r
	changed.BuildpacksAdmission = &BuildpacksAdmission{AvailableMemoryBytes: 4 << 30, PhaseMemoryBytes: 2 << 30}
	if _, err := e.Submit(changed); err == nil {
		t.Fatal("an existing admission accepted different immutable inputs")
	}
	e.Run = func(_ context.Context, _ string, arguments, _ []string, output io.Writer) error {
		if len(arguments) > 1 && arguments[1] == "start" {
			return errors.New("fixture cgroup exit 137")
		}
		if len(arguments) > 2 && arguments[1] == "container" && arguments[2] == "inspect" {
			_, err := io.WriteString(output, "true")
			return err
		}
		return nil
	}
	var memoryFailure *buildpacksMemoryError
	if err := e.runBuildpacksPhase(context.Background(), r, "builder", "none", "", io.Discard); !errors.As(err, &memoryFailure) || memoryFailure.limit != 2<<30 {
		t.Fatal("OOM did not retain the actual recorded limit", err)
	}
}

func TestBuildpacksFrozenMetadataAndSecretBoundary(t *testing.T) {
	before := cnbSnapshot{Metadata: map[string]string{"analyzed": "a", "group": "b", "plan": "c"}, Run: map[string]string{"index.json": "pinned"}}
	if err := verifyBuildpacksSnapshot(before, before); err != nil {
		t.Fatal(err)
	}
	for _, after := range []cnbSnapshot{{Metadata: map[string]string{"analyzed": "changed"}, Run: before.Run}, {Metadata: before.Metadata, Run: map[string]string{"index.json": "changed"}}} {
		if err := verifyBuildpacksSnapshot(before, after); err == nil {
			t.Fatal("changed frozen input reached export")
		}
	}
	for _, name := range []string{"CNB_RUN_IMAGE", "DOCKER_HOST", "HTTP_PROXY", "LD_PRELOAD"} {
		if buildpacksSecretNameAllowed(name) {
			t.Fatal("reserved build secret", name)
		}
	}
	message := (&buildpacksMemoryError{limit: 2 << 30}).Error()
	if !strings.Contains(message, "2147483648-byte") || !strings.Contains(message, "minus 2 GiB") {
		t.Fatal(message)
	}
}

func TestBuildpacksRefusesAdditionalUnboundedBuildServices(t *testing.T) {
	services := map[string]any{"app": map[string]any{"build": "."}, "database": map[string]any{"image": "postgres:17-alpine"}}
	if err := validateBuildpacksServices(services, "app"); err != nil {
		t.Fatal("image-only services were changed", err)
	}
	services["database"] = map[string]any{"build": map[string]any{"context": "."}}
	if err := validateBuildpacksServices(services, "app"); err == nil {
		t.Fatal("an additional ordinary BuildKit build escaped the CNB phase resource budget")
	}
	if err := ValidateRecipeSource(BuildRecipe{Engine: "buildpacks", Service: "app", Directory: "."}, &GitSource{InlineCompose: true}, "services:\n  app:\n    image: fixture/app\n  other:\n    build: .\n", "service"); err == nil {
		t.Fatal("mixed build admission accepted")
	}
}
