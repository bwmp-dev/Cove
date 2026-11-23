package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

var errCertificateTXTUncertain = errors.New("the DNS challenge may have been created; keep its saved ownership proof and reconcile before retrying")

const certificateDNSIntentLimit = 128

type certificateTXT struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
}

func validateCertificateTXT(zone cloudflareZone, record certificateTXT) error {
	base := strings.TrimPrefix(record.Name, "_acme-challenge.")
	value, err := base64.RawURLEncoding.DecodeString(record.Content)
	if !cloudflareID.MatchString(zone.ZoneID) || !validDNSName(zone.Domain) || !strings.HasPrefix(record.Name, "_acme-challenge.") || !validDNSName(base) || (base != zone.Domain && !strings.HasSuffix(base, "."+zone.Domain)) || err != nil || len(value) != 32 || base64.RawURLEncoding.EncodeToString(value) != record.Content || !strings.HasPrefix(record.Comment, "cove-acme-") || !cloudflareID.MatchString(strings.TrimPrefix(record.Comment, "cove-acme-")) || (record.ID != "" && !cloudflareID.MatchString(record.ID)) {
		return errors.New("invalid DNS challenge ownership proof")
	}
	return nil
}

func (s *Server) findCertificateTXT(ctx context.Context, zone cloudflareZone, expected certificateTXT) (*certificateTXT, error) {
	if err := validateCertificateTXT(zone, expected); err != nil {
		return nil, err
	}
	var found *certificateTXT
	for page := 1; page <= 64; page++ {
		query := url.Values{"type": {"TXT"}, "name": {expected.Name}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var records []struct {
			certificateTXT
			Type string `json:"type"`
		}
		if err := s.cloudflareRequest(ctx, zone, "GET", "/dns_records?"+query.Encode(), nil, &records); err != nil {
			return nil, err
		}
		for _, record := range records {
			if record.Comment != expected.Comment {
				continue
			}
			if record.Type != "TXT" || record.Name != expected.Name || record.Content != expected.Content || !cloudflareID.MatchString(record.ID) || (expected.ID != "" && record.ID != expected.ID) || found != nil {
				return nil, errors.New("DNS challenge ownership changed or is duplicated")
			}
			copy := record.certificateTXT
			found = &copy
		}
		if len(records) < 100 {
			return found, nil
		}
	}
	return nil, errors.New("too many DNS challenge records to verify ownership")
}

func (s *Server) presentCertificateTXT(ctx context.Context, zone cloudflareZone, intent certificateTXT, attempted bool) (*certificateTXT, error) {
	found, err := s.findCertificateTXT(ctx, zone, intent)
	if err != nil || found != nil {
		return found, err
	}
	// A lost create response is not evidence that no record exists, even after an empty list read.
	if attempted || intent.ID != "" {
		return nil, errCertificateTXTUncertain
	}
	var created struct {
		certificateTXT
		Type string `json:"type"`
	}
	body := map[string]any{"type": "TXT", "name": intent.Name, "content": intent.Content, "comment": intent.Comment, "ttl": 60}
	if err := s.cloudflareRequest(ctx, zone, "POST", "/dns_records", body, &created); err != nil {
		var status cloudflareStatusError
		if errors.As(err, &status) && status >= 400 && status < 500 && status != 408 {
			return nil, err
		}
		return nil, errCertificateTXTUncertain
	}
	if !cloudflareID.MatchString(created.ID) || created.Type != "TXT" || created.Name != intent.Name || created.Content != intent.Content || created.Comment != intent.Comment {
		return nil, errCertificateTXTUncertain
	}
	return &created.certificateTXT, nil
}

func (s *Server) removeCertificateTXT(ctx context.Context, zone cloudflareZone, proof certificateTXT) error {
	if proof.ID == "" {
		return errCertificateTXTUncertain
	}
	found, err := s.findCertificateTXT(ctx, zone, proof)
	if err != nil || found == nil {
		return err
	}
	if err := s.cloudflareRequest(ctx, zone, "DELETE", "/dns_records/"+proof.ID, nil, nil); err != nil {
		return fmt.Errorf("could not remove the owned DNS challenge: %w", err)
	}
	return nil
}

type certificateDNSIntentState struct {
	Record    certificateTXT `json:"record"`
	Attempted bool           `json:"attempted"`
}

func (s *Server) certificateDNSChallenges(w http.ResponseWriter, r *http.Request) {
	workload, zone := r.PathValue("id"), r.PathValue("zone")
	if workload == "" {
		var manage bool
		err := s.DB.QueryRow(r.Context(), `SELECT COALESCE($2 OR owner_id=$3,false) FROM cloudflare_zones WHERE zone_id=$1`, zone, actor(r).Admin, actor(r).ID).Scan(&manage)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "DNS challenge status unavailable", 503)
			return
		}
		if !manage {
			http.Error(w, "connection owner access required", 403)
			return
		}
	}
	rows, err := s.DB.Query(r.Context(), `SELECT id,issuer_id,workload_id,zone_id,state,phase,cleanup_requested,last_error,next_check_at,created_at FROM route_certificate_dns_intents WHERE phase<>'cleaned' AND (($1<>'' AND workload_id=$1) OR ($2<>'' AND zone_id=$2)) ORDER BY created_at,id LIMIT $3`, workload, zone, certificateDNSIntentLimit+1)
	if err != nil {
		http.Error(w, "DNS challenge status unavailable", 503)
		return
	}
	defer rows.Close()
	type challenge struct {
		ID               string     `json:"id"`
		IssuerID         *string    `json:"issuer_id"`
		WorkloadID       *string    `json:"workload_id"`
		ZoneID           string     `json:"zone_id"`
		Hostname         string     `json:"hostname"`
		Phase            string     `json:"phase"`
		CleanupRequested bool       `json:"cleanup_requested"`
		LastError        string     `json:"last_error"`
		NextCheckAt      *time.Time `json:"next_check_at"`
		CreatedAt        time.Time  `json:"created_at"`
	}
	items := []challenge{}
	more := false
	for rows.Next() {
		if len(items) == certificateDNSIntentLimit {
			more = true
			break
		}
		var item challenge
		var encrypted []byte
		if err := rows.Scan(&item.ID, &item.IssuerID, &item.WorkloadID, &item.ZoneID, &encrypted, &item.Phase, &item.CleanupRequested, &item.LastError, &item.NextCheckAt, &item.CreatedAt); err != nil {
			http.Error(w, "DNS challenge status unavailable", 503)
			return
		}
		raw, err := unseal(s.Key, encrypted)
		var state certificateDNSIntentState
		if err != nil || json.Unmarshal(raw, &state) != nil {
			http.Error(w, "DNS challenge status unavailable", 503)
			return
		}
		item.Hostname = state.Record.Name
		items = append(items, item)
	}
	if rows.Err() != nil {
		http.Error(w, "DNS challenge status unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"items": items, "more": more})
}

func (s *Server) scheduleCertificateDNSIntents(ctx context.Context) error {
	// Failed requests may still reference a cleaned intent when an operator resumes them.
	if _, err := s.DB.Exec(ctx, `DELETE FROM route_certificate_dns_intents WHERE id IN (SELECT d.id FROM route_certificate_dns_intents d LEFT JOIN route_certificate_issuers i ON i.id=d.issuer_id WHERE d.phase='cleaned' AND d.updated_at<now()-interval '7 days' AND (d.issuer_id IS NULL OR i.phase='issued') ORDER BY d.updated_at,d.id LIMIT $1)`, certificateDNSIntentLimit); err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `UPDATE route_certificate_dns_intents SET next_check_at=now()+interval '1 minute' WHERE id IN (SELECT id FROM route_certificate_dns_intents WHERE next_check_at<=now() AND phase<>'cleaned' ORDER BY next_check_at LIMIT 2 FOR UPDATE SKIP LOCKED) RETURNING id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var jobs sync.WaitGroup
	failures := make(chan error, len(ids))
	for _, id := range ids {
		jobs.Go(func() {
			if err := s.processCertificateDNSIntent(ctx, id, false); err != nil {
				failures <- err
			}
		})
	}
	jobs.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	return nil
}

func (s *Server) processCertificateDNSIntent(ctx context.Context, id string, freshAttempt bool) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var issuer *string
	if err := tx.QueryRow(ctx, `SELECT issuer_id FROM route_certificate_dns_intents WHERE id=$1`, id).Scan(&issuer); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var workload, cycle, issuerPhase, zoneID string
	var domains []string
	var acmeState []byte
	if issuer != nil {
		// Issuer controllers take these locks in the same order before requesting cleanup.
		err = tx.QueryRow(ctx, `SELECT workload_id,zone_id,domains,phase,acme_state FROM route_certificate_issuers WHERE id=$1 FOR SHARE`, *issuer).Scan(&workload, &zoneID, &domains, &issuerPhase, &acmeState)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			issuer = nil
		}
	}
	var encrypted []byte
	var cleanup bool
	var phase string
	err = tx.QueryRow(ctx, `SELECT zone_id,cycle_id,state,cleanup_requested,phase FROM route_certificate_dns_intents WHERE id=$1 FOR UPDATE`, id).Scan(&zoneID, &cycle, &encrypted, &cleanup, &phase)
	if errors.Is(err, pgx.ErrNoRows) || phase == "cleaned" {
		return nil
	}
	if err != nil {
		return err
	}
	var state certificateDNSIntentState
	raw, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(raw, &state) != nil {
		return errors.New("stored DNS challenge ownership proof is unreadable")
	}
	soon := time.Now().Add(time.Minute)
	save := func(nextPhase, message string) error {
		sealed, err := s.sealJSON(state)
		if err != nil {
			return err
		}
		var due *time.Time
		if nextPhase != "cleaned" && nextPhase != "blocked" {
			due = &soon
		}
		if _, err := tx.Exec(ctx, `UPDATE route_certificate_dns_intents SET state=$2,phase=$3,last_error=$4,cleanup_requested=$5,next_check_at=$6,updated_at=now() WHERE id=$1`, id, sealed, nextPhase, message, cleanup, due); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if issuer == nil {
		cleanup = true
	} else {
		var pending struct {
			CycleID  string `json:"cycle_id"`
			IntentID string `json:"intent_id"`
		}
		decoded, err := unseal(s.Key, acmeState)
		if err != nil || json.Unmarshal(decoded, &pending) != nil {
			cleanup = true
		} else if pending.CycleID != cycle || pending.IntentID != id || issuerPhase == "issued" || issuerPhase == "failed" {
			cleanup = true
		}
	}
	var zone cloudflareZone
	if err := tx.QueryRow(ctx, `SELECT configuration FROM cloudflare_zones WHERE zone_id=$1 FOR SHARE`, zoneID).Scan(&encrypted); err != nil {
		return err
	}
	raw, err = unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(raw, &zone) != nil || zone.ZoneID != zoneID || validateCertificateTXT(zone, state.Record) != nil {
		return errors.New("stored DNS challenge zone or ownership proof is invalid")
	}
	if !cleanup {
		if _, err := s.issuerZone(ctx, tx, workload, zoneID, domains); err != nil {
			cleanup = true
		}
	}
	if cleanup && freshAttempt && state.Record.ID == "" {
		state.Attempted = false
	}
	if cleanup && !state.Attempted && state.Record.ID == "" {
		return save("cleaned", "")
	}
	if zone.ConnectionID != "" {
		token, err := s.cloudflareConnectionToken(ctx, zone.ConnectionID)
		if err != nil {
			if freshAttempt {
				state.Attempted = false
			}
			if cleanup {
				return save("cleanup", "Cloudflare credentials are unavailable; reconnect to finish the owned DNS challenge cleanup")
			}
			return save(phase, "Cloudflare credentials are unavailable; reconnect to continue the saved DNS challenge or its cleanup")
		}
		zone.Token, zone.ConnectionID = token, ""
	}
	if cleanup {
		found, err := s.findCertificateTXT(ctx, zone, state.Record)
		if err != nil {
			return save("cleanup", "Could not verify the owned DNS challenge; cleanup will retry without touching other records")
		}
		if found != nil {
			if state.Record.ID == "" {
				state.Record = *found
				// Persist the recovered provider ID before attempting a destructive request.
				return save("cleanup", "")
			}
			if err := s.removeCertificateTXT(ctx, zone, state.Record); err != nil {
				return save("cleanup", "Could not remove the owned DNS challenge; cleanup will retry")
			}
		} else if state.Record.ID == "" {
			soon = time.Now().Add(time.Hour)
			return save("cleanup", "An earlier DNS create outcome remains unknown; its ownership proof is retained for later cleanup")
		}
		return save("cleaned", "")
	}
	if !state.Attempted {
		state.Attempted = true
		if err := save("presenting", ""); err != nil {
			return err
		}
		return s.processCertificateDNSIntent(ctx, id, true)
	}
	record, err := s.presentCertificateTXT(ctx, zone, state.Record, !freshAttempt)
	if err != nil || record == nil {
		var status cloudflareStatusError
		if freshAttempt && err != nil && !errors.Is(err, errCertificateTXTUncertain) {
			state.Attempted = false
			if errors.As(err, &status) && status >= 400 && status < 500 && status != 408 {
				return save("blocked", "Cloudflare refused the DNS challenge; review DNS Edit permission, record conflicts and provider rate limits")
			}
			return save("pending", "Cloudflare was unavailable before any DNS record was created; preparation will retry")
		}
		return save("presenting", "DNS challenge creation is not confirmed; Cove will reconcile its saved ownership proof before proceeding")
	}
	state.Record = *record
	return save("present", "")
}
