package control

import (
	"encoding/json"
	"fmt"
	"strings"
)

type scimFilterToken struct {
	text   string
	quoted bool
}

type scimFilterParser struct {
	tokens []scimFilterToken
	index  int
	kind   string
	args   []any
}

func invalidSCIMFilter() error {
	return &scimProblem{Status: 400, Type: "invalidFilter", Detail: "Use supported SCIM attributes with eq, ne, co, sw, ew, pr, and/or and parentheses."}
}

func compileSCIMFilter(raw, kind string) (string, []any, error) {
	if strings.TrimSpace(raw) == "" {
		return "true", []any{}, nil
	}
	if len(raw) > 4096 {
		return "", nil, invalidSCIMFilter()
	}
	p := scimFilterParser{kind: kind, args: []any{}}
	for i := 0; i < len(raw); {
		if strings.ContainsRune(" \r\n\t", rune(raw[i])) {
			i++
			continue
		}
		start := i
		switch raw[i] {
		case '(', ')':
			p.tokens = append(p.tokens, scimFilterToken{text: raw[i : i+1]})
			i++
		case '"':
			i++
			escaped := false
			for i < len(raw) {
				c := raw[i]
				i++
				if c == '"' && !escaped {
					break
				}
				if c == '\\' && !escaped {
					escaped = true
				} else {
					escaped = false
				}
			}
			var value string
			if json.Unmarshal([]byte(raw[start:i]), &value) != nil {
				return "", nil, invalidSCIMFilter()
			}
			p.tokens = append(p.tokens, scimFilterToken{text: value, quoted: true})
		default:
			for i < len(raw) && (raw[i] >= 'a' && raw[i] <= 'z' || raw[i] >= 'A' && raw[i] <= 'Z' || raw[i] >= '0' && raw[i] <= '9' || strings.ContainsRune("._:-", rune(raw[i]))) {
				i++
			}
			if i == start {
				return "", nil, invalidSCIMFilter()
			}
			p.tokens = append(p.tokens, scimFilterToken{text: strings.ToLower(raw[start:i])})
		}
		if len(p.tokens) > 128 {
			return "", nil, invalidSCIMFilter()
		}
	}
	query, err := p.expression(0, false)
	if err != nil || p.index != len(p.tokens) {
		return "", nil, invalidSCIMFilter()
	}
	return query, p.args, nil
}

func (p *scimFilterParser) take(word string) bool {
	if p.index < len(p.tokens) && !p.tokens[p.index].quoted && p.tokens[p.index].text == word {
		p.index++
		return true
	}
	return false
}

func (p *scimFilterParser) expression(depth int, conjunction bool) (string, error) {
	if depth > 8 {
		return "", invalidSCIMFilter()
	}
	var left string
	var err error
	if conjunction {
		left, err = p.atom(depth)
	} else {
		left, err = p.expression(depth, true)
	}
	if err != nil {
		return "", err
	}
	operator := "or"
	if conjunction {
		operator = "and"
	}
	for p.take(operator) {
		var right string
		if conjunction {
			right, err = p.atom(depth)
		} else {
			right, err = p.expression(depth, true)
		}
		if err != nil {
			return "", err
		}
		left = "(" + left + " " + operator + " " + right + ")"
	}
	return left, nil
}

func (p *scimFilterParser) atom(depth int) (string, error) {
	if p.take("(") {
		value, err := p.expression(depth+1, false)
		if err != nil || !p.take(")") {
			return "", invalidSCIMFilter()
		}
		return "(" + value + ")", nil
	}
	if p.index+1 >= len(p.tokens) || p.tokens[p.index].quoted || p.tokens[p.index+1].quoted {
		return "", invalidSCIMFilter()
	}
	attribute, operator := p.tokens[p.index].text, p.tokens[p.index+1].text
	p.index += 2
	attribute = strings.TrimPrefix(attribute, strings.ToLower(scimUserSchema)+":")
	attribute = strings.TrimPrefix(attribute, strings.ToLower(scimGroupSchema)+":")
	fields := map[string]string{"id": "s.id", "username": "s.user_name", "externalid": "s.external_id", "displayname": "u.name", "name.givenname": "s.document#>>'{name,givenName}'", "name.familyname": "s.document#>>'{name,familyName}'", "active": "NOT u.disabled"}
	if p.kind == "groups" {
		fields = map[string]string{"id": "g.id", "externalid": "g.external_id", "displayname": "g.name"}
	}
	column, found := fields[attribute]
	multivalue := attribute == "emails.value" && p.kind == "users" || attribute == "members.value" && p.kind == "groups"
	if !found && !multivalue {
		return "", invalidSCIMFilter()
	}
	if attribute == "active" {
		if operator == "pr" {
			return "true", nil
		}
		if operator != "eq" && operator != "ne" || p.index >= len(p.tokens) {
			return "", invalidSCIMFilter()
		}
		value := strings.ToLower(p.tokens[p.index].text)
		p.index++
		if value != "true" && value != "false" {
			return "", invalidSCIMFilter()
		}
		p.args = append(p.args, value == "true")
		comparison := "="
		if operator == "ne" {
			comparison = "<>"
		}
		return fmt.Sprintf("(%s)%s$%d", column, comparison, len(p.args)), nil
	}
	fold := attribute != "id" && attribute != "externalid" && attribute != "members.value"
	value := ""
	if operator != "pr" {
		if p.index >= len(p.tokens) || !p.tokens[p.index].quoted {
			return "", invalidSCIMFilter()
		}
		p.args = append(p.args, p.tokens[p.index].text)
		p.index++
		value = fmt.Sprintf("$%d", len(p.args))
	}
	if multivalue {
		from := "jsonb_array_elements(COALESCE(s.document->'emails','[]'::jsonb)) item"
		column = "item->>'value'"
		prefix := ""
		if attribute == "members.value" {
			from, column, prefix = "scim_group_members item", "item.user_id", "item.group_id=g.id AND "
		}
		predicate, err := scimTextPredicate(column, operator, value, fold)
		if err != nil {
			return "", err
		}
		return "EXISTS(SELECT 1 FROM " + from + " WHERE " + prefix + predicate + ")", nil
	}
	return scimTextPredicate(column, operator, value, fold)
}

func scimTextPredicate(column, operator, value string, fold bool) (string, error) {
	if operator == "pr" {
		return "(" + column + " IS NOT NULL AND " + column + "<>'')", nil
	}
	if fold {
		column, value = "lower("+column+")", "lower("+value+")"
	}
	switch operator {
	case "eq":
		return column + "=" + value, nil
	case "ne":
		return column + "<>" + value, nil
	case "co":
		return "strpos(" + column + "," + value + ")>0", nil
	case "sw":
		return "left(" + column + ",length(" + value + "))=" + value, nil
	case "ew":
		return "right(" + column + ",length(" + value + "))=" + value, nil
	}
	return "", invalidSCIMFilter()
}
