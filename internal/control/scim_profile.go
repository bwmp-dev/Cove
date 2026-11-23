package control

import (
	"slices"
	"strconv"
	"strings"
)

type scimUserProfile struct {
	Username string
	External string
	Email    string
	Name     string
	Active   bool
	Document scimDocument
}

func scimComplex(value any, fields []string, maximum int) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "A complex attribute must be an object."}
	}
	out := map[string]any{}
	for _, field := range fields {
		v, found := object[field]
		if !found || v == nil {
			continue
		}
		if field == "primary" {
			if _, valid := v.(bool); !valid {
				return nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "primary must be a boolean."}
			}
		} else if _, err := scimString(object, field, maximum); err != nil {
			return nil, err
		}
		out[field] = v
	}
	return out, nil
}

func scimString(document scimDocument, attribute string, maximum int) (string, error) {
	value, found := document[attribute]
	if !found || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok || len(text) > maximum || strings.ContainsRune(text, 0) {
		return "", &scimProblem{Status: 400, Type: "invalidValue", Detail: "A supported string attribute has an invalid value or length."}
	}
	return text, nil
}

func validateSCIMUser(document scimDocument) (scimUserProfile, error) {
	profile := scimUserProfile{Active: true, Document: scimDocument{}}
	if err := scimSchemas(document, scimUserSchema); err != nil {
		return profile, err
	}
	username, err := scimString(document, "userName", 254)
	if err != nil {
		return profile, err
	}
	profile.Username = strings.ToLower(strings.TrimSpace(username))
	if profile.Username == "" || strings.ContainsAny(profile.Username, "\r\n") {
		return profile, &scimProblem{Status: 400, Type: "invalidValue", Detail: "A nonempty userName is required."}
	}
	if profile.External, err = scimString(document, "externalId", 1024); err != nil {
		return profile, err
	}
	if value, found := document["active"]; found {
		switch active := value.(type) {
		case bool:
			profile.Active = active
		case string:
			profile.Active, err = strconv.ParseBool(active)
		default:
			err = &scimProblem{Status: 400, Type: "invalidValue", Detail: "active must be a boolean."}
		}
		if err != nil {
			return profile, &scimProblem{Status: 400, Type: "invalidValue", Detail: "active must be a boolean."}
		}
	}
	for attribute, value := range document {
		if scimWritableAttribute(attribute, "users") && value != nil {
			profile.Document[attribute] = value
		}
	}
	for _, attribute := range []string{"displayName", "nickName", "profileUrl", "title", "userType", "preferredLanguage", "locale", "timezone"} {
		if _, err = scimString(profile.Document, attribute, 2048); err != nil {
			return profile, err
		}
	}
	if value := document["name"]; value != nil {
		profile.Document["name"], err = scimComplex(value, []string{"formatted", "givenName", "familyName", "middleName", "honorificPrefix", "honorificSuffix"}, 256)
		if err != nil {
			return profile, err
		}
	}
	if value := document[scimEnterpriseSchema]; value != nil {
		enterprise, ok := value.(map[string]any)
		if !ok {
			return profile, &scimProblem{Status: 400, Type: "invalidValue", Detail: "The enterprise extension must be a complex attribute."}
		}
		clean, e := scimComplex(enterprise, []string{"employeeNumber", "costCenter", "organization", "division", "department"}, 2048)
		if e != nil {
			return profile, e
		}
		if manager := enterprise["manager"]; manager != nil {
			clean["manager"], err = scimComplex(manager, []string{"value", "$ref", "displayName"}, 2048)
			if err != nil {
				return profile, err
			}
		}
		profile.Document[scimEnterpriseSchema] = clean
	}
	for _, attribute := range []string{"phoneNumbers", "addresses", "ims", "photos", "entitlements", "roles"} {
		if document[attribute] == nil {
			continue
		}
		if len(scimArray(document[attribute])) > 100 {
			return profile, &scimProblem{Status: 400, Type: "tooMany", Detail: "A profile attribute can contain at most 100 values."}
		}
		values := []any{}
		fields := []string{"value", "type", "primary", "display", "$ref"}
		if attribute == "addresses" {
			fields = []string{"formatted", "streetAddress", "locality", "region", "postalCode", "country", "type", "primary"}
		}
		for _, item := range scimArray(document[attribute]) {
			clean, e := scimComplex(item, fields, 4096)
			if e != nil {
				return profile, e
			}
			values = append(values, clean)
		}
		profile.Document[attribute] = values
	}
	emails := []any{}
	primary := false
	if len(scimArray(document["emails"])) > 20 {
		return profile, &scimProblem{Status: 400, Type: "tooMany", Detail: "At most 20 email addresses are supported."}
	}
	for _, item := range scimArray(document["emails"]) {
		email, e := scimComplex(item, []string{"value", "type", "primary", "display"}, 254)
		if e != nil {
			return profile, e
		}
		value, e := scimString(email, "value", 254)
		if e != nil {
			return profile, e
		}
		if value == "" {
			continue
		}
		normalized, valid := normalizeEmail(value)
		if !valid {
			return profile, &scimProblem{Status: 400, Type: "invalidValue", Detail: "A valid email address is required."}
		}
		chosen := false
		if supplied, found := email["primary"]; found {
			var ok bool
			chosen, ok = supplied.(bool)
			if !ok || primary && chosen {
				return profile, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Only one email can be primary."}
			}
		}
		if profile.Email == "" || chosen {
			profile.Email = normalized
		}
		primary = primary || chosen
		email["value"] = normalized
		emails = append(emails, email)
	}
	if profile.Email == "" {
		var valid bool
		profile.Email, valid = normalizeEmail(profile.Username)
		if !valid {
			return profile, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Supply an email address, or an email userName, for the Cove account."}
		}
		emails = append(emails, map[string]any{"value": profile.Email, "primary": true, "type": "work"})
	}
	profile.Name, _ = document["displayName"].(string)
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		if name, ok := document["name"].(map[string]any); ok {
			profile.Name, _ = name["formatted"].(string)
			if profile.Name == "" {
				first, _ := name["givenName"].(string)
				last, _ := name["familyName"].(string)
				profile.Name = strings.TrimSpace(first + " " + last)
			}
		}
		if profile.Name == "" || len(profile.Name) > 120 {
			profile.Name = "Provisioned user"
		}
	}
	if len(profile.Name) > 120 || strings.ContainsAny(profile.Name, "\x00\r\n") {
		return profile, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Use a display name up to 120 bytes without control characters."}
	}
	profile.Document["schemas"] = []any{scimUserSchema}
	if profile.Document[scimEnterpriseSchema] != nil {
		profile.Document["schemas"] = []any{scimUserSchema, scimEnterpriseSchema}
	}
	profile.Document["userName"], profile.Document["externalId"], profile.Document["active"], profile.Document["emails"] = profile.Username, profile.External, profile.Active, emails
	return profile, nil
}

func validateSCIMGroup(document scimDocument) (string, string, []string, error) {
	if err := scimSchemas(document, scimGroupSchema); err != nil {
		return "", "", nil, err
	}
	name, err := scimString(document, "displayName", 256)
	if err != nil {
		return "", "", nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return "", "", nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "A nonempty group displayName is required."}
	}
	external, err := scimString(document, "externalId", 1024)
	if err != nil {
		return "", "", nil, err
	}
	if len(scimArray(document["members"])) > 10000 {
		return "", "", nil, &scimProblem{Status: 400, Type: "tooMany", Detail: "A group can have at most 10000 user members."}
	}
	members := []string{}
	for _, item := range scimArray(document["members"]) {
		member, ok := item.(map[string]any)
		if !ok {
			return "", "", nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Group members must be user resource objects."}
		}
		id, e := scimString(member, "value", 128)
		if e != nil {
			return "", "", nil, e
		}
		kind, e := scimString(member, "type", 32)
		if e != nil || id == "" || kind != "" && !strings.EqualFold(kind, "User") {
			return "", "", nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Nested groups are not supported; use existing SCIM user IDs."}
		}
		members = append(members, id)
	}
	slices.Sort(members)
	return name, external, slices.Compact(members), nil
}
