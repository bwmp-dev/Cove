package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recipeRequest() Request {
	return Request{ID: "recipe-build", Workload: "recipe-app", Action: "build", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Source: &GitSource{Repository: "https://git.example.test/fixture.git", Branch: "main", InlineCompose: true}, Build: &BuildOptions{Repository: "fixture/app"}, Registry: &Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture-placeholder"}, BuildRecipe: &BuildRecipe{Engine: "railpack", Service: "app", Directory: "."}}
}

func TestBuildRecipeFinalizationRetainsRecordedRuntime(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := recipeRequest()
	r.BuildRecipe = &BuildRecipe{Engine: "static", Service: "app", Directory: ".", Output: "public"}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	runtimeDigest := "sha256:" + strings.Repeat("a", 64)
	e.Run = func(_ context.Context, _ string, _ []string, _ []string, _ io.Writer) error {
		current, err := e.read(r.ID)
		if err != nil {
			return err
		}
		current.Operation.BuildRecipe.StaticRuntime = runtimeDigest
		if err := writeJSON(e.path(r.ID), current); err != nil {
			return err
		}
		return errors.New("fixture build failure after recording runtime")
	}
	j, err := e.read(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.execute(context.Background(), j)
	op, err := e.Status(r.ID)
	if err != nil || op.State != "failed" || op.BuildRecipe == nil || op.BuildRecipe.StaticRuntime != runtimeDigest {
		t.Fatal("finalization lost recorded build recipe", op, err)
	}
}

func TestBuildRecipeDowngradeAndSubmissionCannotFallBack(t *testing.T) {
	for _, noCache := range []bool{false, true} {
		e, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		r := recipeRequest()
		r.Build.NoCache = noCache
		for _, method := range []string{"submit", "submit_build_v2"} {
			if result := e.RPC(context.Background(), RPCRequest{Method: method, Request: &r}); result.Error == "" {
				t.Fatal("ordinary submission accepted a recipe")
			}
		}
		if result := e.RPC(context.Background(), RPCRequest{Method: "submit_build_recipe", Request: &r}); result.Error != "" {
			t.Fatal(result.Error)
		}
		data, err := os.ReadFile(e.path(r.ID))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), protectedBuildRecipe) {
			t.Fatal("recipe protection missing")
		}
		var legacy struct {
			Request struct {
				Build struct {
					NoCache bool `json:"no_cache"`
				} `json:"build"`
			} `json:"request"`
		}
		if json.Unmarshal(data, &legacy) == nil {
			t.Fatal("older worker can silently drop the recipe")
		}
		restarted, err := NewEngine(e.Root)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := restarted.read(r.ID)
		if err != nil || journal.Request.Build.NoCache != noCache || journal.Request.BuildRecipe == nil || *journal.Request.BuildRecipe != *r.BuildRecipe || journal.Operation.BuildRecipe.Engine != "railpack" {
			t.Fatal("recipe/history lost after restart", err)
		}
	}
}

func TestBuildRecipeCleanupKeepsOwnershipUntilRecovery(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := recipeRequest()
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(e.Root, "workloads", r.Workload)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "pending-build.json")
	if err := writeJSON(marker, pendingBuild{Operation: r.ID, Owner: owner, Services: []string{"app"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.recipeInputs(r), 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(e.recipeInputs(r), "private-input")
	if err := os.WriteFile(input, []byte("fixture-private"), 0600); err != nil {
		t.Fatal(err)
	}
	planner, relay := recipeResourceName(r, "planner"), recipeResourceName(r, "egress")
	resources := map[string]bool{planner: true, relay: true}
	foreign := true
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if args[1] == "image" {
			return nil
		}
		switch args[2] {
		case "ls":
			for name := range resources {
				for _, argument := range args {
					if argument == "name=^"+name+"$" {
						_, err := io.WriteString(out, name)
						return err
					}
				}
			}
			return nil
		case "inspect":
			operation := r.ID
			if foreign && args[len(args)-1] == relay {
				operation = "different-operation"
			}
			return json.NewEncoder(out).Encode(map[string]string{"cove.managed": "build-recipe", "cove.workload": r.Workload, "cove.operation": operation})
		case "rm":
			name := args[len(args)-1]
			if foreign && name == relay {
				return errors.New("foreign relay was removed")
			}
			delete(resources, name)
			return nil
		default:
			return errors.New("unexpected cleanup command")
		}
	}
	if err := e.recoverBuild(context.Background(), r.Workload); err == nil || !strings.Contains(err.Error(), "different ownership") {
		t.Fatal("foreign cleanup was not refused", err)
	}
	if !resources[relay] {
		t.Fatal("foreign relay removed")
	}
	for _, path := range []string{marker, input} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("cleanup ownership/input lost before completion", err)
		}
	}
	foreign = false
	if err := e.recoverBuild(context.Background(), r.Workload); err != nil {
		t.Fatal(err)
	}
	if len(resources) != 0 {
		t.Fatal("owned resources remain")
	}
	for _, path := range []string{marker, e.recipeInputs(r)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("completed cleanup retained private inputs", err)
		}
	}
}
