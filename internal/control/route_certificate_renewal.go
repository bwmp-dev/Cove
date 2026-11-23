package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) scheduleCertificateIssuers(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE route_certificate_issuers SET next_check_at=now()+interval '1 minute' WHERE id IN (SELECT id FROM route_certificate_issuers WHERE next_check_at<=now() ORDER BY next_check_at LIMIT 2 FOR UPDATE SKIP LOCKED) RETURNING id`)
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
			if err := s.processCertificateIssuer(ctx, id, false); err != nil {
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

func certificateRenewalAt(info worker.RouteCertificateInfo) time.Time {
	lifetime := info.ExpiresAt.Sub(info.NotBefore)
	// Short-lived authorities need time for durable DNS validation and route delivery.
	lead := max(lifetime/3, min(5*time.Minute, lifetime/2))
	return info.ExpiresAt.Add(-min(30*24*time.Hour, lead))
}

func (s *Server) processCertificateIssuer(ctx context.Context, id string, freshAttempt bool) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var item certificateIssuer
	var workload string
	var encrypted, acmeState []byte
	err = tx.QueryRow(ctx, `SELECT id,workload_id,name,provider,zone_id,domains,validity_days,auto_renew,phase,state,COALESCE(certificate_id,''),operation_id,acme_state FROM route_certificate_issuers WHERE id=$1 FOR UPDATE`, id).Scan(&item.ID, &workload, &item.Name, &item.Provider, &item.ZoneID, &item.Domains, &item.ValidityDays, &item.AutoRenew, &item.Phase, &encrypted, &item.CertificateID, &item.OperationID, &acmeState)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var state certificateIssuerState
	raw, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(raw, &state) != nil || !cloudflareID.MatchString(state.CandidateID) {
		return errors.New("stored certificate issuer request is unreadable")
	}
	save := func(phase, message string, due *time.Time) error {
		sealed, err := s.sealJSON(state)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE route_certificate_issuers SET state=$2,phase=$3,last_error=$4,next_check_at=$5,updated_at=now() WHERE id=$1`, id, sealed, phase, message, due); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	soon := time.Now().Add(time.Minute)
	if item.Provider == "acme_dns" {
		soon = time.Now().Add(3 * time.Second)
	}
	if item.Phase == "issued" {
		if !item.AutoRenew {
			return save("issued", "", nil)
		}
		certificate, at, err := s.routeCertificate(ctx, tx, workload, item.CertificateID)
		if err != nil {
			return save("failed", "the issued certificate is unavailable; remove this issuer and create a new request", nil)
		}
		info, err := worker.ParseRouteCertificate(certificate, at)
		if err != nil {
			return save("failed", "the issued certificate cannot be read", nil)
		}
		due := certificateRenewalAt(info)
		if due.After(time.Now()) {
			return save("issued", "", &due)
		}
		csr, key, err := worker.NewRouteCertificateKey(item.Domains)
		if err != nil {
			return err
		}
		state = certificateIssuerState{CSR: csr, PrivateKeyPEM: key, CandidateID: resourceID(), PreviousCertificateID: item.CertificateID}
		if _, err := tx.Exec(ctx, "UPDATE route_certificate_issuers SET operation_id='' WHERE id=$1", item.ID); err != nil {
			return err
		}
		return save("requesting", "", &soon)
	}
	if item.Phase == "applying" || item.CertificateID == state.CandidateID {
		return s.applyIssuerCertificate(ctx, tx, workload, item, state)
	}
	if item.Provider == "acme_dns" {
		return s.processACMECertificateIssuer(ctx, tx, workload, item, state, acmeState, freshAttempt)
	}
	if !state.Attempted {
		state.Attempted = true
		// The private key and attempted request must commit before a provider can accept the CSR.
		if err := save("requesting", "", &soon); err != nil {
			return err
		}
		return s.processCertificateIssuer(ctx, id, true)
	}
	var locked string
	if err := tx.QueryRow(ctx, "SELECT id FROM workloads WHERE id=$1 FOR UPDATE", workload).Scan(&locked); err != nil {
		return err
	}
	zone, err := s.issuerZone(ctx, tx, workload, item.ZoneID, item.Domains)
	if err != nil {
		if freshAttempt {
			state.Attempted = false
		}
		return save("failed", "Cloudflare zone access is unavailable; restore this team's hostname share before retrying", nil)
	}
	routes := make([]worker.Route, len(item.Domains))
	for i, domain := range item.Domains {
		routes[i].Domain = domain
	}
	if conflict, err := s.routeConflict(ctx, tx, workload, routes); err != nil {
		return err
	} else if conflict != "" {
		if freshAttempt {
			state.Attempted = false
		}
		return save("failed", "a certificate hostname is now owned by another resource or the panel", nil)
	}
	if zone.ConnectionID != "" {
		zone.Token, err = s.cloudflareConnectionToken(ctx, zone.ConnectionID)
		if err != nil {
			if freshAttempt {
				state.Attempted = false
			}
			return save("failed", "Cloudflare credentials are unavailable; reconnect before retrying", nil)
		}
	}
	material := worker.RouteCertificate{ID: state.CandidateID, PrivateKeyPEM: state.PrivateKeyPEM}
	var issued *originCertificate
	if freshAttempt {
		created, createErr := s.createOriginCertificate(ctx, zone.Token, state.CSR, item.Domains, item.ValidityDays)
		if createErr != nil {
			if errors.Is(createErr, errOriginCertificateUncertain) {
				return save("reconciling", "Cloudflare may have issued this request; Cove will reconcile its saved key before trying anything else", &soon)
			}
			state.Attempted = false
			return save("failed", createErr.Error(), nil)
		}
		issued = &created
	} else {
		issued, err = s.findOriginCertificate(ctx, zone.Token, item.ZoneID, material, state.CSR)
		if err != nil || issued == nil {
			return save("reconciling", "the saved request has not been found in Cloudflare; Cove will keep reconciling without duplicating issuance", &soon)
		}
	}
	material.CertificatePEM = issued.Certificate
	if _, err := validateIssuedRouteCertificate(material, state.CSR, time.Now().UTC()); err != nil {
		return save("failed", "the provider's certificate does not match this saved request; it was not installed", nil)
	}
	return s.storeIssuerCertificate(ctx, tx, workload, item, state, material)
}

func (s *Server) storeIssuerCertificate(ctx context.Context, tx pgx.Tx, workload string, item certificateIssuer, state certificateIssuerState, material worker.RouteCertificate) error {
	now := time.Now().UTC()
	info, err := validateIssuedRouteCertificate(material, state.CSR, now)
	if err != nil {
		return errors.New("issued certificate does not match the saved request")
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM route_certificates WHERE workload_id=$1`, workload).Scan(&count); err != nil {
		return err
	}
	if count >= 32 {
		_, err := tx.Exec(ctx, `UPDATE route_certificate_issuers SET phase='failed',last_error='Remove unused certificate versions before retrying; this resource has reached its 32-version limit',next_check_at=NULL WHERE id=$1`, item.ID)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	protected, err := s.sealJSON(material)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO route_certificates(id,workload_id,name,material,metadata,created_at,source) VALUES($1,$2,$3,$4,$5,$6,$7)`, material.ID, workload, item.Name+" "+now.Format("2006-01-02 15:04:05"), protected, metadata, now, item.Provider); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE route_certificate_issuers SET certificate_id=$2,phase='applying',last_error='',updated_at=now() WHERE id=$1`, item.ID, material.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('certificate-issuer','issue route certificate',$1,201)`, workload); err != nil {
		return err
	}
	item.CertificateID = material.ID
	return s.applyIssuerCertificate(ctx, tx, workload, item, state)
}

func (s *Server) applyIssuerCertificate(ctx context.Context, tx pgx.Tx, workload string, item certificateIssuer, state certificateIssuerState) error {
	finish := func(phase, message, operation string, due *time.Time) error {
		if _, err := tx.Exec(ctx, `UPDATE route_certificate_issuers SET phase=$2,last_error=$3,operation_id=$4,next_check_at=$5,updated_at=now() WHERE id=$1`, item.ID, phase, message, operation, due); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	soon := time.Now().Add(time.Minute)
	if item.Provider == "acme_dns" {
		soon = time.Now().Add(3 * time.Second)
	}
	if item.OperationID != "" {
		var status string
		if err := tx.QueryRow(ctx, "SELECT state FROM operations WHERE id=$1", item.OperationID).Scan(&status); err != nil {
			return err
		}
		if status == "queued" || status == "running" {
			return finish("applying", "Waiting for the certificate route update", item.OperationID, &soon)
		}
		if status != "succeeded" && item.Phase != "failed" {
			return finish("failed", "the certificate route update did not succeed; the previous routes were retained; retry to apply the issued version", item.OperationID, nil)
		}
	}
	var raw []byte
	var live, active bool
	if err := tx.QueryRow(ctx, `SELECT w.configuration,COALESCE((n.capabilities->>'live_routes')::boolean,false),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, workload).Scan(&raw, &live, &active); err != nil {
		return err
	}
	data, err := unseal(s.Key, raw)
	var config workloadInput
	if err != nil || json.Unmarshal(data, &config) != nil {
		return errors.New("certificate renewal configuration unavailable")
	}
	changed := false
	for i, route := range config.Routes {
		if state.PreviousCertificateID != "" && route.Policy != nil && route.Policy.CertificateID == state.PreviousCertificateID {
			policy := *route.Policy
			policy.CertificateID = item.CertificateID
			config.Routes[i].Policy = &policy
			changed = true
		}
	}
	if !changed {
		var due *time.Time
		if item.AutoRenew {
			certificate, at, err := s.routeCertificate(ctx, tx, workload, item.CertificateID)
			if err != nil {
				return err
			}
			info, err := worker.ParseRouteCertificate(certificate, at)
			if err != nil {
				return err
			}
			renewAt := certificateRenewalAt(info)
			due = &renewAt
		}
		return finish("issued", "", item.OperationID, due)
	}
	if active {
		return finish("applying", "Waiting for the active resource operation before renewing its routes", "", &soon)
	}
	if _, err := s.issuerZone(ctx, tx, workload, item.ZoneID, item.Domains); err != nil {
		return finish("failed", "Cloudflare zone access was revoked before renewal could be applied; restore this team's hostname share before retrying", "", nil)
	}
	attempted, err := deployAttempted(ctx, tx, workload)
	if err != nil {
		return err
	}
	if !live && attempted {
		return finish("applying", "Upgrade and verify the worker before applying the renewed certificate", "", &soon)
	}
	if err := s.validateRouteCertificates(ctx, tx, workload, config.Routes); err != nil {
		return finish("failed", "the renewed certificate no longer fits the resource's saved routes; review hostnames before retrying", "", nil)
	}
	if err := checkRouteProtocol(ctx, tx, workload, config.Routes); err != nil {
		return finish("applying", "Upgrade and verify the worker before applying the renewed certificate", "", &soon)
	}
	conflict, err := s.routeConflict(ctx, tx, workload, config.Routes)
	if err != nil {
		return err
	}
	if conflict != "" {
		return finish("failed", "a saved route hostname is no longer available for this resource", "", nil)
	}
	if !attempted {
		sealed, err := s.sealJSON(config)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE workloads SET configuration=$2 WHERE id=$1", workload, sealed); err != nil {
			return err
		}
		return finish("issued", "", "", &soon)
	}
	request := worker.Request{ID: resourceID(), Workload: workload, Kind: "service", Action: "routes", Routes: config.Routes}
	if err := worker.ValidateRequest(request); err != nil {
		return finish("failed", "the renewed route configuration is invalid", "", nil)
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'routes',$3)", request.ID, workload, payload); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('certificate-renewal','renew route certificate',$1,202)`, workload); err != nil {
		return err
	}
	return finish("applying", "", request.ID, &soon)
}
