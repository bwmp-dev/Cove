package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const scimUserSchema = "urn:ietf:params:scim:schemas:core:2.0:User"
const scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
const scimEnterpriseSchema = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
const scimPatchSchema = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
const scimListSchema = "urn:ietf:params:scim:api:messages:2.0:ListResponse"

type scimConfiguration struct {
	Token        string `json:"token"`
	OIDCProvider string `json:"oidc_provider_id"`
}

type scimState struct {
	Config    scimConfiguration
	Enabled   bool
	Version   int64
	RotatedAt *time.Time
}

type scimContextKey struct{}

type scimProblem struct {
	Status int
	Type   string
	Detail string
}

func (e *scimProblem) Error() string { return e.Detail }

func scimReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/scim+json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func scimFailure(w http.ResponseWriter, err error) {
	problem := &scimProblem{Status: 503, Detail: "Provisioning is unavailable; no changes were committed."}
	var p *scimProblem
	var postgres *pgconn.PgError
	if errors.As(err, &p) {
		problem = p
	} else if errors.As(err, &postgres) && postgres.Code == "23505" {
		problem = &scimProblem{Status: 409, Type: "uniqueness", Detail: "The user name, email or external identifier is already assigned. Existing Cove accounts require an explicit administrator link."}
	}
	body := map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:Error"}, "status": strconv.Itoa(problem.Status), "detail": problem.Detail}
	if problem.Type != "" {
		body["scimType"] = problem.Type
	}
	if problem.Status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Cove SCIM"`)
	}
	scimReply(w, problem.Status, body)
}

func loadSCIMState(ctx context.Context, tx pgx.Tx, key []byte) (scimState, error) {
	var state scimState
	var raw []byte
	err := tx.QueryRow(ctx, "SELECT enabled,configuration,rotated_at,version FROM scim_settings WHERE id=1").Scan(&state.Enabled, &raw, &state.RotatedAt, &state.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	raw, err = unseal(key, raw)
	if err == nil {
		err = json.Unmarshal(raw, &state.Config)
	}
	return state, err
}

func (s *Server) scimSettings(w http.ResponseWriter, r *http.Request) {
	var tx pgx.Tx
	var err error
	if r.Method == "GET" {
		tx, err = s.DB.Begin(r.Context())
	} else {
		tx, err = s.identityTransaction(r.Context())
	}
	if err != nil {
		http.Error(w, "provisioning settings unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	state, err := loadSCIMState(r.Context(), tx, s.Key)
	if err != nil {
		http.Error(w, "provisioning settings unavailable", 503)
		return
	}
	if r.Method == "GET" {
		respond(w, 200, map[string]any{"enabled": state.Enabled, "token_configured": state.Config.Token != "", "oidc_provider_id": state.Config.OIDCProvider, "rotated_at": state.RotatedAt, "version": state.Version, "base_url": s.PublicURL + "/scim/v2"})
		return
	}
	var in scimSettingsInput
	if !decode(w, r, &in) {
		return
	}
	if state.Version != in.Version {
		http.Error(w, "provisioning settings changed; reload before saving", 409)
		return
	}
	rotate := strings.HasSuffix(r.URL.Path, "/token")
	if rotate {
		if !in.Confirm {
			http.Error(w, "confirm rotation; the previous provisioning token stops working immediately", 400)
			return
		}
		state.Config.Token = randomToken()
		now := time.Now().UTC()
		state.RotatedAt = &now
	} else {
		if in.Enabled && state.Config.Token == "" {
			http.Error(w, "generate a provisioning token before enabling SCIM", 409)
			return
		}
		if in.OIDCProvider != "" {
			var exists bool
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM oidc_providers WHERE id=$1)", in.OIDCProvider).Scan(&exists); err != nil {
				http.Error(w, "provisioning settings unavailable", 503)
				return
			}
			if !exists {
				http.Error(w, "select an existing OIDC provider", 400)
				return
			}
		}
		state.Enabled, state.Config.OIDCProvider = in.Enabled, in.OIDCProvider
	}
	raw, err := json.Marshal(state.Config)
	if err == nil {
		raw, err = seal(s.Key, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO scim_settings(id,enabled,configuration,rotated_at,version) VALUES(1,$1,$2,$3,1)
 ON CONFLICT(id) DO UPDATE SET enabled=EXCLUDED.enabled,configuration=EXCLUDED.configuration,rotated_at=EXCLUDED.rotated_at,version=scim_settings.version+1`, state.Enabled, raw, state.RotatedAt)
	}
	action := "SCIM settings updated"
	if rotate {
		action = "SCIM token rotated"
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES($1,$2,'instance',200)", actor(r).ID, action)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save provisioning settings", 503)
		return
	}
	response := map[string]any{"ok": true, "version": state.Version + 1}
	if rotate {
		response["token"] = state.Config.Token
	}
	respond(w, 200, response)
}

func (s *Server) scimAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			scimFailure(w, err)
			return
		}
		state, err := loadSCIMState(r.Context(), tx, s.Key)
		tx.Rollback(r.Context())
		if err != nil {
			scimFailure(w, err)
			return
		}
		fields := strings.Fields(r.Header.Get("Authorization"))
		credential := ""
		if len(r.Header.Values("Authorization")) == 1 && len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
			credential = fields[1]
		}
		expected, actual := sha256.Sum256([]byte(state.Config.Token)), sha256.Sum256([]byte(credential))
		if !state.Enabled || state.Config.Token == "" || credential == "" || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			s.recordAudit(r, "SCIM authentication rejected", 401)
			scimFailure(w, &scimProblem{Status: 401, Detail: "A current instance provisioning token is required."})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scimContextKey{}, state)))
	})
}

func scimMutation(ctx context.Context, tx pgx.Tx) error {
	state, ok := ctx.Value(scimContextKey{}).(scimState)
	if !ok {
		return &scimProblem{Status: 401, Detail: "Provisioning authentication is required."}
	}
	var current bool
	err := tx.QueryRow(ctx, "SELECT enabled AND version=$1 FROM scim_settings WHERE id=1 FOR SHARE", state.Version).Scan(&current)
	if err != nil {
		return err
	}
	if !current {
		return &scimProblem{Status: 401, Detail: "The provisioning token or configuration changed; retry with the current token."}
	}
	return nil
}

type scimSettingsInput struct {
	Enabled      bool   `json:"enabled"`
	OIDCProvider string `json:"oidc_provider_id"`
	Version      int64  `json:"version"`
	Confirm      bool   `json:"confirm"`
}
