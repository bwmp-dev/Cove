package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) loadNodeProvider(ctx context.Context, id string) (nodeProvider, error) {
	provider := nodeProvider{ID: id}
	var secret []byte
	if err := s.DB.QueryRow(ctx, `SELECT kind,name,secret FROM node_providers WHERE id=$1`, id).Scan(&provider.Kind, &provider.Name, &secret); err != nil {
		return provider, err
	}
	data, err := unseal(s.Key, secret)
	if err != nil {
		return provider, err
	}
	provider.Token = string(data)
	provider.HasToken = provider.Token != ""
	return provider, nil
}

func (s *Server) nodeProviders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	if r.Method == "DELETE" {
		var active bool
		if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM node_provisions WHERE provider_id=$1 AND phase IN ('queued','creating','server_pending','enrolling','uncertain'))`, id).Scan(&active); err != nil {
			http.Error(w, "provisioning state unavailable", 503)
			return
		}
		if active {
			http.Error(w, "finish or explicitly abandon active provisioning before disconnecting its provider", 409)
			return
		}
		if _, err := s.DB.Exec(r.Context(), `DELETE FROM node_providers WHERE id=$1`, id); err != nil {
			http.Error(w, "provider could not be disconnected", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method != "GET" {
		var in struct {
			Kind  string `json:"kind"`
			Name  string `json:"name"`
			Token string `json:"token"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if providerOrigin(in.Kind) == "" || len(in.Name) == 0 || len(in.Name) > 120 || strings.ContainsAny(in.Name, "\x00\r\n") || len(in.Token) > 4096 || strings.ContainsAny(in.Token, " \t\r\n\x00") {
			http.Error(w, "select a supported provider, a name up to 120 characters and a single-line API token", 400)
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(1129272944)`); err != nil {
			http.Error(w, "provider settings busy", 503)
			return
		}
		if id == "" {
			var count int
			if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM node_providers`).Scan(&count); err != nil {
				http.Error(w, "providers unavailable", 503)
				return
			}
			if count >= 16 {
				http.Error(w, "at most 16 cloud provider connections are allowed", 409)
				return
			}
			id = resourceID()
		} else {
			var kind string
			var previous []byte
			if err = tx.QueryRow(r.Context(), `SELECT kind,secret FROM node_providers WHERE id=$1 FOR UPDATE`, id).Scan(&kind, &previous); errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			} else if err != nil {
				http.Error(w, "provider unavailable", 503)
				return
			}
			if kind != in.Kind {
				http.Error(w, "disconnect and create a new connection to change provider type", 409)
				return
			}
			if in.Token == "" {
				value, err := unseal(s.Key, previous)
				if err != nil {
					http.Error(w, "saved provider token unavailable", 503)
					return
				}
				in.Token = string(value)
			}
		}
		if len(in.Token) < 16 {
			http.Error(w, "an API token of 16–4096 characters is required", 400)
			return
		}
		secret, err := seal(s.Key, []byte(in.Token))
		if err != nil {
			http.Error(w, "provider token could not be protected", 503)
			return
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO node_providers(id,kind,name,secret) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,secret=EXCLUDED.secret`, id, in.Kind, in.Name, secret); err != nil {
			http.Error(w, "provider could not be saved", 503)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "provider save could not be confirmed", 503)
			return
		}
		respond(w, 200, map[string]string{"id": id})
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT id,kind,name FROM node_providers ORDER BY created_at LIMIT 16`)
	if err != nil {
		http.Error(w, "providers unavailable", 503)
		return
	}
	defer rows.Close()
	providers := []nodeProvider{}
	for rows.Next() {
		var provider nodeProvider
		if rows.Scan(&provider.ID, &provider.Kind, &provider.Name) != nil {
			http.Error(w, "provider unavailable", 503)
			return
		}
		provider.HasToken = true
		providers = append(providers, provider)
	}
	if rows.Err() != nil {
		http.Error(w, "providers unavailable", 503)
		return
	}
	respond(w, 200, providers)
}

func (s *Server) nodeProviderCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	provider, err := s.loadNodeProvider(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "provider credentials unavailable", 503)
		return
	}
	catalog, err := s.cloudCatalog(r.Context(), provider)
	if err != nil {
		http.Error(w, "provider catalogue unavailable; verify its token scopes and account", 502)
		return
	}
	respond(w, 200, catalog)
}

type nodeProvisionConfig struct {
	Name   string    `json:"name"`
	Region string    `json:"region"`
	Plan   string    `json:"plan"`
	Image  string    `json:"image"`
	Token  string    `json:"token,omitempty"`
	Cost   *nodeCost `json:"cost,omitempty"`
}

type nodeProvision struct {
	ID           string    `json:"id"`
	ProviderID   *string   `json:"provider_id"`
	ProviderKind string    `json:"provider_kind"`
	ProviderName string    `json:"provider_name"`
	NodeID       *string   `json:"node_id"`
	CloudID      string    `json:"server_id"`
	CloudName    string    `json:"server_name"`
	Phase        string    `json:"phase"`
	Error        string    `json:"last_error"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Deadline     time.Time `json:"deadline"`
}

func pruneNodeProvisions(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `DELETE FROM node_provisions WHERE id IN (SELECT id FROM node_provisions WHERE phase IN ('succeeded','failed','cancelled') AND updated_at<now()-interval '30 days' ORDER BY updated_at LIMIT 128); DELETE FROM node_provisions WHERE id IN (SELECT id FROM node_provisions WHERE phase IN ('succeeded','failed','cancelled') ORDER BY updated_at DESC OFFSET 255 LIMIT 128)`)
	return err
}

func (s *Server) nodeProvisions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "POST" {
		var in struct {
			ProviderID string    `json:"provider_id"`
			Name       string    `json:"name"`
			Region     string    `json:"region"`
			Plan       string    `json:"plan"`
			Image      string    `json:"image"`
			Confirm    bool      `json:"confirm"`
			Cost       *nodeCost `json:"cost,omitempty"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if !in.Confirm || len(in.Name) == 0 || len(in.Name) > 120 || strings.ContainsAny(in.Name, "\x00\r\n") || !providerValue.MatchString(in.Region) || !providerValue.MatchString(in.Plan) || !providerValue.MatchString(in.Image) || in.Cost != nil && !validNodeCost(*in.Cost) {
			http.Error(w, "confirm paid server creation with a name and catalogue region, plan and Linux image", 400)
			return
		}
		provider, err := s.loadNodeProvider(r.Context(), in.ProviderID)
		if err != nil {
			http.Error(w, "provider unavailable", 409)
			return
		}
		catalog, err := s.cloudCatalog(r.Context(), provider)
		if err != nil {
			http.Error(w, "verify the provider catalogue before creating a server", 502)
			return
		}
		contains := func(options []providerOption, id string) bool {
			for _, option := range options {
				if option.ID == id {
					return true
				}
			}
			return false
		}
		if !contains(catalog.Regions, in.Region) || !contains(catalog.Plans, in.Plan) || !contains(catalog.Images, in.Image) {
			http.Error(w, "select a region, plan and supported Linux image from the current provider catalogue", 409)
			return
		}
		for _, plan := range catalog.Plans {
			if plan.ID == in.Plan && len(plan.Regions) > 0 && !slices.Contains(plan.Regions, in.Region) {
				http.Error(w, "this plan is not offered in the selected region", 409)
				return
			}
		}
		config, err := json.Marshal(nodeProvisionConfig{Name: in.Name, Region: in.Region, Plan: in.Plan, Image: in.Image, Cost: in.Cost})
		if err != nil {
			http.Error(w, "invalid provisioning configuration", 400)
			return
		}
		secret, err := seal(s.Key, config)
		if err != nil {
			http.Error(w, "provisioning configuration could not be protected", 503)
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(1129272944)`); err != nil {
			http.Error(w, "provisioning busy", 503)
			return
		}
		if err = pruneNodeProvisions(r.Context(), tx); err != nil {
			http.Error(w, "provisioning history unavailable", 503)
			return
		}
		var active, total int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FILTER(WHERE phase IN ('queued','creating','server_pending','enrolling','uncertain')),count(*) FROM node_provisions`).Scan(&active, &total); err != nil {
			http.Error(w, "provisioning state unavailable", 503)
			return
		}
		if active >= 4 || total >= 260 {
			http.Error(w, "at most four unresolved server creations and 260 history records are retained; reconcile active requests first", 409)
			return
		}
		id := resourceID()
		if _, err = tx.Exec(r.Context(), `INSERT INTO node_provisions(id,provider_id,provider_kind,provider_name,team_id,cloud_name,configuration) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, provider.ID, provider.Kind, provider.Name, actor(r).Team, "cove-"+id, secret); err != nil {
			http.Error(w, "server creation could not be queued", 503)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "server request could not be confirmed; inspect history before retrying", 503)
			return
		}
		respond(w, 202, map[string]string{"id": id})
		return
	}
	if r.Method == "DELETE" {
		var in struct {
			Confirm bool `json:"confirm"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !in.Confirm {
			http.Error(w, "confirm abandoning tracking without deleting any cloud server", 400)
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var node *string
		err = tx.QueryRow(r.Context(), `UPDATE node_provisions SET phase='cancelled',last_error='Tracking was explicitly abandoned; no cloud server was deleted.',configuration='\x',updated_at=now() WHERE id=$1 AND phase IN ('queued','creating','server_pending','enrolling','uncertain') RETURNING node_id`, r.PathValue("id")).Scan(&node)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "this request is no longer active", 409)
			return
		}
		if err != nil {
			http.Error(w, "tracking cancellation unavailable", 503)
			return
		}
		if node != nil {
			if _, err = tx.Exec(r.Context(), `UPDATE enrollments SET consumed_at=now() WHERE node_id=$1 AND consumed_at IS NULL`, *node); err != nil {
				http.Error(w, "enrollment cancellation unavailable", 503)
				return
			}
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "tracking cancellation could not be confirmed", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT id,provider_id,provider_kind,provider_name,node_id,cloud_id,cloud_name,phase,last_error,created_at,updated_at,deadline FROM node_provisions ORDER BY created_at DESC LIMIT 260`)
	if err != nil {
		http.Error(w, "provisioning history unavailable", 503)
		return
	}
	defer rows.Close()
	items := []nodeProvision{}
	for rows.Next() {
		var item nodeProvision
		if rows.Scan(&item.ID, &item.ProviderID, &item.ProviderKind, &item.ProviderName, &item.NodeID, &item.CloudID, &item.CloudName, &item.Phase, &item.Error, &item.CreatedAt, &item.UpdatedAt, &item.Deadline) != nil {
			http.Error(w, "provisioning history unavailable", 503)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		http.Error(w, "provisioning history unavailable", 503)
		return
	}
	respond(w, 200, items)
}
