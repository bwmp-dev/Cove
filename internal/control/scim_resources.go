package control

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func checkSCIMVersion(r *http.Request, version int64) error {
	if header := r.Header.Get("If-Match"); header != "" {
		for _, value := range strings.Split(header, ",") {
			if value = strings.TrimSpace(value); value == "*" || value == scimETag(version) {
				return nil
			}
		}
		return &scimProblem{Status: 412, Type: "invalidVers", Detail: "The resource changed. Read its current version before updating it."}
	}
	return nil
}

func (s *Server) scimResources(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/scim/v2/"), "/"), "/")
	if len(parts) == 0 || len(parts) > 2 {
		scimFailure(w, &scimProblem{Status: 404, Detail: "The SCIM endpoint does not exist."})
		return
	}
	kind, id := strings.ToLower(parts[0]), ""
	if len(parts) == 2 {
		id = parts[1]
	}
	if kind == "serviceproviderconfig" || kind == "schemas" || kind == "resourcetypes" {
		s.scimDiscovery(w, r, kind, id)
		return
	}
	if kind != "users" && kind != "groups" {
		scimFailure(w, &scimProblem{Status: 404, Detail: "Use the advertised SCIM Users, Groups or discovery endpoints."})
		return
	}
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" || r.Method == "DELETE" {
		response := &statusResponse{ResponseWriter: w}
		w = response
		defer func() {
			if response.status < 400 {
				return
			}
			target := id
			if len(target) > 128 || strings.ContainsAny(target, "\x00\r\n") {
				target = ""
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			if _, err := s.DB.Exec(ctx, "INSERT INTO audit_events(actor,action,subject,status) VALUES('SCIM',$1,$2,$3)", r.Method+" SCIM "+kind, target, response.status); err != nil {
				slog.Error("failed provisioning activity could not be persisted")
			}
		}()
	}
	if r.Method == "GET" && id == "" {
		s.listSCIMResources(w, r, kind)
		return
	}
	if r.Method == "GET" {
		s.getSCIMResource(w, r, kind, id)
		return
	}
	if (r.Method == "POST" && id != "") || (r.Method != "POST" && id == "") || (r.Method != "POST" && r.Method != "PUT" && r.Method != "PATCH" && r.Method != "DELETE") {
		scimFailure(w, &scimProblem{Status: 405, Detail: "Use POST on a collection, or PUT, PATCH or DELETE on an existing resource."})
		return
	}
	var document scimDocument
	var err error
	if r.Method != "DELETE" {
		document, err = readSCIMDocument(w, r)
		if err != nil {
			scimFailure(w, err)
			return
		}
	}
	tx, err := s.identityTransaction(r.Context())
	if err != nil {
		scimFailure(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err = scimMutation(r.Context(), tx); err != nil {
		scimFailure(w, err)
		return
	}
	var value scimDocument
	var version int64
	if kind == "users" {
		value, version, id, err = s.writeSCIMUser(r, tx, id, document)
	} else {
		value, version, id, err = s.writeSCIMGroup(r, tx, id, document)
	}
	if err == nil {
		err = syncSCIMMemberships(r.Context(), tx)
	}
	if err == nil {
		err = scimAudit(r.Context(), tx, r.Method+" SCIM "+kind, id)
	}
	if err == nil && value != nil {
		value, err = projectSCIMResource(value, r.URL.Query())
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		scimFailure(w, err)
		return
	}
	status := 200
	if r.Method == "POST" {
		status = 201
		w.Header().Set("Location", s.PublicURL+"/scim/v2/"+strings.ToUpper(kind[:1])+kind[1:]+"/"+url.PathEscape(id))
	}
	if r.Method == "DELETE" || kind == "groups" && r.Method == "PATCH" && r.URL.Query().Get("attributes") == "" {
		status = 204
	}
	if version > 0 {
		w.Header().Set("ETag", scimETag(version))
	}
	scimReply(w, status, value)
}

func (s *Server) getSCIMResource(w http.ResponseWriter, r *http.Request, kind, id string) {
	tx, err := s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		scimFailure(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var value scimDocument
	var version int64
	if kind == "users" {
		user, e := s.loadSCIMUser(r.Context(), tx, id, false, false)
		value, version, err = user.Value, user.Version, e
	} else {
		group, e := s.loadSCIMGroup(r.Context(), tx, id, false)
		value, version, err = group.Value, group.Version, e
	}
	if err == nil {
		value, err = projectSCIMResource(value, r.URL.Query())
	}
	if err != nil {
		scimFailure(w, err)
		return
	}
	w.Header().Set("ETag", scimETag(version))
	scimReply(w, 200, value)
}

func (s *Server) writeSCIMUser(r *http.Request, tx pgx.Tx, id string, document scimDocument) (scimDocument, int64, string, error) {
	ctx := r.Context()
	var previous scimUserRecord
	var err error
	if r.Method != "POST" {
		previous, err = s.loadSCIMUser(ctx, tx, id, true, false)
		if err == nil {
			err = checkSCIMVersion(r, previous.Version)
		}
		if err != nil {
			return nil, 0, id, err
		}
	}
	if r.Method == "DELETE" {
		return nil, 0, id, deleteSCIMUser(ctx, tx, previous)
	}
	if r.Method == "PATCH" {
		document, err = applySCIMPatch(previous.Value, document, "users")
		if err != nil {
			return nil, 0, id, err
		}
	}
	profile, err := validateSCIMUser(document)
	if err != nil {
		return nil, 0, id, err
	}
	if r.Method == "POST" {
		state := ctx.Value(scimContextKey{}).(scimState)
		id, err = s.createSCIMUser(ctx, tx, profile, state.Config.OIDCProvider)
	} else {
		err = updateSCIMUser(ctx, tx, previous, profile, false)
	}
	if err != nil {
		return nil, 0, id, err
	}
	user, err := s.loadSCIMUser(ctx, tx, id, false, false)
	return user.Value, user.Version, id, err
}

func (s *Server) writeSCIMGroup(r *http.Request, tx pgx.Tx, id string, document scimDocument) (scimDocument, int64, string, error) {
	ctx := r.Context()
	var previous scimGroupRecord
	var err error
	if r.Method != "POST" {
		previous, err = s.loadSCIMGroup(ctx, tx, id, true)
		if err == nil {
			err = checkSCIMVersion(r, previous.Version)
		}
		if err != nil {
			return nil, 0, id, err
		}
	}
	if r.Method == "DELETE" {
		return nil, 0, id, deleteSCIMGroup(ctx, tx, id)
	}
	if r.Method == "PATCH" {
		document, err = applySCIMPatch(previous.Value, document, "groups")
		if err != nil {
			return nil, 0, id, err
		}
	}
	id, err = saveSCIMGroup(ctx, tx, id, document, r.Method == "POST")
	if err != nil {
		return nil, 0, id, err
	}
	group, err := s.loadSCIMGroup(ctx, tx, id, false)
	return group.Value, group.Version, id, err
}
