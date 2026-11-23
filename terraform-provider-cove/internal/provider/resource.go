package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strings"
	"time"

	"cove/internal/client"
	"cove/internal/worker"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type managedResource struct {
	kind string
	api  *client.Client
}

func (r *managedResource) Metadata(_ context.Context, request resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = request.ProviderTypeName + "_" + r.kind
}
func (r *managedResource) attributes() map[string]schema.Attribute {
	attributes := map[string]schema.Attribute{"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}}}
	required := func(name, description string, replace, sensitive bool) {
		a := schema.StringAttribute{Required: true, Description: description, Sensitive: sensitive}
		if replace {
			a.PlanModifiers = []planmodifier.String{stringplanmodifier.RequiresReplace()}
		}
		attributes[name] = a
	}
	optional := func(name, value, description string, replace, sensitive bool) {
		a := schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(value), Description: description, Sensitive: sensitive}
		if replace {
			a.PlanModifiers = []planmodifier.String{stringplanmodifier.RequiresReplace()}
		}
		attributes[name] = a
	}
	switch r.kind {
	case "project":
		required("name", "Project name.", false, false)
	case "environment":
		required("name", "Environment name.", false, false)
		required("project_id", "Parent project.", true, false)
	case "workload":
		required("name", "Resource name.", false, false)
		required("node_id", "Ready, team-assigned worker.", true, false)
		required("project_id", "Parent project.", true, false)
		optional("environment_id", "", "Optional environment.", true, false)
		optional("compose", "", "Saved Compose; required unless source_json selects Git.", false, true)
		optional("source_json", "", "GitSource JSON. Sensitive credentials remain in Terraform state; deploy keys and provider connections are managed separately.", false, true)
		optional("kind", "service", "service or job.", false, false)
		optional("job_service", "", "One-shot service for jobs.", false, false)
	case "variable":
		required("workload_id", "Resource containing this variable.", true, false)
		required("name", "Variable name.", true, false)
		required("value", "Saved literal/reference. Protect state storage; sensitive values still exist there.", false, true)
		attributes["revision"] = schema.StringAttribute{Computed: true, Description: "Opaque server revision; plaintext is never returned."}
	case "routes":
		required("workload_id", "Resource whose complete public route set is managed.", true, false)
		required("routes_json", "JSON array of routes and policy fields. One resource owns the entire route set. Forward-auth destinations are kept only in sensitive Terraform state.", false, true)
	}
	return attributes
}
func (r *managedResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	response.Schema = schema.Schema{Description: "Manage Cove " + r.kind + " configuration. Updates do not deploy. Workload deletion requires it to be stopped first.", Attributes: r.attributes()}
}
func (r *managedResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
	if request.ProviderData == nil {
		return
	}
	api, ok := request.ProviderData.(*client.Client)
	if !ok {
		response.Diagnostics.AddError("Invalid provider data", "Cove API client was not configured.")
		return
	}
	r.api = api
}

type attributeReader interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}

func (r *managedResource) read(ctx context.Context, source attributeReader, diagnostics *diag.Diagnostics) map[string]types.String {
	data := map[string]types.String{}
	for name := range r.attributes() {
		var value types.String
		diagnostics.Append(source.GetAttribute(ctx, path.Root(name), &value)...)
		data[name] = value
	}
	return data
}
func setState(ctx context.Context, state *tfsdk.State, data map[string]types.String, diagnostics *diag.Diagnostics) {
	for name, value := range data {
		diagnostics.Append(state.SetAttribute(ctx, path.Root(name), value)...)
	}
}
func escape(value string) string { return url.PathEscape(value) }
func (r *managedResource) endpoint(data map[string]types.String) string {
	switch r.kind {
	case "project":
		return "/projects/" + escape(data["id"].ValueString())
	case "environment":
		return "/environments/" + escape(data["id"].ValueString())
	case "workload":
		return "/workloads/" + escape(data["id"].ValueString())
	case "variable":
		return "/workloads/" + escape(data["workload_id"].ValueString()) + "/variables/" + escape(data["name"].ValueString())
	case "routes":
		return "/workloads/" + escape(data["workload_id"].ValueString()) + "/routes"
	}
	return ""
}
func parseRoutes(text string) ([]worker.Route, error) {
	var routes []worker.Route
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&routes) != nil || routes == nil || worker.ValidateRoutes(routes, "service") != nil {
		return nil, errors.New("use a JSON array of valid public domain/service/port routes")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("routes_json must contain exactly one array")
	}
	for _, route := range routes {
		if route.Auth != "" {
			return nil, errors.New("preview credentials cannot be managed through public routes")
		}
	}
	return routes, nil
}

func sourceJSON(text string) (*worker.GitSource, error) {
	if text == "" {
		return nil, nil
	}
	var source worker.GitSource
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return nil, errors.New("source_json must contain valid GitSource JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || strings.TrimSpace(text) == "null" {
		return nil, errors.New("source_json must contain exactly one GitSource object")
	}
	if source.SSHPrivateKey != "" {
		return nil, errors.New("private SSH keys cannot be configured through Terraform")
	}
	return &source, nil
}
func (r *managedResource) ValidateConfig(ctx context.Context, request resource.ValidateConfigRequest, response *resource.ValidateConfigResponse) {
	data := r.read(ctx, request.Config, &response.Diagnostics)
	if response.Diagnostics.HasError() {
		return
	}
	if r.kind == "routes" && !data["routes_json"].IsUnknown() {
		if _, err := parseRoutes(data["routes_json"].ValueString()); err != nil {
			response.Diagnostics.AddAttributeError(path.Root("routes_json"), "Invalid routes", err.Error())
		}
	}

	if r.kind == "workload" && !data["source_json"].IsUnknown() {
		if _, err := sourceJSON(data["source_json"].ValueString()); err != nil {
			response.Diagnostics.AddAttributeError(path.Root("source_json"), "Invalid Git source", err.Error())
		}
	}
}
func (r *managedResource) write(ctx context.Context, data map[string]types.String, create bool) error {
	if r.api == nil {
		return errors.New("Cove provider is not configured")
	}
	method, endpoint := "PUT", r.endpoint(data)
	var body any
	switch r.kind {
	case "project", "environment":
		body = map[string]string{"name": data["name"].ValueString()}
		if create {
			method = "POST"
			endpoint = "/projects"
			if r.kind == "environment" {
				endpoint = "/projects/" + escape(data["project_id"].ValueString()) + "/environments"
			}
		}
	case "workload":
		source, err := sourceJSON(data["source_json"].ValueString())
		if err != nil {
			return err
		}
		body = map[string]any{"name": data["name"].ValueString(), "node_id": data["node_id"].ValueString(), "project_id": data["project_id"].ValueString(), "environment_id": data["environment_id"].ValueString(), "compose": data["compose"].ValueString(), "kind": data["kind"].ValueString(), "job_service": data["job_service"].ValueString(), "source": source}
		if create {
			method = "POST"
			endpoint = "/workloads"
		}
	case "variable":
		body = map[string]string{"value": data["value"].ValueString()}
	case "routes":
		routes, err := parseRoutes(data["routes_json"].ValueString())
		if err != nil {
			return err
		}
		body = map[string]any{"routes": routes}
	}
	var output struct {
		ID       string `json:"id"`
		Revision string `json:"version"`
		State    string `json:"state"`
	}
	if err := r.api.Do(ctx, method, endpoint, body, &output); err != nil {
		return err
	}
	switch r.kind {
	case "variable":
		data["id"] = types.StringValue(data["workload_id"].ValueString() + "/" + data["name"].ValueString())
		data["revision"] = types.StringValue(output.Revision)
	case "routes":
		data["id"] = data["workload_id"]
		if output.State == "queued" {
			if output.ID == "" {
				return errors.New("route update omitted operation ID")
			}
			if err := r.wait(ctx, output.ID); err != nil {
				return err
			}
		}
	default:
		if create {
			if output.ID == "" {
				return errors.New("create response omitted resource ID")
			}
			data["id"] = types.StringValue(output.ID)
		}
	}
	return nil
}
func (r *managedResource) wait(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		var result struct {
			State string `json:"state"`
		}
		if err := r.api.Do(ctx, "GET", "/operations/"+escape(id), nil, &result); err != nil {
			return err
		}
		switch result.State {
		case "succeeded":
			return nil
		case "queued", "running":
		case "failed", "cancelled", "interrupted":
			return errors.New("route operation failed; inspect Cove before applying again")
		default:
			return errors.New("unknown route operation state")
		}
		select {
		case <-ctx.Done():
			return errors.New("route wait expired; inspect operation before retrying")
		case <-time.After(time.Second):
		}
	}
}
func (r *managedResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	data := r.read(ctx, request.Plan, &response.Diagnostics)
	if response.Diagnostics.HasError() {
		return
	}
	if err := r.write(ctx, data, true); err != nil {
		response.Diagnostics.AddError("Cove create failed", err.Error())
		return
	}
	response.State = tfsdk.State{Raw: request.Plan.Raw, Schema: request.Plan.Schema}
	setState(ctx, &response.State, data, &response.Diagnostics)
}
func (r *managedResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	data := r.read(ctx, request.Plan, &response.Diagnostics)
	if response.Diagnostics.HasError() {
		return
	}
	if err := r.write(ctx, data, false); err != nil {
		response.Diagnostics.AddError("Cove update failed", err.Error())
		return
	}
	response.State = tfsdk.State{Raw: request.Plan.Raw, Schema: request.Plan.Schema}
	setState(ctx, &response.State, data, &response.Diagnostics)
}
func (r *managedResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	data := r.read(ctx, request.State, &response.Diagnostics)
	if response.Diagnostics.HasError() {
		return
	}
	if r.api == nil {
		response.Diagnostics.AddError("Cove read failed", "Provider is not configured.")
		return
	}
	endpoint := r.endpoint(data)
	if r.kind == "routes" {
		endpoint = "/workloads/" + escape(data["workload_id"].ValueString())
	}
	var body map[string]json.RawMessage
	if err := r.api.Do(ctx, "GET", endpoint, nil, &body); err != nil {
		var remote *client.Error
		if errors.As(err, &remote) && remote.Status == 404 {
			response.State.RemoveResource(ctx)
			return
		}
		response.Diagnostics.AddError("Cove read failed", err.Error())
		return
	}
	if r.kind == "variable" {
		var version string
		if json.Unmarshal(body["version"], &version) != nil {
			response.Diagnostics.AddError("Invalid Cove response", "Variable revision missing.")
			return
		}
		if !data["revision"].IsNull() && !data["revision"].IsUnknown() && data["revision"].ValueString() != version {
			data["value"] = types.StringNull()
		}
		data["revision"] = types.StringValue(version)
	} else if r.kind == "routes" {
		var routes []worker.Route
		if json.Unmarshal(body["routes"], &routes) != nil {
			response.Diagnostics.AddError("Invalid Cove response", "Route set missing.")
			return
		}
		if routes == nil {
			routes = []worker.Route{}
		}
		var saved []worker.Route
		if !data["routes_json"].IsNull() && !data["routes_json"].IsUnknown() {
			if err := json.Unmarshal([]byte(data["routes_json"].ValueString()), &saved); err != nil {
				response.Diagnostics.AddError("Invalid saved routes", "The previous route configuration cannot be decoded.")
				return
			}
		}
		for i := range routes {
			current := &routes[i]
			if current.Policy == nil || current.Policy.ForwardAuth == nil {
				continue
			}
			for _, previous := range saved {
				if previous.Domain != current.Domain || previous.Policy == nil || previous.Policy.PathPrefix != current.Policy.PathPrefix || previous.Policy.ForwardAuth == nil {
					continue
				}
				auth, old := current.Policy.ForwardAuth, previous.Policy.ForwardAuth
				if auth.URL == "" && auth.Service == "" {
					auth.URL = old.URL
				}
				if auth.Path == "" {
					auth.Path = old.Path
				}
				break
			}
		}
		encoded, _ := json.Marshal(routes)
		if !sameJSON(data["routes_json"].ValueString(), string(encoded)) {
			data["routes_json"] = types.StringValue(string(encoded))
		}
	} else {
		for name := range data {
			if name == "id" || name == "source_json" {
				continue
			}
			if raw, ok := body[name]; ok {
				var value string
				if json.Unmarshal(raw, &value) == nil {
					data[name] = types.StringValue(value)
				}
			}
		}
		if r.kind == "workload" {
			var source map[string]any
			if raw := body["source"]; len(raw) > 0 && string(raw) != "null" {
				if json.Unmarshal(raw, &source) != nil {
					response.Diagnostics.AddError("Invalid Cove response", "Git source is invalid.")
					return
				}
			}
			if source == nil {
				data["source_json"] = types.StringValue("")
			} else {
				var previous map[string]any
				json.Unmarshal([]byte(data["source_json"].ValueString()), &previous)
				importing := previous == nil
				if importing {
					previous = map[string]any{}
				}
				changed := importing
				for key, old := range previous {
					if key == "token" || key == "ssh_private_key" {
						continue
					}
					if _, exists := source[key]; !exists && !emptyJSON(old) {
						delete(previous, key)
						changed = true
					}
				}
				for key, value := range source {
					if key == "token" || key == "ssh_private_key" {
						continue
					}
					if old, managed := previous[key]; managed || importing {
						if !reflect.DeepEqual(old, value) {
							previous[key] = value
							changed = true
						}
					}
				}
				if changed {
					raw, _ := json.Marshal(previous)
					data["source_json"] = types.StringValue(string(raw))
				}
			}
		}
	}
	response.State = request.State
	setState(ctx, &response.State, data, &response.Diagnostics)
}
func emptyJSON(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case bool:
		return !value
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	case float64:
		return value == 0
	}
	return false
}

func sameJSON(a, b string) bool {
	var left, right any
	return json.Unmarshal([]byte(a), &left) == nil && json.Unmarshal([]byte(b), &right) == nil && reflect.DeepEqual(left, right)
}
func (r *managedResource) Delete(ctx context.Context, request resource.DeleteRequest, response *resource.DeleteResponse) {
	data := r.read(ctx, request.State, &response.Diagnostics)
	if response.Diagnostics.HasError() {
		return
	}
	if r.api == nil {
		response.Diagnostics.AddError("Cove delete failed", "Provider is not configured.")
		return
	}
	method, body := "DELETE", any(nil)
	if r.kind == "routes" {
		method = "PUT"
		body = map[string]any{"routes": []worker.Route{}}
	}
	var output struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	err := r.api.Do(ctx, method, r.endpoint(data), body, &output)
	var remote *client.Error
	if err != nil && !(errors.As(err, &remote) && remote.Status == 404) {
		response.Diagnostics.AddError("Cove delete failed", err.Error())
		return
	}
	if err == nil && r.kind == "routes" && output.State == "queued" {
		if err = r.wait(ctx, output.ID); err != nil {
			response.Diagnostics.AddError("Cove route removal failed", err.Error())
			return
		}
	}
	response.State.RemoveResource(ctx)
}
func (r *managedResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("id"), request.ID)...)
	if r.kind == "routes" {
		response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("workload_id"), request.ID)...)
	}
	if r.kind == "variable" {
		workload, name, ok := strings.Cut(request.ID, "/")
		if !ok || workload == "" || name == "" {
			response.Diagnostics.AddError("Invalid import ID", "Use WORKLOAD_ID/VARIABLE_NAME.")
			return
		}
		response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("workload_id"), workload)...)
		response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("name"), name)...)
	}
}

var _ resource.ResourceWithImportState = &managedResource{}
var _ resource.ResourceWithValidateConfig = &managedResource{}
var _ resource.ResourceWithConfigure = &managedResource{}
