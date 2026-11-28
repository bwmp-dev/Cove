package control

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

func addressRanges(values []string) ([]netip.Prefix, error) {
	if len(values) > 128 {
		return nil, errors.New("at most 128 IP addresses or CIDRs allowed")
	}
	ranges := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			address, e := netip.ParseAddr(value)
			if e != nil || address.Zone() != "" || address.Is4In6() {
				return nil, errors.New("valid IPv4 or IPv6 addresses or CIDRs required")
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
			return nil, errors.New("IPv4-mapped IPv6 ranges are not supported")
		}
		ranges = append(ranges, prefix.Masked())
	}
	return ranges, nil
}

func inRanges(address netip.Addr, ranges []netip.Prefix) bool {
	for _, prefix := range ranges {
		if prefix.Contains(address.Unmap()) {
			return true
		}
	}
	return false
}

func panelClientAddress(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	trusted := os.Getenv("COVE_TRUSTED_PROXIES")
	if trusted == "" {
		return address.Unmap(), nil
	}
	ranges, err := addressRanges(strings.Split(trusted, ","))
	if err != nil {
		return netip.Addr{}, err
	}
	if !inRanges(address, ranges) {
		return address.Unmap(), nil
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(chain) > 32 {
		return netip.Addr{}, errors.New("forwarded chain too long")
	}
	for i := len(chain) - 1; i >= 0; i-- {
		address, err = netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil || address.Zone() != "" {
			return netip.Addr{}, errors.New("invalid forwarded address")
		}
		if !inRanges(address, ranges) {
			return address.Unmap(), nil
		}
	}
	return address.Unmap(), nil
}

func panelAccessExempt(r *http.Request) bool {
	p := r.URL.Path
	return r.Method == "POST" && (p == "/bootstrap/register" || p == "/hooks/github-app" || strings.HasPrefix(p, "/hooks/github/") || strings.HasPrefix(p, "/hooks/git/")) || r.Method == "GET" && (p == "/healthz" || p == "/install.sh" || strings.HasPrefix(p, "/bootstrap/worker/") || strings.HasPrefix(p, "/releases/worker/"))
}

func (s *Server) restrictPanel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if panelAccessExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		var values []string
		if override, ok := os.LookupEnv("COVE_PANEL_ALLOWLIST"); ok {
			if override != "" {
				values = strings.Split(override, ",")
			}
		} else if err := s.DB.QueryRow(r.Context(), "SELECT panel_allowlist FROM control_settings WHERE id=1").Scan(&values); err != nil {
			http.Error(w, "panel access settings unavailable", 503)
			return
		}
		ranges, err := addressRanges(values)
		if err != nil {
			http.Error(w, "panel access configuration invalid", 503)
			return
		}
		if len(ranges) != 0 {
			address, err := panelClientAddress(r)
			if err != nil || !inRanges(address, ranges) {
				r.Pattern = "panel access restriction"
				s.recordAudit(r, "network access denied", 403)
				http.Error(w, "panel access denied", 403)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) panelAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		var in struct {
			Allowlist []string `json:"allowlist"`
		}
		if !decode(w, r, &in) {
			return
		}
		ranges, err := addressRanges(in.Allowlist)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if len(ranges) > 0 {
			if r.Header.Get("X-Forwarded-For") != "" {
				host, _, err := net.SplitHostPort(r.RemoteAddr)
				peer, parseErr := netip.ParseAddr(host)
				trusted, trustErr := addressRanges(strings.Split(os.Getenv("COVE_TRUSTED_PROXIES"), ","))
				if err != nil || parseErr != nil || trustErr != nil || !inRanges(peer, trusted) {
					http.Error(w, "configure the actual trusted proxy before restricting forwarded panel traffic", 409)
					return
				}
			}
			address, err := panelClientAddress(r)
			if err != nil || !inRanges(address, ranges) {
				http.Error(w, "allowlist must include your current client address", 409)
				return
			}
		}
		values := make([]string, len(ranges))
		for i, prefix := range ranges {
			values[i] = prefix.String()
		}
		if _, err := s.DB.Exec(r.Context(), "UPDATE control_settings SET panel_allowlist=$1 WHERE id=1", values); err != nil {
			http.Error(w, "could not save panel access", 500)
			return
		}
	}
	var values []string
	if err := s.DB.QueryRow(r.Context(), "SELECT panel_allowlist FROM control_settings WHERE id=1").Scan(&values); err != nil {
		http.Error(w, "panel access unavailable", 503)
		return
	}
	if values == nil {
		values = []string{}
	}
	_, overridden := os.LookupEnv("COVE_PANEL_ALLOWLIST")
	address, err := panelClientAddress(r)
	client := ""
	if err == nil {
		client = address.String()
	}
	respond(w, 200, map[string]any{"allowlist": values, "environment_override": overridden, "client_address": client})
}
