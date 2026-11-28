package control

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

type scimDocument map[string]any

var scimAttributeNames = map[string]string{
	"username": "userName", "externalid": "externalId", "displayname": "displayName",
	"givenname": "givenName", "familyname": "familyName", "middlename": "middleName",
	"honorificprefix": "honorificPrefix", "honorificsuffix": "honorificSuffix",
	"nickname": "nickName", "profileurl": "profileUrl", "usertype": "userType",
	"preferredlanguage": "preferredLanguage", "phonenumbers": "phoneNumbers",
	"streetaddress": "streetAddress", "postalcode": "postalCode", "employeenumber": "employeeNumber",
	"costcenter": "costCenter", "operations": "Operations",
}

func scimAttribute(name string) string {
	if strings.EqualFold(name, scimEnterpriseSchema) {
		return scimEnterpriseSchema
	}
	lower := strings.ToLower(name)
	if canonical := scimAttributeNames[lower]; canonical != "" {
		return canonical
	}
	return lower
}

func normalizeSCIMObject(document map[string]any) error {
	seen := map[string]bool{}
	normalized := map[string]any{}
	for name, value := range document {
		canonical := scimAttribute(name)
		if seen[canonical] {
			return &scimProblem{Status: 400, Type: "invalidSyntax", Detail: "Duplicate attribute names are not allowed."}
		}
		seen[canonical] = true
		normalized[canonical] = value
		switch nested := value.(type) {
		case map[string]any:
			if err := normalizeSCIMObject(nested); err != nil {
				return err
			}
		case []any:
			for _, item := range nested {
				if object, ok := item.(map[string]any); ok {
					if err := normalizeSCIMObject(object); err != nil {
						return err
					}
				}
			}
		}
	}
	for name := range document {
		delete(document, name)
	}
	for name, value := range normalized {
		document[name] = value
	}
	return nil
}

func readSCIMDocument(w http.ResponseWriter, r *http.Request) (scimDocument, error) {
	if header := r.Header.Get("Content-Type"); header != "" {
		kind, _, err := mime.ParseMediaType(header)
		if err != nil || kind != "application/json" && kind != "application/scim+json" {
			return nil, &scimProblem{Status: 415, Detail: "Use application/scim+json or application/json."}
		}
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*1024*1024))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, &scimProblem{Status: 400, Type: "invalidSyntax", Detail: "A valid SCIM JSON object is required."}
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, &scimProblem{Status: 400, Type: "invalidSyntax", Detail: "Only one SCIM JSON object is allowed."}
	}
	if err := normalizeSCIMObject(document); err != nil {
		return nil, err
	}
	return document, nil
}

func scimSchemas(document scimDocument, required string) error {
	values, ok := document["schemas"].([]any)
	if !ok || len(values) > 8 {
		return &scimProblem{Status: 400, Type: "invalidValue", Detail: "The resource's SCIM schemas are required."}
	}
	found := false
	for _, value := range values {
		schema, ok := value.(string)
		if !ok || schema != required && !(required == scimUserSchema && schema == scimEnterpriseSchema) {
			return &scimProblem{Status: 400, Type: "invalidValue", Detail: "Only the advertised SCIM schemas are supported."}
		}
		found = found || schema == required
	}
	if !found {
		return &scimProblem{Status: 400, Type: "invalidValue", Detail: "The resource's core SCIM schema is required."}
	}
	return nil
}

type scimAttributePath struct {
	root        string
	sub         string
	filter      string
	filterValue any
}

var scimPathPattern = regexp.MustCompile(`(?i)^([a-z][a-z0-9]*)(?:\[([a-z][a-z0-9]*)\s+eq\s+("(?:[^"\\]|\\.)*"|true|false)\])?(?:\.([a-z][a-z0-9]*))?$`)

func parseSCIMPath(raw string) (scimAttributePath, error) {
	for _, schema := range []string{scimUserSchema, scimGroupSchema} {
		if strings.HasPrefix(strings.ToLower(raw), strings.ToLower(schema)+":") {
			raw = raw[len(schema)+1:]
		}
	}
	if strings.EqualFold(raw, scimEnterpriseSchema) {
		return scimAttributePath{root: scimEnterpriseSchema}, nil
	}
	if strings.HasPrefix(strings.ToLower(raw), strings.ToLower(scimEnterpriseSchema)+":") {
		attribute := raw[len(scimEnterpriseSchema)+1:]
		parts := strings.Split(attribute, ".")
		if len(parts) > 2 || parts[0] == "" {
			return scimAttributePath{}, &scimProblem{Status: 400, Type: "invalidPath", Detail: "Unsupported enterprise attribute path."}
		}
		return scimAttributePath{root: scimEnterpriseSchema, sub: attribute}, nil
	}
	match := scimPathPattern.FindStringSubmatch(raw)
	if match == nil {
		return scimAttributePath{}, &scimProblem{Status: 400, Type: "invalidPath", Detail: "Use an advertised attribute or a single eq value filter."}
	}
	path := scimAttributePath{root: scimAttribute(match[1]), sub: scimAttribute(match[4]), filter: scimAttribute(match[2])}
	if match[3] != "" {
		if json.Unmarshal([]byte(match[3]), &path.filterValue) != nil || !slices.Contains([]string{"value", "type", "primary"}, path.filter) {
			return path, &scimProblem{Status: 400, Type: "invalidPath", Detail: "Unsupported multivalue filter."}
		}
	}
	return path, nil
}

func scimWritableAttribute(root, kind string) bool {
	if kind == "groups" {
		return slices.Contains([]string{"displayName", "externalId", "members"}, root)
	}
	return slices.Contains([]string{"userName", "externalId", "displayName", "name", "nickName", "profileUrl", "title", "userType", "preferredLanguage", "locale", "timezone", "active", "emails", "phoneNumbers", "addresses", "ims", "photos", "entitlements", "roles", scimEnterpriseSchema}, root)
}

func applySCIMPatch(document, patch scimDocument, kind string) (scimDocument, error) {
	if err := scimSchemas(patch, scimPatchSchema); err != nil {
		return nil, err
	}
	operations, ok := patch["Operations"].([]any)
	if !ok || len(operations) == 0 || len(operations) > 100 {
		return nil, &scimProblem{Status: 400, Type: "tooMany", Detail: "Supply between one and 100 patch operations."}
	}
	for _, item := range operations {
		operation, ok := item.(map[string]any)
		if !ok {
			return nil, &scimProblem{Status: 400, Type: "invalidSyntax", Detail: "Each patch operation must be an object."}
		}
		op, _ := operation["op"].(string)
		op = strings.ToLower(op)
		path, _ := operation["path"].(string)
		value, supplied := operation["value"]
		if !slices.Contains([]string{"add", "replace", "remove"}, op) || op != "remove" && !supplied {
			return nil, &scimProblem{Status: 400, Type: "invalidSyntax", Detail: "Use add, replace or remove with the required value."}
		}
		if path == "" {
			fields, ok := value.(map[string]any)
			if !ok || op == "remove" {
				return nil, &scimProblem{Status: 400, Type: "invalidPath", Detail: "A pathless add or replace requires an attribute object."}
			}
			for attribute, v := range fields {
				if err := patchSCIMAttribute(document, scimAttributePath{root: attribute}, op, v, true, kind); err != nil {
					return nil, err
				}
			}
		} else {
			parsed, err := parseSCIMPath(path)
			if err != nil {
				return nil, err
			}
			if err = patchSCIMAttribute(document, parsed, op, value, supplied, kind); err != nil {
				return nil, err
			}
		}
	}
	return document, nil
}

func patchSCIMAttribute(document scimDocument, path scimAttributePath, op string, value any, supplied bool, kind string) error {
	if !scimWritableAttribute(path.root, kind) || path.root == "active" && op == "remove" {
		return &scimProblem{Status: 400, Type: "mutability", Detail: "This attribute is read-only or cannot be removed. Password synchronization is not supported."}
	}
	if op != "remove" {
		primary := path.sub == "primary" && value == true
		for _, item := range scimArray(value) {
			if object, ok := item.(map[string]any); ok && object["primary"] == true {
				primary = true
			}
		}
		if primary {
			for _, item := range scimArray(document[path.root]) {
				if object, ok := item.(map[string]any); ok {
					if path.filter != "" && scimValuesEqual(object[path.filter], path.filterValue) {
						continue
					}
					object["primary"] = false
				}
			}
		}
	}
	if path.filter != "" {
		items, _ := document[path.root].([]any)
		out := []any{}
		matched := false
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return &scimProblem{Status: 400, Type: "invalidValue", Detail: "Multivalued attributes must contain objects."}
			}
			matches := scimValuesEqual(object[path.filter], path.filterValue)
			if !matches {
				out = append(out, item)
				continue
			}
			matched = true
			if path.sub == "" {
				if op != "remove" {
					out = append(out, scimArray(value)...)
				}
			} else {
				if op == "remove" {
					delete(object, path.sub)
				} else {
					object[path.sub] = value
				}
				out = append(out, object)
			}
		}
		if !matched && op == "add" && path.sub != "" {
			out = append(out, map[string]any{path.filter: path.filterValue, path.sub: value})
		} else if !matched && !(op == "remove" && kind == "groups" && path.root == "members") {
			return &scimProblem{Status: 400, Type: "noTarget", Detail: "The selected attribute value does not exist."}
		}
		document[path.root] = out
		return nil
	}
	if path.sub != "" {
		if items, ok := document[path.root].([]any); ok {
			for _, item := range items {
				object, ok := item.(map[string]any)
				if !ok {
					return &scimProblem{Status: 400, Type: "invalidValue", Detail: "Multivalued attributes must contain objects."}
				}
				if op == "remove" {
					delete(object, path.sub)
				} else {
					object[path.sub] = value
				}
			}
			return nil
		}
		object, ok := document[path.root].(map[string]any)
		if !ok {
			object = map[string]any{}
			document[path.root] = object
		}
		parts := strings.Split(path.sub, ".")
		if len(parts) > 1 {
			nested, _ := object[scimAttribute(parts[0])].(map[string]any)
			if nested == nil {
				nested = map[string]any{}
				object[scimAttribute(parts[0])] = nested
			}
			object = nested
		}
		attribute := scimAttribute(parts[len(parts)-1])
		if op == "remove" {
			delete(object, attribute)
		} else {
			object[attribute] = value
		}
		return nil
	}
	if op == "remove" {
		if supplied && path.root == "members" {
			remove := map[string]bool{}
			for _, item := range scimArray(value) {
				object, ok := item.(map[string]any)
				id, valid := object["value"].(string)
				if !ok || !valid || id == "" {
					return &scimProblem{Status: 400, Type: "invalidValue", Detail: "Each removed member must identify a user."}
				}
				remove[id] = true
			}
			retained := []any{}
			for _, item := range scimArray(document[path.root]) {
				if object, ok := item.(map[string]any); ok {
					id, _ := object["value"].(string)
					if !remove[id] {
						retained = append(retained, item)
					}
				}
			}
			document[path.root] = retained
		} else {
			delete(document, path.root)
		}
	} else if op == "add" && slices.Contains([]string{"members", "emails", "phoneNumbers", "addresses", "ims", "photos", "entitlements", "roles"}, path.root) {
		items := scimArray(document[path.root])
		for _, added := range scimArray(value) {
			object, ok := added.(map[string]any)
			if ok && object["value"] != nil {
				index := slices.IndexFunc(items, func(existing any) bool {
					current, ok := existing.(map[string]any)
					return ok && scimValuesEqual(current["value"], object["value"]) && scimValuesEqual(current["type"], object["type"])
				})
				if index >= 0 {
					items[index] = added
					continue
				}
			}
			if !slices.ContainsFunc(items, func(existing any) bool { return reflect.DeepEqual(existing, added) }) {
				items = append(items, added)
			}
		}
		document[path.root] = items
	} else {
		document[path.root] = value
	}
	return nil
}

func scimValuesEqual(actual, expected any) bool {
	if a, ok := actual.(string); ok {
		if b, ok := expected.(string); ok {
			return strings.EqualFold(a, b)
		}
	}
	return reflect.DeepEqual(actual, expected)
}

func scimArray(value any) []any {
	if value == nil {
		return []any{}
	}
	if array, ok := value.([]any); ok {
		return array
	}
	return []any{value}
}
