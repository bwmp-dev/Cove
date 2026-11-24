package worker

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestBuildRecipesRequireExplicitEngineAndContainedPaths(t *testing.T) {
	valid := BuildRecipe{Engine: "static", Service: "app", Directory: ".", Output: "dist"}
	if err := ValidateBuildRecipe(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*BuildRecipe){
		func(r *BuildRecipe) { r.Engine = "auto" },
		func(r *BuildRecipe) { r.Service = "" },
		func(r *BuildRecipe) { r.Directory = "../private" },
		func(r *BuildRecipe) { r.Directory = "/app" },
		func(r *BuildRecipe) { r.Output = "dist/../private" },
		func(r *BuildRecipe) { r.Output = "C:\\private" },
		func(r *BuildRecipe) { r.BuildCommand = "echo unexpected" },
		func(r *BuildRecipe) { r.StartCommand = "echo unexpected" },
	} {
		r := valid
		change(&r)
		if ValidateBuildRecipe(r) == nil {
			t.Fatal("accepted unsafe recipe", r)
		}
	}
	if err := ValidateBuildRecipe(BuildRecipe{Engine: "compose"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBuildRecipe(BuildRecipe{Engine: "compose", Service: "app"}); err == nil {
		t.Fatal("hidden recipe fields kept for Compose")
	}
	if err := ValidateBuildRecipe(BuildRecipe{Engine: "railpack", Service: "app", Directory: "packages/api", BuildCommand: "echo inside builder", StartCommand: "node index.js"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBuildRecipe(BuildRecipe{Engine: "railpack", Service: "app", Directory: ".", BuildCommand: strings.Repeat("x", 8193)}); err == nil {
		t.Fatal("unbounded command accepted")
	}
}

func TestBuildRecipeDoesNotAdoptRepositoryComposeOrAnotherService(t *testing.T) {
	compose := "services:\n  app:\n    image: busybox:1.37\n"
	recipe := BuildRecipe{Engine: "static", Service: "app", Directory: ".", Output: "public"}
	if err := ValidateRecipeSource(recipe, &GitSource{InlineCompose: true}, compose, "service"); err != nil {
		t.Fatal(err)
	}
	for _, source := range []*GitSource{nil, {ComposePath: "compose.yaml"}} {
		if err := ValidateRecipeSource(recipe, source, compose, "service"); err == nil {
			t.Fatal("silently replaced repository Compose")
		}
	}
	if err := ValidateRecipeSource(recipe, &GitSource{InlineCompose: true}, compose, "job"); err == nil {
		t.Fatal("static server selected for job")
	}
	recipe.Service = "missing"
	if err := ValidateRecipeSource(recipe, &GitSource{InlineCompose: true}, compose, "service"); err == nil {
		t.Fatal("missing service replaced")
	}
}

func TestStaticRuntimeForcesItsOwnNonRootReadonlyServer(t *testing.T) {
	compose := "services:\n  app:\n    image: fixture/static@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    user: root\n    read_only: false\n    command: echo should-not-run\n    environment:\n      APP_NAME: ${APP_NAME}\n  db:\n    image: fixture/database\n"
	rendered, err := StaticRuntimeCompose(compose, BuildRecipe{Engine: "static", Service: "app"})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			User        string
			ReadOnly    bool `yaml:"read_only"`
			Privileged  bool
			CapDrop     []string `yaml:"cap_drop"`
			SecurityOpt []string `yaml:"security_opt"`
			Entrypoint  []string
			Command     []string
			Environment map[string]string
			Image       string
		}
	}
	if err = yaml.Unmarshal([]byte(rendered), &document); err != nil {
		t.Fatal(err)
	}
	app, db := document.Services["app"], document.Services["db"]
	if app.User != "65534:65534" || !app.ReadOnly || app.Privileged || len(app.CapDrop) != 1 || app.CapDrop[0] != "ALL" || len(app.SecurityOpt) != 1 || app.SecurityOpt[0] != "no-new-privileges:true" || len(app.Entrypoint) != 2 || app.Entrypoint[0] != "/cove-static" || app.Entrypoint[1] != "static-site" || len(app.Command) != 0 || app.Environment["APP_NAME"] != "${APP_NAME}" || db.Image != "fixture/database" || db.ReadOnly {
		t.Fatal(document)
	}
	for _, field := range []string{"volumes: ['/private:/srv']", "configs: [private]", "secrets: [private]", "devices: [/dev/kvm]", "cap_add: [SYS_ADMIN]", "privileged: true"} {
		if _, err := StaticRuntimeCompose("services:\n  app:\n    image: fixture/static\n    "+field+"\n", BuildRecipe{Engine: "static", Service: "app"}); err == nil {
			t.Fatal("unsafe static runtime accepted", field)
		}
	}
}
