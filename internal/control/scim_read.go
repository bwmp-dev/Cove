package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type scimUserRecord struct {
	ID, Username, External, Email, Name     string
	Disabled, Administrator, ManagedDisable bool
	OIDCProvider                            *string
	DeletedAt                               *time.Time
	Created, Modified                       time.Time
	Version                                 int64
	Value                                   scimDocument
}

type scimGroupRecord struct {
	ID                string
	Created, Modified time.Time
	Version           int64
	Value             scimDocument
}

func scimETag(version int64) string { return `W/"` + strconv.FormatInt(version, 10) + `"` }

func (s *Server) scimMetadata(id, kind string, created, modified time.Time, version int64) map[string]any {
	return map[string]any{"resourceType": kind, "created": created.UTC().Format(time.RFC3339Nano), "lastModified": modified.UTC().Format(time.RFC3339Nano), "version": scimETag(version), "location": s.PublicURL + "/scim/v2/" + kind + "s/" + url.PathEscape(id)}
}

func (s *Server) loadSCIMUser(ctx context.Context, tx pgx.Tx, id string, lock, includeDeleted bool) (scimUserRecord, error) {
	var user scimUserRecord
	query := `SELECT s.id,s.user_name,s.external_id,COALESCE(u.email,''),u.name,u.disabled,u.is_admin,s.disabled_by_scim,s.oidc_provider_id,s.deleted_at,s.created_at,s.modified_at,s.version,s.document
 FROM scim_users s JOIN users u ON u.id=s.id WHERE s.id=$1 AND ($2 OR s.deleted_at IS NULL)`
	if lock {
		query += " FOR UPDATE OF s,u"
	}
	var raw []byte
	err := tx.QueryRow(ctx, query, id, includeDeleted).Scan(&user.ID, &user.Username, &user.External, &user.Email, &user.Name, &user.Disabled, &user.Administrator, &user.ManagedDisable, &user.OIDCProvider, &user.DeletedAt, &user.Created, &user.Modified, &user.Version, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return user, &scimProblem{Status: 404, Detail: "The SCIM user does not exist."}
	}
	if err != nil {
		return user, err
	}
	if err = json.Unmarshal(raw, &user.Value); err != nil {
		return user, err
	}
	user.Value["id"], user.Value["userName"], user.Value["externalId"], user.Value["displayName"], user.Value["active"] = user.ID, user.Username, user.External, user.Name, !user.Disabled
	user.Value["meta"] = s.scimMetadata(user.ID, "User", user.Created, user.Modified, user.Version)
	groups := []any{}
	rows, err := tx.Query(ctx, "SELECT g.id,g.name FROM scim_group_members m JOIN scim_groups g ON g.id=m.group_id WHERE m.user_id=$1 ORDER BY g.id", id)
	if err != nil {
		return user, err
	}
	defer rows.Close()
	for rows.Next() {
		var group, name string
		if err = rows.Scan(&group, &name); err != nil {
			return user, err
		}
		groups = append(groups, map[string]any{"value": group, "display": name, "type": "direct", "$ref": s.PublicURL + "/scim/v2/Groups/" + url.PathEscape(group)})
	}
	user.Value["groups"] = groups
	return user, rows.Err()
}

func (s *Server) loadSCIMGroup(ctx context.Context, tx pgx.Tx, id string, lock bool) (scimGroupRecord, error) {
	group := scimGroupRecord{ID: id}
	query := "SELECT name,external_id,created_at,modified_at,version FROM scim_groups WHERE id=$1"
	if lock {
		query += " FOR UPDATE"
	}
	var name, external string
	err := tx.QueryRow(ctx, query, id).Scan(&name, &external, &group.Created, &group.Modified, &group.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return group, &scimProblem{Status: 404, Detail: "The SCIM group does not exist."}
	}
	if err != nil {
		return group, err
	}
	members := []any{}
	rows, err := tx.Query(ctx, "SELECT u.id,u.name FROM scim_group_members m JOIN scim_users s ON s.id=m.user_id JOIN users u ON u.id=s.id WHERE m.group_id=$1 AND s.deleted_at IS NULL ORDER BY u.id", id)
	if err != nil {
		return group, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, display string
		if err = rows.Scan(&id, &display); err != nil {
			return group, err
		}
		members = append(members, map[string]any{"value": id, "display": display, "type": "User", "$ref": s.PublicURL + "/scim/v2/Users/" + url.PathEscape(id)})
	}
	group.Value = scimDocument{"schemas": []any{scimGroupSchema}, "id": id, "displayName": name, "externalId": external, "members": members, "meta": s.scimMetadata(id, "Group", group.Created, group.Modified, group.Version)}
	return group, rows.Err()
}

func projectSCIMResource(document scimDocument, query url.Values) (scimDocument, error) {
	attributes, excluded := query.Get("attributes"), query.Get("excludedAttributes")
	if attributes != "" && excluded != "" {
		return nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Use attributes or excludedAttributes, not both."}
	}
	if len(attributes)+len(excluded) > 4096 {
		return nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Attribute projection is too long."}
	}
	out := document
	if attributes != "" {
		out = scimDocument{"schemas": document["schemas"], "id": document["id"], "meta": document["meta"]}
	}
	raw := attributes
	if excluded != "" {
		raw = excluded
	}
	if raw == "" {
		return out, nil
	}
	for _, name := range strings.Split(raw, ",") {
		path, err := parseSCIMPath(strings.TrimSpace(name))
		if err != nil || path.filter != "" {
			return nil, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Use simple attribute paths for projection."}
		}
		if path.root == "id" || path.root == "schemas" {
			continue
		}
		if excluded != "" {
			if path.sub == "" {
				delete(out, path.root)
			} else if object, ok := out[path.root].(map[string]any); ok {
				projectSCIMObject(object, nil, strings.Split(path.sub, "."), true)
			} else if array, ok := out[path.root].([]any); ok {
				for _, value := range array {
					if object, ok := value.(map[string]any); ok {
						projectSCIMObject(object, nil, strings.Split(path.sub, "."), true)
					}
				}
			}
		} else if value, found := document[path.root]; found {
			if path.sub == "" {
				out[path.root] = value
			} else if object, ok := value.(map[string]any); ok {
				selected, _ := out[path.root].(map[string]any)
				if selected == nil {
					selected = map[string]any{}
					out[path.root] = selected
				}
				projectSCIMObject(object, selected, strings.Split(path.sub, "."), false)
			} else if array, ok := value.([]any); ok {
				selected, _ := out[path.root].([]any)
				if len(selected) != len(array) {
					selected = make([]any, len(array))
				}
				for i, item := range array {
					if object, ok := item.(map[string]any); ok {
						entry, _ := selected[i].(map[string]any)
						if entry == nil {
							entry = map[string]any{}
							selected[i] = entry
						}
						projectSCIMObject(object, entry, strings.Split(path.sub, "."), false)
					}
				}
				out[path.root] = selected
			}
		}
	}
	return out, nil
}

func projectSCIMObject(source, target map[string]any, path []string, exclude bool) {
	key := scimAttribute(path[0])
	value, found := source[key]
	if !found {
		return
	}
	if len(path) == 1 {
		if exclude {
			delete(source, key)
		} else {
			target[key] = value
		}
		return
	}
	child, ok := value.(map[string]any)
	if !ok {
		return
	}
	if exclude {
		projectSCIMObject(child, nil, path[1:], true)
		return
	}
	next, _ := target[key].(map[string]any)
	if next == nil {
		next = map[string]any{}
		target[key] = next
	}
	projectSCIMObject(child, next, path[1:], false)
}

func (s *Server) listSCIMResources(w http.ResponseWriter, r *http.Request, kind string) {
	query := r.URL.Query()
	start, count := 1, 100
	var err error
	if query.Has("startIndex") {
		start, err = strconv.Atoi(query.Get("startIndex"))
		if err != nil || start > 2147483647 {
			scimFailure(w, &scimProblem{Status: 400, Type: "invalidValue", Detail: "startIndex must be a valid one-based index."})
			return
		}
		start = max(start, 1)
	}
	if query.Has("count") {
		count, err = strconv.Atoi(query.Get("count"))
		if err != nil || count < 0 {
			scimFailure(w, &scimProblem{Status: 400, Type: "invalidValue", Detail: "count must be a nonnegative integer."})
			return
		}
		count = min(count, 200)
	}
	if query.Get("sortBy") != "" || query.Get("sortOrder") != "" || len(query["filter"]) > 1 {
		scimFailure(w, &scimProblem{Status: 400, Type: "invalidValue", Detail: "Sorting and multiple filters are not supported."})
		return
	}
	if _, err = projectSCIMResource(scimDocument{}, query); err != nil {
		scimFailure(w, err)
		return
	}
	filter, args, err := compileSCIMFilter(query.Get("filter"), kind)
	if err != nil {
		scimFailure(w, err)
		return
	}
	from, column, where := "scim_groups g", "g.id", "("+filter+")"
	if kind == "users" {
		from, column, where = "scim_users s JOIN users u ON u.id=s.id", "s.id", "s.deleted_at IS NULL AND ("+filter+")"
	}
	tx, err := s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		scimFailure(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var total int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM "+from+" WHERE "+where, args...).Scan(&total); err != nil {
		scimFailure(w, err)
		return
	}
	values := []any{}
	if count > 0 {
		statement := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d", column, from, where, column, len(args)+1, len(args)+2)
		rows, e := tx.Query(r.Context(), statement, append(args, count, start-1)...)
		if e != nil {
			scimFailure(w, e)
			return
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			scimFailure(w, errors.New("SCIM resource page unavailable"))
			return
		}
		for _, id := range ids {
			var value scimDocument
			if kind == "users" {
				user, e := s.loadSCIMUser(r.Context(), tx, id, false, false)
				value, err = user.Value, e
			} else {
				group, e := s.loadSCIMGroup(r.Context(), tx, id, false)
				value, err = group.Value, e
			}
			if err == nil {
				value, err = projectSCIMResource(value, query)
			}
			if err != nil {
				scimFailure(w, err)
				return
			}
			values = append(values, value)
		}
	}
	scimReply(w, 200, map[string]any{"schemas": []string{scimListSchema}, "totalResults": total, "startIndex": start, "itemsPerPage": len(values), "Resources": values})
}
