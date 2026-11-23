package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/acme"
)

type certificateACMEIssuerState struct {
	Directory          string    `json:"directory"`
	TermsURL           string    `json:"terms_url"`
	TermsAcceptedAt    time.Time `json:"terms_accepted_at"`
	TermsAcceptedBy    string    `json:"terms_accepted_by"`
	CycleID            string    `json:"cycle_id"`
	OrderURI           string    `json:"order_uri,omitempty"`
	OrderAttempts      int       `json:"order_attempts"`
	AuthorizationIndex int       `json:"authorization_index"`
	IntentID           string    `json:"intent_id,omitempty"`
}

func (s *Server) processACMECertificateIssuer(ctx context.Context, tx pgx.Tx, workload string, item certificateIssuer, state certificateIssuerState, encrypted []byte, freshOrder bool) error {
	var pending certificateACMEIssuerState
	decoded, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(decoded, &pending) != nil || pending.Directory == "" || pending.TermsAcceptedAt.IsZero() || pending.TermsAcceptedBy == "" {
		return errors.New("stored DNS-01 certificate request is unreadable")
	}
	soon := time.Now().Add(3 * time.Second)
	save := func(phase, message string) error {
		sealed, err := s.sealJSON(pending)
		if err != nil {
			return err
		}
		var due *time.Time
		if phase != "failed" {
			due = &soon
		}
		if _, err := tx.Exec(ctx, `UPDATE route_certificate_issuers SET acme_state=$2,phase=$3,last_error=$4,next_check_at=$5,updated_at=now() WHERE id=$1`, item.ID, sealed, phase, message, due); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	cleanup := func() error {
		if pending.IntentID == "" {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE route_certificate_dns_intents SET cleanup_requested=true,next_check_at=now() WHERE id=$1 AND issuer_id=$2`, pending.IntentID, item.ID)
		return err
	}
	if pending.CycleID != state.CandidateID {
		if err := cleanup(); err != nil {
			return err
		}
		pending = certificateACMEIssuerState{Directory: pending.Directory, TermsURL: pending.TermsURL, TermsAcceptedAt: pending.TermsAcceptedAt, TermsAcceptedBy: pending.TermsAcceptedBy, CycleID: state.CandidateID}
		return save("requesting", "")
	}
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM workloads WHERE id=$1 FOR UPDATE`, workload).Scan(&locked); err != nil {
		return err
	}
	if _, err := s.issuerZone(ctx, tx, workload, item.ZoneID, item.Domains); err != nil {
		if err := cleanup(); err != nil {
			return err
		}
		return save("failed", "Cloudflare hostname sharing is unavailable; restore access before retrying; owned DNS challenges will be cleaned up")
	}
	routes := make([]worker.Route, len(item.Domains))
	for i, domain := range item.Domains {
		routes[i].Domain = domain
	}
	if conflict, err := s.routeConflict(ctx, tx, workload, routes); err != nil {
		return err
	} else if conflict != "" {
		if err := cleanup(); err != nil {
			return err
		}
		return save("failed", "a requested certificate hostname is no longer available; no certificate was installed")
	}
	client, err := s.certificateACMEAccount(ctx, pending.Directory, pending.TermsURL)
	if err != nil {
		soon = time.Now().Add(time.Minute)
		return save("reconciling", "The saved certificate authority account is unavailable; Cove will reconcile it before proceeding")
	}
	defer client.HTTPClient.CloseIdleConnections()
	if pending.OrderURI == "" {
		if !freshOrder {
			if pending.OrderAttempts >= 3 {
				return save("failed", "The authority did not confirm an order after three attempts; no DNS was published; remove this issuer and create a new request")
			}
			pending.OrderAttempts++
			// A lost new-order response cannot be listed through this ACME client; bound orphaned, unfinalized orders.
			if err := save("requesting", ""); err != nil {
				return err
			}
			return s.processCertificateIssuer(ctx, item.ID, true)
		}
		identifiers := make([]acme.AuthzID, len(item.Domains))
		for i, domain := range item.Domains {
			identifiers[i] = acme.AuthzID{Type: "dns", Value: domain}
		}
		order, err := client.AuthorizeOrder(ctx, identifiers)
		if err != nil || validateCertificateOrder(client, order, item.Domains) != nil {
			soon = time.Now().Add(time.Duration(pending.OrderAttempts) * time.Minute)
			return save("reconciling", "The certificate authority has not confirmed an order; no DNS challenge was published")
		}
		pending.OrderURI = order.URI
		return save("requesting", "")
	}
	order, err := client.GetOrder(ctx, pending.OrderURI)
	if err != nil {
		soon = time.Now().Add(time.Minute)
		return save("reconciling", "The saved certificate order is unavailable; Cove will retry the same order")
	}
	if err := validateCertificateOrder(client, order, item.Domains); err != nil {
		if err := cleanup(); err != nil {
			return err
		}
		return save("failed", "The certificate authority rejected or expired this order; remove the issuer and create a new request")
	}
	if pending.AuthorizationIndex < 0 || pending.AuthorizationIndex > len(order.AuthzURLs) {
		return errors.New("stored certificate authorization progress is invalid")
	}
	if pending.AuthorizationIndex < len(order.AuthzURLs) {
		endpoint := order.AuthzURLs[pending.AuthorizationIndex]
		authorization, err := readCertificateDNSAuthorization(ctx, client, endpoint, item.Domains)
		if err != nil {
			soon = time.Now().Add(time.Minute)
			return save("reconciling", "The saved hostname authorization is not confirmed; Cove will retry without creating another DNS record")
		}
		if authorization == nil {
			if err := cleanup(); err != nil {
				return err
			}
			pending.IntentID = ""
			pending.AuthorizationIndex++
			return save("requesting", "")
		}
		if pending.IntentID == "" {
			var count int
			// The hostname-claim lock above also serializes challenge admission across this zone.
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM route_certificate_dns_intents WHERE zone_id=$1 AND phase<>'cleaned'`, item.ZoneID).Scan(&count); err != nil {
				return err
			}
			if count >= certificateDNSIntentLimit {
				return save("failed", "This zone has reached its 128 unresolved DNS-challenge limit; finish owned challenge cleanup before retrying")
			}
			intent := certificateDNSIntentState{Record: certificateTXT{Name: authorization.TXTName, Content: authorization.TXTValue, Comment: "cove-acme-" + resourceID()}}
			protected, err := s.sealJSON(intent)
			if err != nil {
				return err
			}
			pending.IntentID = resourceID()
			if _, err := tx.Exec(ctx, `INSERT INTO route_certificate_dns_intents(id,issuer_id,workload_id,zone_id,cycle_id,state,next_check_at) VALUES($1,$2,$3,$4,$5,$6,now())`, pending.IntentID, item.ID, workload, item.ZoneID, pending.CycleID, protected); err != nil {
				return err
			}
			return save("requesting", "Publishing the owned DNS challenge")
		}
		var phase string
		var protected []byte
		if err := tx.QueryRow(ctx, `SELECT phase,state FROM route_certificate_dns_intents WHERE id=$1 AND issuer_id=$2 AND cycle_id=$3`, pending.IntentID, item.ID, pending.CycleID).Scan(&phase, &protected); err != nil {
			return err
		}
		if phase == "cleaned" {
			pending.IntentID = ""
			return save("requesting", "Preparing a new owned DNS challenge after confirmed cleanup")
		}
		if phase == "cleanup" {
			soon = time.Now().Add(time.Minute)
			return save("reconciling", "The previous DNS challenge is still being cleaned up; Cove retains its ownership proof")
		}
		if phase == "blocked" {
			return save("failed", "Cloudflare refused the DNS challenge; review DNS Edit permission, record conflicts and provider rate limits before retrying")
		}
		if phase != "present" {
			soon = time.Now().Add(15 * time.Second)
			return save("requesting", "Waiting for the owned DNS challenge to be confirmed")
		}
		var intent certificateDNSIntentState
		raw, err := unseal(s.Key, protected)
		if err != nil || json.Unmarshal(raw, &intent) != nil || intent.Record.Name != authorization.TXTName || intent.Record.Content != authorization.TXTValue {
			return errors.New("saved DNS challenge does not match the certificate authorization")
		}
		challenge, err := client.GetChallenge(ctx, authorization.ChallengeURI)
		if err == nil && challenge == nil {
			err = errors.New("certificate authority returned no saved challenge")
		}
		if err == nil && challenge.Status == acme.StatusPending {
			lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
			values, lookupErr := net.DefaultResolver.LookupTXT(lookup, authorization.TXTName)
			cancel()
			if lookupErr != nil || !slices.Contains(values, authorization.TXTValue) {
				soon = time.Now().Add(15 * time.Second)
				return save("requesting", "Waiting for DNS propagation before asking the certificate authority to validate")
			}
			_, err = client.Accept(ctx, challenge)
		}
		if err != nil {
			soon = time.Now().Add(time.Minute)
			return save("reconciling", "DNS validation has not been confirmed; Cove will reconcile the saved challenge")
		}
		soon = time.Now().Add(3 * time.Second)
		return save("requesting", "Waiting for the certificate authority to validate DNS")
	}
	material, err := finalizeCertificateOrder(ctx, client, pending.OrderURI, item.Domains, worker.RouteCertificate{ID: state.CandidateID, PrivateKeyPEM: state.PrivateKeyPEM}, state.CSR)
	if err != nil {
		soon = time.Now().Add(30 * time.Second)
		return save("reconciling", "Issuance is not confirmed; Cove will reconcile the saved order and key before proceeding")
	}
	return s.storeIssuerCertificate(ctx, tx, workload, item, state, material)
}
