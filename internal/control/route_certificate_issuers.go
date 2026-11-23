package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type certificateIssuer struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Provider      string     `json:"provider"`
	ZoneID        string     `json:"zone_id"`
	Domains       []string   `json:"domains"`
	ValidityDays  int        `json:"validity_days"`
	AutoRenew     bool       `json:"auto_renew"`
	Phase         string     `json:"phase"`
	CertificateID string     `json:"certificate_id"`
	OperationID   string     `json:"operation_id"`
	LastError     string     `json:"last_error"`
	NextCheckAt   *time.Time `json:"next_check_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

type certificateIssuerState struct {
	CSR                   []byte `json:"csr"`
	PrivateKeyPEM         string `json:"private_key_pem"`
	Attempted             bool   `json:"attempted"`
	CandidateID           string `json:"candidate_id"`
	PreviousCertificateID string `json:"previous_certificate_id,omitempty"`
}

func (s *Server) issuerZone(ctx context.Context, tx pgx.Tx, workload, zone string, domains []string) (cloudflareZone, error) {
	var raw []byte
	var patterns []string
	var selected cloudflareZone
	if err := tx.QueryRow(ctx, `SELECT z.configuration,g.domains FROM cloudflare_zones z JOIN cloudflare_zone_teams g ON g.zone_id=z.zone_id JOIN workloads w ON w.team_id=g.team_id WHERE w.id=$1 AND z.zone_id=$2 FOR SHARE OF z,g,w`, workload, zone).Scan(&raw, &patterns); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return selected, errors.New("share this Cloudflare zone and the requested hostnames with the resource's team before issuing a certificate")
		}
		return selected, err
	}
	data, err := unseal(s.Key, raw)
	if err != nil || json.Unmarshal(data, &selected) != nil || selected.ZoneID != zone || !selected.Enabled {
		return selected, errors.New("Cloudflare zone is unavailable or disabled")
	}
	for _, domain := range domains {
		base := strings.TrimPrefix(domain, "*.")
		if !cloudflareDomainAllowed(patterns, domain) || (base != selected.Domain && !strings.HasSuffix(base, "."+selected.Domain)) {
			return selected, errors.New("every requested certificate hostname must be allowed by the team's Cloudflare zone share")
		}
	}
	return selected, nil
}

func (s *Server) certificateZones(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT z.zone_id,z.domain,g.domains FROM cloudflare_zones z JOIN cloudflare_zone_teams g ON g.zone_id=z.zone_id JOIN workloads w ON w.team_id=g.team_id WHERE w.id=$1 ORDER BY z.domain,z.zone_id`, r.PathValue("id"))
	if err != nil {
		http.Error(w, "certificate zones unavailable", 503)
		return
	}
	defer rows.Close()
	list := []struct {
		ID      string   `json:"zone_id"`
		Domain  string   `json:"domain"`
		Domains []string `json:"domains"`
	}{}
	for rows.Next() {
		var item struct {
			ID      string   `json:"zone_id"`
			Domain  string   `json:"domain"`
			Domains []string `json:"domains"`
		}
		if rows.Scan(&item.ID, &item.Domain, &item.Domains) != nil {
			http.Error(w, "certificate zones unavailable", 503)
			return
		}
		list = append(list, item)
	}
	if rows.Err() != nil {
		http.Error(w, "certificate zones unavailable", 503)
		return
	}
	respond(w, 200, list)
}

func (s *Server) certificateIssuers(w http.ResponseWriter, r *http.Request) {
	workload := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,name,provider,zone_id,domains,validity_days,auto_renew,phase,COALESCE(certificate_id,''),operation_id,last_error,next_check_at,created_at FROM route_certificate_issuers WHERE workload_id=$1 ORDER BY created_at DESC,id`, workload)
		if err != nil {
			http.Error(w, "certificate issuers unavailable", 503)
			return
		}
		defer rows.Close()
		list := []certificateIssuer{}
		for rows.Next() {
			var item certificateIssuer
			if rows.Scan(&item.ID, &item.Name, &item.Provider, &item.ZoneID, &item.Domains, &item.ValidityDays, &item.AutoRenew, &item.Phase, &item.CertificateID, &item.OperationID, &item.LastError, &item.NextCheckAt, &item.CreatedAt) != nil {
				http.Error(w, "certificate issuers unavailable", 503)
				return
			}
			list = append(list, item)
		}
		if rows.Err() != nil {
			http.Error(w, "certificate issuers unavailable", 503)
			return
		}
		respond(w, 200, list)
		return
	}
	var input struct {
		Name         string   `json:"name"`
		Provider     string   `json:"provider"`
		ZoneID       string   `json:"zone_id"`
		Domains      []string `json:"domains"`
		ValidityDays int      `json:"validity_days"`
		AutoRenew    *bool    `json:"auto_renew"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 60 || strings.ContainsFunc(input.Name, func(r rune) bool { return r < 32 || r == 127 }) || input.Provider != "cloudflare_origin" || !cloudflareID.MatchString(input.ZoneID) || input.AutoRenew == nil || !slices.Contains(originCertificateValidityDays, input.ValidityDays) {
		http.Error(w, "a name up to 60 characters, connected Cloudflare zone, Origin CA provider, supported validity and explicit auto_renew are required", 400)
		return
	}
	csr, key, err := worker.NewRouteCertificateKey(input.Domains)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	slices.Sort(input.Domains)
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "certificate issuer unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var eligible bool
	if err = tx.QueryRow(r.Context(), `SELECT kind='service' AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) FROM workloads w WHERE id=$1 FOR UPDATE`, workload).Scan(&eligible); err != nil || !eligible {
		http.Error(w, "certificate issuers require a service outside managed previews", 409)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM route_certificate_issuers WHERE workload_id=$1", workload).Scan(&count); err != nil || count >= 8 {
		http.Error(w, "a resource can have at most eight certificate issuers", 409)
		return
	}
	if _, err = s.issuerZone(r.Context(), tx, workload, input.ZoneID, input.Domains); err != nil {
		http.Error(w, "a connected, enabled Cloudflare zone must share every requested hostname with this team", 403)
		return
	}
	routes := make([]worker.Route, len(input.Domains))
	for i, domain := range input.Domains {
		routes[i].Domain = domain
	}
	if conflict, err := s.routeConflict(r.Context(), tx, workload, routes); err != nil {
		http.Error(w, "certificate hostname claims unavailable", 503)
		return
	} else if conflict != "" {
		http.Error(w, "a requested certificate hostname overlaps the panel or another resource", 409)
		return
	}
	state := certificateIssuerState{CSR: csr, PrivateKeyPEM: key, CandidateID: resourceID()}
	sealed, err := s.sealJSON(state)
	if err != nil {
		http.Error(w, "could not protect certificate request", 503)
		return
	}
	id := resourceID()
	if _, err = tx.Exec(r.Context(), `INSERT INTO route_certificate_issuers(id,workload_id,zone_id,name,provider,domains,validity_days,auto_renew,state,next_check_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now())`, id, workload, input.ZoneID, input.Name, input.Provider, input.Domains, input.ValidityDays, *input.AutoRenew, sealed); err != nil {
		http.Error(w, "could not save certificate issuer", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save certificate issuer", 503)
		return
	}
	respond(w, 202, map[string]string{"id": id, "phase": "requesting"})
}

func (s *Server) updateCertificateIssuer(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AutoRenew *bool `json:"auto_renew"`
		Retry     bool  `json:"retry"`
	}
	if r.Method != "DELETE" {
		if !decode(w, r, &input) {
			return
		}
		if input.AutoRenew == nil && !input.Retry {
			http.Error(w, "auto_renew or retry is required", 400)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "certificate issuer unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var phase, operation string
	err = tx.QueryRow(r.Context(), `SELECT phase,operation_id FROM route_certificate_issuers WHERE id=$1 AND workload_id=$2 FOR UPDATE`, r.PathValue("issuer"), r.PathValue("id")).Scan(&phase, &operation)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "certificate issuer unavailable", 503)
		return
	}
	if input.Retry && phase != "failed" && phase != "reconciling" {
		http.Error(w, "retry is available only after an issuance or route update failure", 409)
		return
	}
	if r.Method == "DELETE" {
		var active bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE id=$1 AND state IN ('queued','running'))`, operation).Scan(&active); err != nil || active {
			http.Error(w, "wait for this issuer's route update before removing it", 409)
			return
		}
		_, err = tx.Exec(r.Context(), "DELETE FROM route_certificate_issuers WHERE id=$1", r.PathValue("issuer"))
	} else {
		_, err = tx.Exec(r.Context(), `UPDATE route_certificate_issuers SET auto_renew=COALESCE($2,auto_renew),next_check_at=CASE WHEN phase='issued' AND $2=false THEN NULL WHEN $3 OR phase='issued' AND $2=true THEN now() ELSE next_check_at END,last_error=CASE WHEN $3 THEN '' ELSE last_error END,updated_at=now() WHERE id=$1`, r.PathValue("issuer"), input.AutoRenew, input.Retry)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not update certificate issuer", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
