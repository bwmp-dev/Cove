package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var tunnelUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var errTunnelDNSNotCreated = errors.New("the DNS create was refused before mutation")

var errTunnelOutcomeUnknown = errors.New("the tunnel create outcome is unknown; its saved credential must authenticate before adoption or cleanup")

type cloudflareTunnelIdentity struct {
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	ID        string `json:"id,omitempty"`
	Attempted bool   `json:"attempted"`
	Owned     bool   `json:"owned"`
}

type cloudflareTunnelRemote struct {
	ID           string     `json:"id"`
	Account      string     `json:"account_tag"`
	Name         string     `json:"name"`
	ConfigSource string     `json:"config_src"`
	DeletedAt    *time.Time `json:"deleted_at"`
}

func (s *Server) cloudflareTunnelRequest(ctx context.Context, token, account, method, path string, body, result any) error {
	if !validCloudflareToken(token) || !cloudflareID.MatchString(account) || !strings.HasPrefix(path, "/cfd_tunnel") || strings.ContainsAny(path, "\r\n#") {
		return errors.New("invalid managed tunnel provider request")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4/accounts/"+account+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid managed tunnel provider request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, Transport: s.CloudflareTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Cloudflare could not be reached")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cloudflareStatusError(response.StatusCode)
	}
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 || json.Unmarshal(data, &envelope) != nil || !envelope.Success {
		return errors.New("Cloudflare returned an invalid managed tunnel response")
	}
	if result != nil && json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("Cloudflare returned an invalid managed tunnel result")
	}
	return nil
}

func validateTunnelIntent(account string, intent cloudflareTunnelIdentity) error {
	secret, err := base64.StdEncoding.DecodeString(intent.Secret)
	if !cloudflareID.MatchString(account) || !strings.HasPrefix(intent.Name, "cove-") || !cloudflareID.MatchString(strings.TrimPrefix(intent.Name, "cove-")) || err != nil || len(secret) != 32 || (intent.ID != "" && !tunnelUUID.MatchString(intent.ID)) {
		return errors.New("invalid saved tunnel ownership proof")
	}
	return nil
}

func validTunnelRemote(remote cloudflareTunnelRemote, account, name string) bool {
	return tunnelUUID.MatchString(remote.ID) && remote.Account == account && remote.Name == name && remote.ConfigSource == "local" && remote.DeletedAt == nil
}

func (s *Server) findCloudflareTunnel(ctx context.Context, token, account string, intent cloudflareTunnelIdentity) (*cloudflareTunnelRemote, error) {
	if err := validateTunnelIntent(account, intent); err != nil {
		return nil, err
	}
	var found *cloudflareTunnelRemote
	for page := 1; page <= 64; page++ {
		query := url.Values{"name": {intent.Name}, "is_deleted": {"false"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var list []cloudflareTunnelRemote
		if err := s.cloudflareTunnelRequest(ctx, token, account, "GET", "/cfd_tunnel?"+query.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, remote := range list {
			if remote.Name != intent.Name {
				continue
			}
			if !validTunnelRemote(remote, account, intent.Name) || found != nil || intent.ID != "" && remote.ID != intent.ID {
				return nil, errors.New("tunnel identity changed or is duplicated")
			}
			copy := remote
			found = &copy
		}
		if len(list) < 100 {
			return found, nil
		}
	}
	return nil, errors.New("too many tunnels to confirm the saved ownership proof")
}

func (s *Server) createCloudflareTunnel(ctx context.Context, token, account string, intent cloudflareTunnelIdentity) (*cloudflareTunnelRemote, error) {
	if err := validateTunnelIntent(account, intent); err != nil {
		return nil, err
	}
	if intent.Attempted || intent.ID != "" {
		return nil, errTunnelOutcomeUnknown
	}
	var result cloudflareTunnelRemote
	body := map[string]string{"name": intent.Name, "config_src": "local", "tunnel_secret": intent.Secret}
	if err := s.cloudflareTunnelRequest(ctx, token, account, "POST", "/cfd_tunnel", body, &result); err != nil {
		var status cloudflareStatusError
		if errors.As(err, &status) && status >= 400 && status < 500 && status != 408 {
			return nil, err
		}
		return nil, errTunnelOutcomeUnknown
	}
	if !validTunnelRemote(result, account, intent.Name) {
		return nil, errTunnelOutcomeUnknown
	}
	return &result, nil
}

func (s *Server) removeCloudflareTunnel(ctx context.Context, token, account string, intent cloudflareTunnelIdentity) error {
	if !intent.Owned || intent.ID == "" {
		return errTunnelOutcomeUnknown
	}
	if err := validateTunnelIntent(account, intent); err != nil {
		return err
	}
	var current cloudflareTunnelRemote
	err := s.cloudflareTunnelRequest(ctx, token, account, "GET", "/cfd_tunnel/"+intent.ID, nil, &current)
	var status cloudflareStatusError
	if errors.As(err, &status) && status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if current.DeletedAt != nil && current.ID == intent.ID && current.Account == account && current.Name == intent.Name && current.ConfigSource == "local" {
		return nil
	}
	if !validTunnelRemote(current, account, intent.Name) || current.ID != intent.ID {
		return errors.New("provider tunnel ownership changed; cleanup is withheld")
	}
	return s.cloudflareTunnelRequest(ctx, token, account, "DELETE", "/cfd_tunnel/"+intent.ID, nil, nil)
}

type tunnelDNSProof struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
}

func validateTunnelDNS(zone cloudflareZone, proof tunnelDNSProof) error {
	id := strings.TrimSuffix(proof.Content, ".cfargotunnel.com")
	if !cloudflareID.MatchString(zone.ZoneID) || !validDNSName(zone.Domain) || !validDNSName(proof.Name) || proof.Name != zone.Domain && !strings.HasSuffix(proof.Name, "."+zone.Domain) || !strings.HasSuffix(proof.Content, ".cfargotunnel.com") || !tunnelUUID.MatchString(id) || !strings.HasPrefix(proof.Comment, "cove-tunnel-") || !cloudflareID.MatchString(strings.TrimPrefix(proof.Comment, "cove-tunnel-")) || proof.ID != "" && !cloudflareID.MatchString(proof.ID) {
		return errors.New("invalid managed tunnel DNS ownership proof")
	}
	return nil
}

func (s *Server) findTunnelDNS(ctx context.Context, zone cloudflareZone, proof tunnelDNSProof) (*tunnelDNSProof, error) {
	if err := validateTunnelDNS(zone, proof); err != nil {
		return nil, err
	}
	var found *tunnelDNSProof
	for page := 1; page <= 64; page++ {
		var list []struct {
			tunnelDNSProof
			Type    string `json:"type"`
			Proxied bool   `json:"proxied"`
		}
		query := url.Values{"name": {proof.Name}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := s.cloudflareRequest(ctx, zone, "GET", "/dns_records?"+query.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, record := range list {
			if record.Name != proof.Name || record.Type != "CNAME" || !record.Proxied || record.Content != proof.Content || record.Comment != proof.Comment || !cloudflareID.MatchString(record.ID) || proof.ID != "" && proof.ID != record.ID || found != nil {
				return nil, errors.New("conflicting or changed DNS records prevent tunnel publication or cleanup")
			}
			copy := record.tunnelDNSProof
			found = &copy
		}
		if len(list) < 100 {
			return found, nil
		}
	}
	return nil, errors.New("too many DNS records to verify managed tunnel ownership")
}

func (s *Server) presentTunnelDNS(ctx context.Context, zone cloudflareZone, proof tunnelDNSProof, attempted bool) (*tunnelDNSProof, error) {
	found, err := s.findTunnelDNS(ctx, zone, proof)
	if err != nil && !attempted && proof.ID == "" {
		return nil, fmt.Errorf("%w: DNS preflight must succeed before creation", errTunnelDNSNotCreated)
	}
	if err != nil || found != nil {
		return found, err
	}
	if attempted || proof.ID != "" {
		return nil, errTunnelOutcomeUnknown
	}
	var record struct {
		tunnelDNSProof
		Type    string `json:"type"`
		Proxied bool   `json:"proxied"`
	}
	body := map[string]any{"type": "CNAME", "name": proof.Name, "content": proof.Content, "proxied": true, "ttl": 1, "comment": proof.Comment}
	if err := s.cloudflareRequest(ctx, zone, "POST", "/dns_records", body, &record); err != nil {
		var status cloudflareStatusError
		if errors.As(err, &status) && status >= 400 && status < 500 && status != 408 {
			return nil, errTunnelDNSNotCreated
		}
		return nil, errTunnelOutcomeUnknown
	}
	if record.Type != "CNAME" || !record.Proxied || record.Name != proof.Name || record.Content != proof.Content || record.Comment != proof.Comment || !cloudflareID.MatchString(record.ID) {
		return nil, errTunnelOutcomeUnknown
	}
	return &record.tunnelDNSProof, nil
}

func (s *Server) removeTunnelDNS(ctx context.Context, zone cloudflareZone, proof tunnelDNSProof) error {
	if proof.ID == "" {
		return errTunnelOutcomeUnknown
	}
	if err := validateTunnelDNS(zone, proof); err != nil {
		return err
	}
	var record struct {
		tunnelDNSProof
		Type    string `json:"type"`
		Proxied bool   `json:"proxied"`
	}
	err := s.cloudflareRequest(ctx, zone, "GET", "/dns_records/"+proof.ID, nil, &record)
	var status cloudflareStatusError
	if errors.As(err, &status) && status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if record.ID != proof.ID || record.Name != proof.Name || record.Type != "CNAME" || !record.Proxied || record.Content != proof.Content || record.Comment != proof.Comment {
		return errors.New("DNS ownership changed; cleanup is withheld")
	}
	return s.cloudflareRequest(ctx, zone, "DELETE", "/dns_records/"+proof.ID, nil, nil)
}
