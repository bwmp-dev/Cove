package control

func (s *Server) documentSCIM(document map[string]any) {
	paths := document["paths"].(map[string]any)
	schemes := document["components"].(map[string]any)["securitySchemes"].(map[string]any)
	schemes["scim"] = map[string]any{"type": "http", "scheme": "bearer", "description": "Separate instance provisioning token. Cove API tokens and session cookies are not accepted."}
	for _, resource := range []string{"Users", "Groups", "ServiceProviderConfig", "Schemas", "ResourceTypes"} {
		for _, item := range []bool{false, true} {
			if item && resource == "ServiceProviderConfig" {
				continue
			}
			path := "/" + resource
			parameters := []any{}
			if item {
				path += "/{id}"
				parameters = append(parameters, map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			}
			methods := []string{"get"}
			if resource == "Users" || resource == "Groups" {
				if item {
					methods = append(methods, "put", "patch", "delete")
				} else {
					methods = append(methods, "post")
				}
			}
			operations := map[string]any{}
			for _, method := range methods {
				responses := map[string]any{}
				for _, code := range []string{"200", "201", "204", "400", "401", "403", "404", "409", "412", "503"} {
					response := map[string]any{"description": "SCIM protocol response"}
					if code != "204" {
						response["content"] = map[string]any{"application/scim+json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": true}}}
					}
					responses[code] = response
				}
				operation := map[string]any{
					"summary":     "SCIM 2.0 " + resource,
					"description": "Native provisioning protocol; current token version is rechecked before committing writes. Deprovisioning revokes owned credentials and automation. Last active owners and instance administrators are protected.",
					"servers":     []any{map[string]string{"url": s.panelBasePath() + "/scim/v2"}},
					"security":    []any{map[string]any{"scim": []string{}}}, "parameters": parameters, "responses": responses,
				}
				if method == "post" || method == "put" || method == "patch" {
					operation["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/scim+json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": true}}}}
				}
				operations[method] = operation
			}
			paths[path] = operations
		}
	}
}
