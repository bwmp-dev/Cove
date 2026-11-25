package control

import (
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var previewCIKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var previewCIDelivery = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var previewCIRevision = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

type previewCIEvent struct {
	Delivery string                       `json:"delivery_id"`
	Preview  string                       `json:"preview"`
	Sequence int64                        `json:"sequence"`
	Revision string                       `json:"revision"`
	Images   map[string]map[string]string `json:"images"`
}

func validatePreviewCIEvent(event previewCIEvent, bindings map[string]previewCIBinding) error {
	if !previewCIDelivery.MatchString(event.Delivery) || !previewCIKey.MatchString(event.Preview) || strings.Contains(event.Preview, "..") || event.Sequence < 1 || event.Sequence > 9007199254740991 || !previewCIRevision.MatchString(event.Revision) {
		return errors.New("use a delivery ID, preview key of 1-64 characters, positive safe-integer sequence and full lowercase Git commit digest")
	}
	return validatePreviewCIImages(event.Images, bindings)
}

func validatePreviewCIImages(images map[string]map[string]string, bindings map[string]previewCIBinding) error {
	if len(bindings) == 0 || len(bindings) > previewCIMaxResources || len(images) != len(bindings) {
		return errors.New("submit every configured resource and every service together; partial submissions are refused")
	}
	for id, binding := range bindings {
		pins := images[id]
		if len(binding.Services) == 0 || len(binding.Services) > 64 || len(pins) != len(binding.Services) {
			return errors.New("submit every configured resource and every service together; partial submissions are refused")
		}
		known := map[string]bool{}
		for _, service := range binding.Services {
			if !worker.ValidServiceName(service) || known[service] {
				return errors.New("CI service scope is invalid")
			}
			known[service] = true
			if pins[service] == "" {
				return errors.New("submit every configured resource and every service together; partial submissions are refused")
			}
		}
		if err := worker.ValidatePreviewRegistryImages(pins, binding.Server); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) previewCIReceive(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	if bearer(r) == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-preview-recipe:'||$1,0))`, project); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var team string
	var raw []byte
	var boundVersion int64
	err = tx.QueryRow(r.Context(), `SELECT c.team_id,c.bindings,c.recipe_version FROM preview_ci_credentials c JOIN projects p ON p.id=c.project_id AND p.team_id=c.team_id JOIN users u ON u.id=c.user_id JOIN preview_recipes v ON v.project_id=p.id AND v.team_id=c.team_id WHERE c.project_id=$1 AND c.token_hash=$2 AND c.expires_at>now() AND cove_automation_allowed(u.id,c.team_id,p.id,v.base_environment_id,'edit_configuration') AND (u.is_admin OR cove_access(u.id,c.team_id,p.id,v.base_environment_id)>=3) FOR UPDATE OF c`, project, tokenHash(bearer(r))).Scan(&team, &raw, &boundVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	var bindings map[string]previewCIBinding
	if json.Unmarshal(raw, &bindings) != nil {
		http.Error(w, "CI scope unavailable", 503)
		return
	}
	recipe, version, saved, err := s.loadPreviewRecipe(r.Context(), tx, project)
	if err != nil {
		http.Error(w, "preview recipe unavailable", 503)
		return
	}
	if !saved || !recipe.Enabled || version != boundVersion {
		http.Error(w, "preview recipe changed or is disabled; issue a replacement CI credential", 409)
		return
	}
	mediaType, _, mediaError := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaError != nil || mediaType != "application/json" {
		http.Error(w, "application/json is required", 415)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
	decoder.DisallowUnknownFields()
	var event previewCIEvent
	if decoder.Decode(&event) != nil {
		http.Error(w, "invalid or oversized CI image submission", 400)
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		http.Error(w, "submit exactly one JSON object", 400)
		return
	}
	if err = validatePreviewCIEvent(event, bindings); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fingerprint := previewCIHash(event)
	var previous, id string
	err = tx.QueryRow(r.Context(), `SELECT fingerprint,COALESCE(preview_id,'') FROM preview_ci_deliveries WHERE project_id=$1 AND delivery_id=$2`, project, event.Delivery).Scan(&previous, &id)
	if err == nil {
		if previous != fingerprint {
			http.Error(w, "delivery ID was already used for different inputs", 409)
			return
		}
		respond(w, 202, map[string]any{"preview_id": id, "duplicate": true})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "replay ledger unavailable", 503)
		return
	}
	// A maintenance inventory and newly accepted desired pins must observe one another.
	rows, err := tx.Query(r.Context(), `SELECT state FROM managed_registries WHERE team_id=$1 ORDER BY id FOR SHARE`, team)
	if err != nil {
		http.Error(w, "registry state unavailable", 503)
		return
	}
	busy := false
	for rows.Next() {
		var state string
		if err = rows.Scan(&state); err != nil {
			break
		}
		busy = busy || state == "maintenance" || state == "removing"
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		http.Error(w, "registry state unavailable", 503)
		return
	}
	if busy {
		http.Error(w, "managed registry maintenance is active; retry the submission later", 409)
		return
	}
	for resource, binding := range bindings {
		current, _, problem := s.previewCIBinding(r.Context(), tx, team, recipe.BaseEnvironment, resource, recipe)
		if problem != nil || previewCIHash(binding) != previewCIHash(current) {
			http.Error(w, "resource configuration or connected registry changed; issue a replacement CI credential", 409)
			return
		}
	}
	var sequence int64
	var delivery, slotFingerprint string
	err = tx.QueryRow(r.Context(), `SELECT sequence,COALESCE(preview_id,''),delivery_id,fingerprint FROM preview_ci_slots WHERE project_id=$1 AND suffix=$2 FOR UPDATE`, project, event.Preview).Scan(&sequence, &id, &delivery, &slotFingerprint)
	if err == nil {
		if id == "" {
			http.Error(w, "this preview key was closed; use a new preview key", 410)
			return
		}
		p, problem := s.loadPreviewRow(r.Context(), tx, id, true)
		if problem != nil {
			http.Error(w, "preview unavailable", 503)
			return
		}
		if p.State == "deleting" {
			http.Error(w, "this preview key is closing; use a new preview key", 410)
			return
		}
		if event.Sequence <= sequence {
			if event.Sequence == sequence && delivery == event.Delivery && slotFingerprint == fingerprint {
				respond(w, 202, map[string]any{"preview_id": id, "duplicate": true})
				return
			}
			http.Error(w, "stale or conflicting preview sequence", 409)
			return
		}
		if p.Snapshot.CI == nil || p.Snapshot.CI.RecipeVersion != boundVersion || previewCIHash(p.Snapshot.CI.Bindings) != previewCIHash(bindings) {
			http.Error(w, "the preview's frozen scope differs; use a new preview key", 409)
			return
		}
		var active bool
		if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations o JOIN environment_preview_resources v ON v.resource_id=o.workload_id WHERE v.preview_id=$1 AND o.state IN ('queued','running'))`, id).Scan(&active) != nil {
			http.Error(w, "preview admission unavailable", 503)
			return
		}
		if active || (p.State != "ready" && p.State != "failed" && p.State != "awaiting_approval") {
			http.Error(w, "wait for the current preview phase and operations before submitting new inputs; retry this delivery later", 409)
			return
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		var slots int
		if tx.QueryRow(r.Context(), `SELECT count(*) FROM preview_ci_slots WHERE project_id=$1`, project).Scan(&slots) != nil {
			http.Error(w, "replay ledger unavailable", 503)
			return
		}
		if slots >= previewCIMaxSlots {
			http.Error(w, "project CI preview-key history is full; existing keys can still be updated", 409)
			return
		}
		var existing bool
		if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environment_previews WHERE project_id=$1 AND suffix=$2)`, project, event.Preview).Scan(&existing) != nil {
			http.Error(w, "preview unavailable", 503)
			return
		}
		if existing {
			http.Error(w, "preview key already belongs to another workflow", 409)
			return
		}
		var active int
		if tx.QueryRow(r.Context(), `SELECT count(*) FROM environment_previews WHERE project_id=$1 AND state<>'deleting'`, project).Scan(&active) != nil {
			http.Error(w, "preview admission unavailable", 503)
			return
		}
		if active >= recipe.MaxPreviews {
			http.Error(w, "the preview recipe's active-preview limit is reached", 409)
			return
		}
		var created bool
		id, created, err = s.createPreviewRecord(r.Context(), tx, project, recipe, version, event.Preview, nil, "")
		if err != nil || !created {
			http.Error(w, "could not create the preview", 503)
			return
		}
	} else {
		http.Error(w, "replay ledger unavailable", 503)
		return
	}
	p, err := s.loadPreviewRow(r.Context(), tx, id, true)
	if err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	inputs := &previewCIInputs{Revision: event.Revision, RecipeVersion: boundVersion, Bindings: bindings, Images: event.Images}
	inputs.Identity = previewCIInputIdentity(event.Revision, event.Images)
	previousIdentity := ""
	if p.Snapshot.CI != nil {
		previousIdentity = p.Snapshot.CI.Identity
	}
	p.Snapshot.CI = inputs
	encrypted, err := s.sealJSON(p.Snapshot)
	if err != nil {
		http.Error(w, "could not protect CI inputs", 503)
		return
	}
	state, stage := p.State, p.Stage
	detail := p.Detail
	if previousIdentity != inputs.Identity {
		detail = ""
		if p.Environment != "" {
			state = "creating"
			if stage != "data" && stage != "connect" {
				state, stage = "updating", "deploy"
			}
		} else {
			state, stage = "creating", "plan"
		}
		if p.Snapshot.Recipe.RequireApproval {
			state, stage = "awaiting_approval", "approval"
		}
	}
	_, err = tx.Exec(r.Context(), `UPDATE environment_previews SET configuration=$2,state=$3,stage=$4,detail=$5,last_activity_at=now(),next_attempt_at=now() WHERE id=$1`, id, encrypted, state, stage, detail)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO preview_ci_slots(project_id,suffix,sequence,preview_id,delivery_id,fingerprint) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(project_id,suffix) DO UPDATE SET sequence=EXCLUDED.sequence,preview_id=EXCLUDED.preview_id,delivery_id=EXCLUDED.delivery_id,fingerprint=EXCLUDED.fingerprint`, project, event.Preview, event.Sequence, id, event.Delivery, fingerprint)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `DELETE FROM preview_ci_deliveries WHERE project_id=$1 AND delivery_id IN (SELECT delivery_id FROM preview_ci_deliveries WHERE project_id=$1 ORDER BY received_at DESC,delivery_id OFFSET $2)`, project, previewCIMaxDeliveries-1)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO preview_ci_deliveries(project_id,delivery_id,fingerprint,preview_id) VALUES($1,$2,$3,$4)`, project, event.Delivery, fingerprint, id)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE preview_ci_credentials SET last_received_at=$2 WHERE project_id=$1`, project, time.Now())
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES('CI preview credential','preview image submission',$1,202,$2)`, id, team)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not accept CI inputs", 503)
		return
	}
	respond(w, 202, map[string]any{"preview_id": id, "duplicate": false})
}
