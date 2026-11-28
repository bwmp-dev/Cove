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

type certificateZone struct {
	ID      string   `json:"zone_id"`
	Domain  string   `json:"domain"`
	Domains []string `json:"domains"`
}

type certificateIssuerAccepted struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
}

type certificateIssuerInput struct {
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	ZoneID       string   `json:"zone_id"`
	Domains      []string `json:"domains"`
	ValidityDays int      `json:"validity_days"`
	AutoRenew    *bool    `json:"auto_renew"`
	TermsURL     string   `json:"terms_url"`
	AcceptTerms  bool     `json:"accept_terms"`
}

type certificateIssuerUpdate struct {
	AutoRenew *bool `json:"auto_renew"`
	Retry     bool  `json:"retry"`
}

type certificateIssuer struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Provider        string     `json:"provider"`
	ZoneID          string     `json:"zone_id"`
	Domains         []string   `json:"domains"`
	ValidityDays    int        `json:"validity_days"`
	AutoRenew       bool       `json:"auto_renew"`
	Phase           string     `json:"phase"`
	CertificateID   string     `json:"certificate_id"`
	OperationID     string     `json:"operation_id"`
	LastError       string     `json:"last_error"`
	NextCheckAt     *time.Time `json:"next_check_at"`
	Directory       string     `json:"directory,omitempty"`
	TermsURL        string     `json:"terms_url,omitempty"`
	TermsText       string     `json:"terms_text,omitempty"`
	TermsAcceptedAt *time.Time `json:"terms_accepted_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type certificateIssuerState struct {
	CSR                   []byte `json:"csr"`
	PrivateKeyPEM         string `json:"private_key_pem"`
	Attempted             bool   `json:"attempted"`
	CandidateID           string `json:"candidate_id"`
	PreviousCertificateID string `json:"previous_certificate_id,omitempty"`
}

func (s *Server) issuerZone(ctx context.Context, tx pgx.Tx, workload, zone string, domains []string) (cloudflareZone, error) {
	var raw, configuration []byte
	var patterns []string
	var selected cloudflareZone
	if err := tx.QueryRow(ctx, `SELECT z.configuration,g.domains,w.configuration FROM cloudflare_zones z JOIN cloudflare_zone_teams g ON g.zone_id=z.zone_id JOIN workloads w ON w.team_id=g.team_id WHERE w.id=$1 AND z.zone_id=$2 AND cove_account_active(z.owner_id) FOR SHARE OF z,g,w`, workload, zone).Scan(&raw, &patterns, &configuration); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return selected, errors.New("share this Cloudflare zone and the requested hostnames with the resource's team before issuing a certificate")
		}
		return selected, err
	}
	data, err := unseal(s.Key, raw)
	if err != nil || json.Unmarshal(data, &selected) != nil || selected.ZoneID != zone || !selected.Enabled {
		return selected, errors.New("Cloudflare zone is unavailable or disabled")
	}
	plain, err := unseal(s.Key, configuration)
	var config workloadInput
	if err != nil || json.Unmarshal(plain, &config) != nil {
		return selected, errors.New("certificate resource configuration is unavailable")
	}
	for _, domain := range domains {
		for _, route := range config.Routes {
			if !route.IsPublicHTTPRoute() && worker.RouteDomainsOverlap(route.Domain, domain) {
				return selected, errors.New("private hostnames cannot be submitted to public certificate authorities; upload a private certificate instead")
			}
		}
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
	list := []certificateZone{}
	for rows.Next() {
		var item certificateZone
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
		rows, err := s.DB.Query(r.Context(), `SELECT id,name,provider,zone_id,domains,validity_days,auto_renew,phase,COALESCE(certificate_id,''),operation_id,last_error,next_check_at,created_at,acme_state FROM route_certificate_issuers WHERE workload_id=$1 ORDER BY created_at DESC,id`, workload)
		if err != nil {
			http.Error(w, "certificate issuers unavailable", 503)
			return
		}
		defer rows.Close()
		list := []certificateIssuer{}
		for rows.Next() {
			var item certificateIssuer
			var acmeState []byte
			if rows.Scan(&item.ID, &item.Name, &item.Provider, &item.ZoneID, &item.Domains, &item.ValidityDays, &item.AutoRenew, &item.Phase, &item.CertificateID, &item.OperationID, &item.LastError, &item.NextCheckAt, &item.CreatedAt, &acmeState) != nil {
				http.Error(w, "certificate issuers unavailable", 503)
				return
			}
			if item.Provider == "acme_dns" {
				raw, err := unseal(s.Key, acmeState)
				var pending certificateACMEIssuerState
				if err != nil || json.Unmarshal(raw, &pending) != nil {
					http.Error(w, "certificate authority metadata unavailable", 503)
					return
				}
				item.Directory, item.TermsURL = pending.Directory, pending.TermsURL
				item.TermsText, err = certificateTerms(pending.TermsURL)
				if err != nil {
					http.Error(w, "certificate authority metadata unavailable", 503)
					return
				}
				if !pending.TermsAcceptedAt.IsZero() {
					item.TermsAcceptedAt = &pending.TermsAcceptedAt
				}
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
	var input certificateIssuerInput
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 60 || strings.ContainsFunc(input.Name, func(r rune) bool { return r < 32 || r == 127 }) || !cloudflareID.MatchString(input.ZoneID) || input.AutoRenew == nil {
		http.Error(w, "a name up to 60 characters, connected Cloudflare zone, supported certificate provider and explicit auto_renew are required", 400)
		return
	}
	switch input.Provider {
	case "cloudflare_origin":
		if !slices.Contains(originCertificateValidityDays, input.ValidityDays) || input.AcceptTerms || input.TermsURL != "" {
			http.Error(w, "Origin CA requires a supported validity and no ACME terms fields", 400)
			return
		}
	case "acme_dns":
		if input.ValidityDays != 0 || !input.AcceptTerms {
			http.Error(w, "DNS-01 requires explicit acceptance of the authority's terms; its validity is chosen by the authority", 400)
			return
		}
	default:
		http.Error(w, "choose Cloudflare Origin CA or ACME DNS-01", 400)
		return
	}
	csr, key, err := worker.NewRouteCertificateKey(input.Domains)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var directory, terms string
	if input.Provider == "acme_dns" {
		lookup, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		directory, terms, err = s.discoverCertificateAuthority(lookup)
		cancel()
		if err != nil {
			http.Error(w, "configured certificate authority unavailable", 503)
			return
		}
		if terms != input.TermsURL {
			http.Error(w, "certificate authority terms changed; refresh and accept the current terms before requesting", 409)
			return
		}
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
	var acmeState []byte
	if input.Provider == "acme_dns" {
		acmeState, err = s.sealJSON(certificateACMEIssuerState{Directory: directory, TermsURL: terms, TermsAcceptedAt: time.Now().UTC(), TermsAcceptedBy: actor(r).ID, CycleID: state.CandidateID})
		if err != nil {
			http.Error(w, "could not protect certificate authority request", 503)
			return
		}
	}
	id := resourceID()
	if _, err = tx.Exec(r.Context(), `INSERT INTO route_certificate_issuers(id,workload_id,zone_id,name,provider,domains,validity_days,auto_renew,state,next_check_at,acme_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),$10)`, id, workload, input.ZoneID, input.Name, input.Provider, input.Domains, input.ValidityDays, *input.AutoRenew, sealed, acmeState); err != nil {
		http.Error(w, "could not save certificate issuer", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save certificate issuer", 503)
		return
	}
	respond(w, 202, certificateIssuerAccepted{ID: id, Phase: "requesting"})
}

func (s *Server) updateCertificateIssuer(w http.ResponseWriter, r *http.Request) {
	var input certificateIssuerUpdate
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
		_, err = tx.Exec(r.Context(), `UPDATE route_certificate_issuers SET auto_renew=COALESCE($2,auto_renew),next_check_at=CASE WHEN phase='issued' AND $2=false THEN NULL WHEN $3 OR phase='issued' AND $2=true THEN now() ELSE next_check_at END,phase=CASE WHEN $3 AND provider='acme_dns' AND phase='failed' AND operation_id='' THEN 'requesting' ELSE phase END,last_error=CASE WHEN $3 THEN '' ELSE last_error END,updated_at=now() WHERE id=$1`, r.PathValue("issuer"), input.AutoRenew, input.Retry)
		if err == nil && input.Retry {
			_, err = tx.Exec(r.Context(), `UPDATE route_certificate_dns_intents SET phase='pending',last_error='',next_check_at=now(),updated_at=now() WHERE issuer_id=$1 AND phase='blocked' AND NOT cleanup_requested`, r.PathValue("issuer"))
		}
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
