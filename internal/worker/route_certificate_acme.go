package worker

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

type acmeCertificateArchive map[string][]json.RawMessage

const maxACMEStorageBytes = 64 * 1024 * 1024

type acmeProjection struct {
	Document map[string]json.RawMessage
	Archive  []json.RawMessage
	Changed  bool
}

func projectACMECertificates(data []byte, retained []json.RawMessage, uploaded []Route) (acmeProjection, error) {
	result := acmeProjection{Document: map[string]json.RawMessage{}, Archive: append([]json.RawMessage{}, retained...)}
	if len(data) > 0 && json.Unmarshal(data, &result.Document) != nil {
		return result, errors.New("managed ACME storage is unreadable")
	}
	if len(retained) > 0 && len(result.Document) == 0 {
		return result, errors.New("managed ACME account storage is missing; restore it before changing certificate assignments")
	}
	fingerprints := map[string]bool{}
	fingerprint := func(raw json.RawMessage) (string, *x509.Certificate, error) {
		var stored struct {
			Certificate []byte `json:"certificate"`
		}
		if json.Unmarshal(raw, &stored) != nil {
			return "", nil, errors.New("managed ACME certificate is unreadable")
		}
		block, _ := pem.Decode(stored.Certificate)
		if block == nil {
			return "", nil, errors.New("managed ACME certificate is unreadable")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", nil, errors.New("managed ACME certificate is unreadable")
		}
		sum := sha256.Sum256(block.Bytes)
		return hex.EncodeToString(sum[:]), certificate, nil
	}
	for _, entry := range retained {
		id, _, err := fingerprint(entry)
		if err != nil {
			return result, err
		}
		var scope struct {
			Resolver string `json:"cove_resolver"`
		}
		if err := json.Unmarshal(entry, &scope); err != nil || scope.Resolver == "" {
			return result, errors.New("managed certificate archive has no resolver identity")
		}
		fingerprints[scope.Resolver+"/"+id] = true
	}
	// Archive entries carry their resolver identity; provider account data stays in the original document.
	for resolver, document := range result.Document {
		var fields map[string]json.RawMessage
		if json.Unmarshal(document, &fields) != nil {
			return result, errors.New("managed ACME account is unreadable")
		}
		var entries []json.RawMessage
		if raw := fields["Certificates"]; len(raw) > 0 && string(raw) != "null" {
			if json.Unmarshal(raw, &entries) != nil {
				return result, errors.New("managed ACME certificates are unreadable")
			}
		}
		combined := append([]json.RawMessage{}, entries...)
		for _, raw := range retained {
			var archived struct {
				Resolver string `json:"cove_resolver"`
			}
			if json.Unmarshal(raw, &archived) != nil {
				return result, errors.New("managed certificate archive is unreadable")
			}
			if archived.Resolver == resolver {
				combined = append(combined, raw)
			}
		}
		kept := []json.RawMessage{}
		seen := map[string]bool{}
		for _, raw := range combined {
			id, certificate, err := fingerprint(raw)
			if err != nil {
				return result, err
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			collision := false
			for _, route := range uploaded {
				if slices.ContainsFunc(append(append([]string{}, certificate.DNSNames...), certificate.Subject.CommonName), func(name string) bool {
					return RouteDomainsOverlap(strings.TrimRight(strings.ToLower(name), "."), route.Domain)
				}) {
					collision = true
					break
				}
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil {
				return result, errors.New("managed ACME certificate is unreadable")
			}
			delete(fields, "cove_resolver")
			clean, err := json.Marshal(fields)
			if err != nil {
				return result, err
			}
			if !collision {
				kept = append(kept, clean)
				continue
			}
			if err := CertificateOwnedByRoutes(RouteCertificate{CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))}, uploaded); err != nil {
				return result, errors.New("a cached ACME certificate also serves names without uploaded replacements or disabled ACME; change all its names before switching")
			}
			if !fingerprints[resolver+"/"+id] {
				encoded, _ := json.Marshal(resolver)
				fields["cove_resolver"] = encoded
				archived, err := json.Marshal(fields)
				if err != nil {
					return result, err
				}
				result.Archive = append(result.Archive, archived)
				fingerprints[resolver+"/"+id] = true
			}
		}
		if len(entries) == 0 && len(kept) == 0 {
			continue
		}
		// Ignore whitespace and object key order while retaining all provider fields.
		var previous []any
		var next []any
		before, _ := json.Marshal(entries)
		after, _ := json.Marshal(kept)
		json.Unmarshal(before, &previous)
		json.Unmarshal(after, &next)
		if !reflect.DeepEqual(previous, next) {
			result.Changed = true
			fields["Certificates"] = after
			encoded, err := json.Marshal(fields)
			if err != nil {
				return result, err
			}
			result.Document[resolver] = encoded
		}
	}
	return result, nil
}

func (e *Engine) readACMEFile(ctx context.Context, name string) ([]byte, error) {
	path := filepath.Join(e.Root, "certificate-read-"+name)
	defer os.Remove(path)
	output := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", "cove-ingress:/data/" + name, path}, nil, output); err != nil {
		if strings.Contains(output.data.String(), "Could not find the file") {
			return nil, nil
		}
		return nil, errors.New("could not read managed ACME storage")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxACMEStorageBytes {
		return nil, errors.New("managed ACME storage exceeds the 64 MiB projection limit")
	}
	return os.ReadFile(path)
}

func (e *Engine) projectRouteCertificateACME(ctx context.Context, routes map[string][]Route, out io.Writer) (result error) {
	uploaded := []Route{}
	for _, list := range routes {
		for _, route := range list {
			if route.Policy != nil && (route.Policy.CertificateID != "" || route.Policy.CertificateSource == "default") {
				uploaded = append(uploaded, route)
			}
		}
	}
	archivePath := filepath.Join(e.Root, "certificate-acme-archive.json")
	archive := acmeCertificateArchive{}
	raw, err := os.ReadFile(archivePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && (len(raw) > maxACMEStorageBytes || json.Unmarshal(raw, &archive) != nil) {
		return errors.New("managed ACME certificate archive is unreadable")
	}
	marker := filepath.Join(e.Root, "certificate-acme-reload.json")
	pending, err := e.certificateReloadPending()
	if err != nil {
		return err
	}
	if len(uploaded) == 0 && len(archive) == 0 && !pending {
		return nil
	}
	names := []string{"traefik-acme.json", "cloudflare-acme.json"}
	plan := func() (map[string]acmeProjection, error) {
		projections := map[string]acmeProjection{}
		for _, name := range names {
			data, err := e.readACMEFile(ctx, name)
			if err != nil {
				return nil, err
			}
			projected, err := projectACMECertificates(data, archive[name], uploaded)
			if err != nil {
				return nil, err
			}
			projections[name] = projected
		}
		return projections, nil
	}
	projected, err := plan()
	if err != nil {
		return err
	}
	changed := pending
	for _, value := range projected {
		changed = changed || value.Changed
	}
	if !changed {
		return nil
	}
	if ctx.Value(nodeLeaseContextKey{}) != e {
		unlock, err := e.lockNodeChange(false)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if err := e.requireInterlockedHostSchedule(); err != nil {
		return err
	}
	// The intent survives a worker exit while the proxy is stopped; startup synchronizes it again.
	if err := writeJSON(marker, map[string]int{"version": 1}); err != nil {
		return err
	}
	stopped := false
	defer func() {
		if stopped {
			restart, done := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer done()
			if err := e.Run(restart, e.Root, []string{"docker", "start", "cove-ingress"}, nil, out); err != nil {
				result = errors.Join(result, err)
			}
		}
	}()
	if err := e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "10", "cove-ingress"}, nil, out); err != nil {
		return err
	}
	stopped = true
	// Re-read after stopping so a concurrent Traefik renewal cannot be overwritten by an earlier snapshot.
	projected, err = plan()
	if err != nil {
		return err
	}
	for name, value := range projected {
		archive[name] = value.Archive
	}
	encoded, err := json.Marshal(archive)
	if err != nil {
		return err
	}
	if len(encoded) > maxACMEStorageBytes {
		return errors.New("managed ACME certificate archive exceeds its 64 MiB limit")
	}
	if err := writeJSON(archivePath, archive); err != nil {
		return err
	}
	commands := []string{}
	for _, name := range names {
		value := projected[name]
		if !value.Changed {
			continue
		}
		local := filepath.Join(e.Root, "certificate-project-"+name)
		if err := writeJSON(local, value.Document); err != nil {
			return err
		}
		err := e.Run(ctx, e.Root, []string{"docker", "cp", local, "cove-ingress:/data/" + name + ".next"}, nil, io.Discard)
		removeErr := os.Remove(local)
		if err != nil {
			return err
		}
		if removeErr != nil {
			return removeErr
		}
		commands = append(commands, "mv /data/"+name+".next /data/"+name)
	}
	if len(commands) > 0 {
		image, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", "cove-ingress")
		if err != nil {
			return err
		}
		if err := e.Run(ctx, e.Root, []string{"docker", "run", "--rm", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--network=none", "--memory=32m", "--pids-limit=16", "--mount", "type=volume,source=cove-ingress,target=/data", "--entrypoint=sh", image, "-ec", strings.Join(commands, "; ")}, nil, out); err != nil {
			return err
		}
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "start", "cove-ingress"}, nil, out); err != nil {
		return err
	}
	stopped = false
	if err := os.Remove(marker); err != nil {
		return err
	}
	return nil
}

func (e *Engine) certificateReloadPending() (bool, error) {
	path := filepath.Join(e.Root, "certificate-acme-reload.json")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size() > 1024 {
		return false, errors.New("managed certificate reload intent is invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var intent struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &intent) != nil || intent.Version != 1 {
		return false, errors.New("managed certificate reload intent is invalid")
	}
	return true, nil
}
