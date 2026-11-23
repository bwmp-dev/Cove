package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var errInvalidRouteCertificate = errors.New("selected certificate is invalid")

type routeCertificateView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
	worker.RouteCertificateInfo
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) routeCertificates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,name,source,metadata,created_at FROM route_certificates WHERE workload_id=$1 ORDER BY created_at DESC,id", id)
		if err != nil {
			http.Error(w, "certificates unavailable", 503)
			return
		}
		defer rows.Close()
		result := []routeCertificateView{}
		for rows.Next() {
			var item routeCertificateView
			var metadata []byte
			if rows.Scan(&item.ID, &item.Name, &item.Source, &metadata, &item.CreatedAt) != nil || json.Unmarshal(metadata, &item.RouteCertificateInfo) != nil {
				http.Error(w, "certificates unavailable", 503)
				return
			}
			result = append(result, item)
		}
		if rows.Err() != nil {
			http.Error(w, "certificates unavailable", 503)
			return
		}
		respond(w, 200, result)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	if err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", id).Scan(&encrypted); errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if r.Method == "DELETE" {
		var active bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if active {
			http.Error(w, "wait for the active operation before removing a certificate", 409)
			return
		}
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM route_certificate_issuers WHERE certificate_id=$1 AND workload_id=$2)", r.PathValue("certificate"), id).Scan(&active); err != nil {
			http.Error(w, "certificate issuers unavailable", 503)
			return
		}
		if active {
			http.Error(w, "remove the certificate issuer before deleting its latest version", 409)
			return
		}
		raw, err := unseal(s.Key, encrypted)
		var config workloadInput
		if err != nil || json.Unmarshal(raw, &config) != nil {
			http.Error(w, "configuration unavailable", 500)
			return
		}
		certificate := r.PathValue("certificate")
		for _, route := range config.Routes {
			if route.Policy != nil && route.Policy.CertificateID == certificate {
				http.Error(w, "remove this certificate from all routes before deleting it", 409)
				return
			}
		}
		result, err := tx.Exec(r.Context(), "DELETE FROM route_certificates WHERE workload_id=$1 AND id=$2", id, certificate)
		if err != nil {
			http.Error(w, "could not remove certificate", 500)
			return
		}
		if result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		if tx.Commit(r.Context()) != nil {
			http.Error(w, "could not remove certificate", 500)
			return
		}
		respond(w, 200, map[string]string{"state": "deleted"})
		return
	}
	var input struct {
		Name           string `json:"name"`
		CertificatePEM string `json:"certificate_pem"`
		PrivateKeyPEM  string `json:"private_key_pem"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 80 || strings.ContainsFunc(input.Name, func(c rune) bool { return c < 32 || c == 127 }) {
		http.Error(w, "certificate name must contain 1 to 80 characters without control characters", 400)
		return
	}
	var count int
	if tx.QueryRow(r.Context(), "SELECT count(*) FROM route_certificates WHERE workload_id=$1", id).Scan(&count) != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if count >= 32 {
		http.Error(w, "remove an unused certificate before uploading more; each resource can retain 32 versions", 409)
		return
	}
	certificate := worker.RouteCertificate{ID: resourceID(), CertificatePEM: input.CertificatePEM, PrivateKeyPEM: input.PrivateKeyPEM}
	now := time.Now().UTC()
	info, err := worker.ParseRouteCertificate(certificate, now)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	raw, err := json.Marshal(certificate)
	if err == nil {
		encrypted, err = seal(s.Key, raw)
	}
	if err != nil {
		http.Error(w, "could not protect certificate", 500)
		return
	}
	metadata, err := json.Marshal(info)
	if err != nil {
		http.Error(w, "could not describe certificate", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO route_certificates(id,workload_id,name,material,metadata,created_at) VALUES($1,$2,$3,$4,$5,$6)", certificate.ID, id, input.Name, encrypted, metadata, now); err != nil {
		http.Error(w, "could not save certificate", 500)
		return
	}
	if tx.Commit(r.Context()) != nil {
		http.Error(w, "could not save certificate", 500)
		return
	}
	respond(w, 201, routeCertificateView{ID: certificate.ID, Name: input.Name, Source: "uploaded", RouteCertificateInfo: info, CreatedAt: now})
}

func (s *Server) routeCertificate(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workload, id string) (worker.RouteCertificate, time.Time, error) {
	var encrypted []byte
	var at time.Time
	var certificate worker.RouteCertificate
	if err := db.QueryRow(ctx, "SELECT material,created_at FROM route_certificates WHERE workload_id=$1 AND id=$2", workload, id).Scan(&encrypted, &at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return certificate, at, fmt.Errorf("%w: certificate does not belong to this resource", errInvalidRouteCertificate)
		}
		return certificate, at, err
	}
	raw, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(raw, &certificate) != nil || certificate.ID != id {
		return certificate, at, errors.New("stored certificate is unreadable")
	}
	if _, err := worker.ParseRouteCertificate(certificate, at); err != nil {
		return certificate, at, errors.New("stored certificate is invalid")
	}
	return certificate, at, nil
}

func (s *Server) validateRouteCertificates(ctx context.Context, tx pgx.Tx, workload string, routes []worker.Route) error {
	hosts := map[string]string{}
	for _, route := range routes {
		if route.Policy != nil && route.Policy.CertificateID != "" {
			hosts[route.Domain] = route.Policy.CertificateID
		}
	}
	for _, route := range routes {
		id := hosts[route.Domain]
		if id == "" {
			continue
		}
		if route.Policy == nil || route.Policy.CertificateID != id {
			return fmt.Errorf("%w: all paths on a hostname must select the same certificate", errInvalidRouteCertificate)
		}
		certificate, _, err := s.routeCertificate(ctx, tx, workload, id)
		if err != nil {
			return err
		}
		if _, err = worker.ParseRouteCertificate(certificate, time.Now()); err != nil {
			return fmt.Errorf("%w: %s", errInvalidRouteCertificate, err)
		}
		if err = worker.CertificateCoversRoute(certificate, route.Domain); err != nil {
			return fmt.Errorf("%w: %s", errInvalidRouteCertificate, err)
		}
		if err = worker.CertificateOwnedByRoutes(certificate, routes); err != nil {
			return fmt.Errorf("%w: %s", errInvalidRouteCertificate, err)
		}
	}
	return nil
}

func (s *Server) prepareRouteCertificates(ctx context.Context, request *worker.Request) error {
	if request.Action != "deploy" && request.Action != "rollback" && request.Action != "routes" {
		return nil
	}
	if len(request.Certificates) > 0 {
		return nil
	}
	selected := map[string]bool{}
	for _, route := range request.Routes {
		if route.Policy != nil && route.Policy.CertificateID != "" {
			selected[route.Policy.CertificateID] = true
		}
	}
	if len(selected) == 0 {
		return nil
	}
	for id := range selected {
		certificate, _, err := s.routeCertificate(ctx, s.DB, request.Workload, id)
		if err != nil {
			return err
		}
		if _, err = worker.ParseRouteCertificate(certificate, time.Now()); err != nil {
			return err
		}
		request.Certificates = append(request.Certificates, certificate)
	}
	if err := worker.ValidateRequest(*request); err != nil {
		return err
	}
	// Freeze key material before the first RPC; retries must never change the worker's immutable input digest.
	payload, err := s.sealJSON(request)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, "UPDATE operations SET payload=$2 WHERE id=$1", request.ID, payload)
	return err
}
