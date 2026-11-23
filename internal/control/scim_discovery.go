package control

import "net/http"

func scimSchemaAttribute(name, kind string, multi, required, readOnly bool, children ...map[string]any) map[string]any {
	mutability := "readWrite"
	if readOnly {
		mutability = "readOnly"
	}
	attribute := map[string]any{"name": name, "type": kind, "multiValued": multi, "required": required, "caseExact": false, "mutability": mutability, "returned": "default", "uniqueness": "none"}
	if kind == "reference" {
		attribute["referenceTypes"] = []string{"uri"}
	}
	if len(children) > 0 {
		attribute["subAttributes"] = children
	}
	return attribute
}

func (s *Server) scimSchemas() []scimDocument {
	common := []map[string]any{
		scimSchemaAttribute("id", "string", false, true, true),
		scimSchemaAttribute("externalId", "string", false, false, false),
		scimSchemaAttribute("meta", "complex", false, false, true,
			scimSchemaAttribute("resourceType", "string", false, false, true),
			scimSchemaAttribute("created", "dateTime", false, false, true),
			scimSchemaAttribute("lastModified", "dateTime", false, false, true),
			scimSchemaAttribute("location", "reference", false, false, true),
			scimSchemaAttribute("version", "string", false, false, true)),
	}
	common[0]["caseExact"], common[0]["returned"], common[0]["uniqueness"] = true, "always", "global"
	common[1]["caseExact"] = true
	username := scimSchemaAttribute("userName", "string", false, true, false)
	username["uniqueness"] = "server"
	user := append([]map[string]any{}, common...)
	user = append(user, username, scimSchemaAttribute("active", "boolean", false, false, false),
		scimSchemaAttribute("name", "complex", false, false, false,
			scimSchemaAttribute("formatted", "string", false, false, false),
			scimSchemaAttribute("givenName", "string", false, false, false),
			scimSchemaAttribute("familyName", "string", false, false, false),
			scimSchemaAttribute("middleName", "string", false, false, false),
			scimSchemaAttribute("honorificPrefix", "string", false, false, false),
			scimSchemaAttribute("honorificSuffix", "string", false, false, false)))
	for _, name := range []string{"displayName", "nickName", "profileUrl", "title", "userType", "preferredLanguage", "locale", "timezone"} {
		user = append(user, scimSchemaAttribute(name, "string", false, false, false))
	}
	for _, name := range []string{"emails", "phoneNumbers", "ims", "photos", "entitlements", "roles"} {
		user = append(user, scimSchemaAttribute(name, "complex", true, false, false,
			scimSchemaAttribute("value", "string", false, false, false),
			scimSchemaAttribute("type", "string", false, false, false),
			scimSchemaAttribute("display", "string", false, false, false),
			scimSchemaAttribute("primary", "boolean", false, false, false)))
	}
	address := []map[string]any{}
	for _, name := range []string{"formatted", "streetAddress", "locality", "region", "postalCode", "country", "type"} {
		address = append(address, scimSchemaAttribute(name, "string", false, false, false))
	}
	address = append(address, scimSchemaAttribute("primary", "boolean", false, false, false))
	user = append(user, scimSchemaAttribute("addresses", "complex", true, false, false, address...),
		scimSchemaAttribute("groups", "complex", true, false, true,
			scimSchemaAttribute("value", "string", false, false, true),
			scimSchemaAttribute("display", "string", false, false, true),
			scimSchemaAttribute("type", "string", false, false, true),
			scimSchemaAttribute("$ref", "reference", false, false, true)))
	group := append([]map[string]any{}, common...)
	group = append(group, scimSchemaAttribute("displayName", "string", false, true, false),
		scimSchemaAttribute("members", "complex", true, false, false,
			scimSchemaAttribute("value", "string", false, true, false),
			scimSchemaAttribute("type", "string", false, false, false),
			scimSchemaAttribute("display", "string", false, false, true),
			scimSchemaAttribute("$ref", "reference", false, false, true)))
	enterprise := []map[string]any{}
	for _, name := range []string{"employeeNumber", "costCenter", "organization", "division", "department"} {
		enterprise = append(enterprise, scimSchemaAttribute(name, "string", false, false, false))
	}
	enterprise = append(enterprise, scimSchemaAttribute("manager", "complex", false, false, false,
		scimSchemaAttribute("value", "string", false, false, false),
		scimSchemaAttribute("$ref", "reference", false, false, false),
		scimSchemaAttribute("displayName", "string", false, false, true)))
	out := []scimDocument{}
	for _, item := range []struct {
		id, name   string
		attributes []map[string]any
	}{{scimUserSchema, "User", user}, {scimGroupSchema, "Group", group}, {scimEnterpriseSchema, "EnterpriseUser", enterprise}} {
		out = append(out, scimDocument{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:Schema"}, "id": item.id, "name": item.name, "attributes": item.attributes, "meta": map[string]any{"resourceType": "Schema", "location": s.PublicURL + "/scim/v2/Schemas/" + item.id}})
	}
	return out
}

func (s *Server) scimDiscovery(w http.ResponseWriter, r *http.Request, kind, id string) {
	if r.Method != "GET" {
		scimFailure(w, &scimProblem{Status: 405, Detail: "SCIM discovery is read-only."})
		return
	}
	if kind == "serviceproviderconfig" && id == "" {
		scimReply(w, 200, map[string]any{
			"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
			"patch":   map[string]any{"supported": true}, "bulk": map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
			"filter": map[string]any{"supported": true, "maxResults": 200}, "changePassword": map[string]any{"supported": false},
			"sort": map[string]any{"supported": false}, "etag": map[string]any{"supported": true},
			"authenticationSchemes": []map[string]any{{"type": "httpbearer", "name": "Cove instance provisioning token", "description": "Use a separately configured, rotatable instance SCIM token.", "specUri": "https://www.rfc-editor.org/rfc/rfc6750", "primary": true}},
		})
		return
	}
	values := []scimDocument{}
	if kind == "schemas" {
		values = s.scimSchemas()
	} else if kind == "resourcetypes" {
		for _, item := range []struct{ name, schema string }{{"User", scimUserSchema}, {"Group", scimGroupSchema}} {
			value := scimDocument{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": item.name, "name": item.name, "endpoint": "/" + item.name + "s", "schema": item.schema, "meta": map[string]any{"resourceType": "ResourceType", "location": s.PublicURL + "/scim/v2/ResourceTypes/" + item.name}}
			if item.name == "User" {
				value["schemaExtensions"] = []map[string]any{{"schema": scimEnterpriseSchema, "required": false}}
			}
			values = append(values, value)
		}
	}
	if id != "" {
		for _, value := range values {
			if value["id"] == id {
				scimReply(w, 200, value)
				return
			}
		}
		scimFailure(w, &scimProblem{Status: 404, Detail: "The SCIM discovery resource does not exist."})
		return
	}
	scimReply(w, 200, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(values), "startIndex": 1, "itemsPerPage": len(values), "Resources": values})
}
