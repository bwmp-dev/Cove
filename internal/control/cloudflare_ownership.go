package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
)

const dnsOwnershipPrefix = "cove-dns-"

func (s *Server) dnsOwnershipComment(zone cloudflareZone, record cloudflareRecord) string {
	if len(s.Key) != 32 || !cloudflareID.MatchString(zone.ZoneID) || !validDNSName(strings.TrimPrefix(record.Name, "*.")) || record.Name != zone.Domain && !strings.HasSuffix(record.Name, "."+zone.Domain) || record.TTL != 1 || len(record.Tags) != 0 {
		return ""
	}
	ip, err := netip.ParseAddr(record.Content)
	if err != nil || ip.Is4In6() || record.Type != "A" && record.Type != "AAAA" || ip.Is4() != (record.Type == "A") {
		return ""
	}
	for name, value := range record.Settings {
		if name != "ipv4_only" && name != "ipv6_only" || string(value) != "false" {
			return ""
		}
	}
	proof, _ := json.Marshal([]any{zone.ZoneID, record.Name, record.Type, ip.String(), record.Proxied, record.TTL})
	mac := hmac.New(sha256.New, s.Key)
	mac.Write([]byte("cove-route-dns-v1\x00"))
	mac.Write(proof)
	return dnsOwnershipPrefix + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) ownsDNSRecord(zone cloudflareZone, record cloudflareRecord) bool {
	comment := s.dnsOwnershipComment(zone, record)
	return comment != "" && hmac.Equal([]byte(comment), []byte(record.Comment))
}

func (s *Server) createDNSRecord(ctx context.Context, zone cloudflareZone, record cloudflareRecord) error {
	record.Comment = s.dnsOwnershipComment(zone, record)
	err := s.cloudflareRequest(ctx, zone, "POST", "/dns_records", record, nil)
	var status cloudflareStatusError
	if record.Comment == "" || !errors.As(err, &status) || status != 400 && status != 403 && status != 422 {
		return err
	}
	// Only a definite rejection may fall back; a lost create reply must first be
	// reconciled by its marker, never followed by an unmarked duplicate create.
	record.Comment = ""
	return s.cloudflareRequest(ctx, zone, "POST", "/dns_records", record, nil)
}

func (s *Server) removeOwnedDNSRecord(ctx context.Context, zone cloudflareZone, proof cloudflareRecord) error {
	if !cloudflareID.MatchString(proof.ID) || !s.ownsDNSRecord(zone, proof) {
		return nil
	}
	var current cloudflareRecord
	err := s.cloudflareRequest(ctx, zone, "GET", "/dns_records/"+proof.ID, nil, &current)
	var status cloudflareStatusError
	if errors.As(err, &status) && status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if current.ID != proof.ID || current.Comment != proof.Comment || !s.ownsDNSRecord(zone, current) {
		return nil
	}
	err = s.cloudflareRequest(ctx, zone, "DELETE", "/dns_records/"+proof.ID, nil, nil)
	if errors.As(err, &status) && status == 404 {
		return nil
	}
	return err
}
