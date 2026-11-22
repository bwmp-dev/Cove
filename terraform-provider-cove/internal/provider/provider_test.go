package provider

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"cove/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

type fixtureAPI struct {
	mu       sync.Mutex
	objects  map[string]map[string]any
	values   map[string]string
	revision int
	next     int
}

func (f *fixtureAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-Cove-Team") != "fixture-team" {
		http.Error(w, "unauthorized", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	body := map[string]any{}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			http.Error(w, "invalid JSON", 400)
			return
		}
	}
	if len(parts) == 4 && parts[0] == "workloads" && parts[2] == "variables" {
		key := parts[1] + "/" + parts[3]
		switch r.Method {
		case "PUT":
			f.values[key] = body["value"].(string)
			f.revision++
		case "DELETE":
			delete(f.values, key)
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		case "GET":
			if _, ok := f.values[key]; !ok {
				http.NotFound(w, r)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"name": parts[3], "version": fmt.Sprint(f.revision)})
		return
	}
	if len(parts) == 3 && parts[0] == "workloads" && parts[2] == "routes" {
		object := f.objects["/workloads/"+parts[1]]
		if object == nil {
			http.NotFound(w, r)
			return
		}
		object["routes"] = body["routes"]
		json.NewEncoder(w).Encode(map[string]string{"state": "saved"})
		return
	}
	if r.Method == "POST" {
		f.next++
		id := fmt.Sprintf("fixture-%d", f.next)
		kind := parts[0]
		if len(parts) == 3 && parts[2] == "environments" {
			kind = "environments"
			body["project_id"] = parts[1]
		}
		body["id"] = id
		if kind == "workloads" {
			body["routes"] = []any{}
		}
		f.objects["/"+kind+"/"+id] = body
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"id": id})
		return
	}
	object := f.objects[path]
	if object == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(object)
	case "PUT":
		for key, value := range body {
			object[key] = value
		}
		json.NewEncoder(w).Encode(map[string]any{"id": object["id"]})
	case "DELETE":
		delete(f.objects, path)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		http.Error(w, "unsupported", 405)
	}
}
func newFixture(t *testing.T) (*fixtureAPI, *httptest.Server, *client.Client) {
	t.Helper()
	fixture := &fixtureAPI{objects: map[string]map[string]any{}, values: map[string]string{}}
	server := httptest.NewTLSServer(fixture)
	t.Cleanup(server.Close)
	api, err := client.New(server.URL, "fixture-token", "fixture-team")
	if err != nil {
		t.Fatal(err)
	}
	api.HTTP.Transport = server.Client().Transport
	return fixture, server, api
}
func TestProtocolSchemasAndFrameworkLifecycle(t *testing.T) {
	server := providerserver.NewProtocol6(New())()
	schema, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || len(schema.Diagnostics) > 0 || len(schema.ResourceSchemas) != 5 {
		t.Fatal("invalid provider schema", err, schema.Diagnostics)
	}
	_, _, api := newFixture(t)
	ctx := context.Background()
	ids := map[string]string{}
	for _, kind := range []string{"project", "environment", "workload", "variable", "routes"} {
		t.Run(kind, func(t *testing.T) {
			r := &managedResource{kind: kind, api: api}
			var definition resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &definition)
			data := map[string]types.String{}
			for name := range r.attributes() {
				data[name] = types.StringNull()
			}
			data["id"] = types.StringUnknown()
			switch kind {
			case "project":
				data["name"] = types.StringValue("Project")
			case "environment":
				data["name"] = types.StringValue("Production")
				data["project_id"] = types.StringValue(ids["project"])
			case "workload":
				data["name"] = types.StringValue("Application")
				data["node_id"] = types.StringValue("fixture-node")
				data["project_id"] = types.StringValue(ids["project"])
				data["environment_id"] = types.StringValue(ids["environment"])
				data["compose"] = types.StringValue("services: {}\n")
				data["source_json"] = types.StringValue("")
				data["kind"] = types.StringValue("service")
				data["job_service"] = types.StringValue("")
			case "variable":
				data["workload_id"] = types.StringValue(ids["workload"])
				data["name"] = types.StringValue("VALUE")
				data["value"] = types.StringValue("fixture-secret")
				data["revision"] = types.StringUnknown()
			case "routes":
				data["workload_id"] = types.StringValue(ids["workload"])
				data["routes_json"] = types.StringValue(`[{"domain":"app.example.com","service":"web","port":80}]`)
			}
			state := tfsdk.State{Schema: definition.Schema}
			attributeTypes := map[string]attr.Type{}
			attributeValues := map[string]attr.Value{}
			for name, value := range data {
				attributeTypes[name] = types.StringType
				attributeValues[name] = value
			}
			if diagnostics := state.Set(ctx, types.ObjectValueMust(attributeTypes, attributeValues)); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			var created resource.CreateResponse
			r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Raw: state.Raw, Schema: state.Schema}}, &created)
			if created.Diagnostics.HasError() {
				t.Fatal(created.Diagnostics)
			}
			var id types.String
			created.State.GetAttribute(ctx, path.Root("id"), &id)
			if id.ValueString() == "" {
				t.Fatal("resource ID missing")
			}
			ids[kind] = id.ValueString()
			var read resource.ReadResponse
			r.Read(ctx, resource.ReadRequest{State: created.State}, &read)
			if read.Diagnostics.HasError() {
				t.Fatal(read.Diagnostics)
			}
			removed := resource.DeleteResponse{State: read.State}
			if kind == "variable" || kind == "routes" {
				r.Delete(ctx, resource.DeleteRequest{State: read.State}, &removed)
				if removed.Diagnostics.HasError() {
					t.Fatal(removed.Diagnostics)
				}
			}
		})
	}
}
func TestNativeTerraformOrTofuLifecycle(t *testing.T) {
	binary, providerBinary := os.Getenv("COVE_TERRAFORM_BINARY"), os.Getenv("COVE_PROVIDER_BINARY")
	if binary == "" || providerBinary == "" {
		t.Skip("set COVE_TERRAFORM_BINARY and COVE_PROVIDER_BINARY for isolated native CLI acceptance")
	}
	fixture, server, _ := newFixture(t)
	directory := t.TempDir()
	mirror := filepath.Join(directory, "plugins")
	platform := runtime.GOOS + "_" + runtime.GOARCH
	pluginDirectory := filepath.Join(mirror, "registry.terraform.io", "bwmp-dev", "cove", "0.1.0", platform)
	if err := os.MkdirAll(pluginDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	name := "terraform-provider-cove_v0.1.0"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	raw, err := os.ReadFile(providerBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(pluginDirectory, name), raw, 0700); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(directory, "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(directory, "cli.tfrc")
	config := fmt.Sprintf("provider_installation {\n filesystem_mirror {\n path = %q\n include = [\"registry.terraform.io/bwmp-dev/cove\"]\n }\n}\n", filepath.ToSlash(mirror))
	if err = os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := `terraform {
 required_providers {
  cove = {
   source = "registry.terraform.io/bwmp-dev/cove"
   version = "0.1.0"
  }
 }
}
provider "cove" {}
resource "cove_project" "project" {
 name = "Fixture project"
}
resource "cove_environment" "environment" {
 name = "Fixture environment"
 project_id = cove_project.project.id
}
resource "cove_workload" "workload" {
 name = "Fixture application"
 node_id = "fixture-node"
 project_id = cove_project.project.id
 environment_id = cove_environment.environment.id
 compose = "services:\n  web:\n    image: busybox:1.37\n"
}
resource "cove_variable" "variable" {
 workload_id = cove_workload.workload.id
 name = "VALUE"
 value = "fixture-secret"
}
resource "cove_routes" "routes" {
 workload_id = cove_workload.workload.id
 routes_json = jsonencode([{domain="app.example.com",service="web",port=80}])
}
`
	if err = os.WriteFile(filepath.Join(directory, "main.tf"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(want int, args ...string) {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Dir = directory
		command.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+configFile, "CHECKPOINT_DISABLE=1", "TF_IN_AUTOMATION=1", "COVE_ENDPOINT="+server.URL, "COVE_API_TOKEN=fixture-token", "COVE_TEAM=fixture-team", "COVE_CA_FILE="+ca)
		output, err := command.CombinedOutput()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != want {
			t.Fatalf("native CLI %v exited%d want%d: %s", args, code, want, output)
		}
	}
	run(0, "init", "-input=false", "-no-color")
	run(0, "validate", "-no-color")
	run(0, "apply", "-auto-approve", "-input=false", "-no-color")
	run(0, "plan", "-detailed-exitcode", "-input=false", "-no-color")
	changed := strings.NewReplacer("Fixture project", "Renamed project", "Fixture environment", "Renamed environment", "Fixture application", "Renamed application", "fixture-secret", "rotated-fixture-secret", "port=80", "port=8080").Replace(configuration)
	if err = os.WriteFile(filepath.Join(directory, "main.tf"), []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	run(0, "apply", "-auto-approve", "-input=false", "-no-color")
	run(0, "plan", "-detailed-exitcode", "-input=false", "-no-color")
	fixture.mu.Lock()
	fixture.revision++
	fixture.mu.Unlock()
	run(2, "plan", "-detailed-exitcode", "-input=false", "-no-color")
	run(0, "apply", "-auto-approve", "-input=false", "-no-color")
	stateRaw, err := os.ReadFile(filepath.Join(directory, "terraform.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Resources []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes map[string]json.RawMessage `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if json.Unmarshal(stateRaw, &state) != nil {
		t.Fatal("could not read native fixture state")
	}
	for _, item := range state.Resources {
		var id string
		if len(item.Instances) != 1 || json.Unmarshal(item.Instances[0].Attributes["id"], &id) != nil || id == "" {
			t.Fatal("native resource ID missing")
		}
		address := item.Type + "." + item.Name
		run(0, "state", "rm", address)
		run(0, "import", "-input=false", "-no-color", address, id)
	}
	run(0, "apply", "-auto-approve", "-input=false", "-no-color")
	run(0, "plan", "-detailed-exitcode", "-input=false", "-no-color")
	run(0, "destroy", "-auto-approve", "-input=false", "-no-color")
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.objects) != 0 || len(fixture.values) != 0 {
		t.Fatal("native destroy left managed fixture objects")
	}
}

var _ provider.Provider = New()
