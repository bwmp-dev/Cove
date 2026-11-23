package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TrafficSettings struct {
	Enabled        bool     `json:"enabled"`
	TrustedProxies []string `json:"trusted_proxies"`
}
type TrafficEntry struct {
	ID         string    `json:"id"`
	Workload   string    `json:"workload"`
	Domain     string    `json:"domain"`
	Timestamp  time.Time `json:"timestamp"`
	Status     int       `json:"status"`
	Bytes      int64     `json:"bytes"`
	DurationMS float64   `json:"duration_ms"`
	ClientIP   string    `json:"client_ip"`
}
type TrafficReport struct {
	Available bool           `json:"available"`
	Limited   bool           `json:"limited"`
	SampledAt time.Time      `json:"sampled_at"`
	Entries   []TrafficEntry `json:"entries"`
}

func validTrafficSettings(s TrafficSettings) bool {
	if len(s.TrustedProxies) > 32 {
		return false
	}
	for _, value := range s.TrustedProxies {
		p, err := netip.ParsePrefix(value)
		if err != nil || p.Bits() == 0 {
			return false
		}
	}
	return true
}
func (e *Engine) trafficSettings() (TrafficSettings, error) {
	s := TrafficSettings{TrustedProxies: []string{}}
	data, err := os.ReadFile(filepath.Join(e.Root, "traffic-settings.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if len(data) > 16384 || json.Unmarshal(data, &s) != nil || !validTrafficSettings(s) {
		return s, errors.New("invalid traffic settings")
	}
	return s, nil
}

func realTrafficIP(peer, forwarded string, trusted []string) string {
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		host, _, splitErr := net.SplitHostPort(peer)
		if splitErr != nil {
			return ""
		}
		ip, err = netip.ParseAddr(host)
		if err != nil {
			return ""
		}
	}
	ip = ip.Unmap()
	isTrusted := func(ip netip.Addr) bool {
		for _, value := range trusted {
			p, err := netip.ParsePrefix(value)
			if err == nil && p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !isTrusted(ip) {
		return ip.String()
	}
	chain := strings.Split(forwarded, ",")
	if len(chain) > 32 {
		return ip.String()
	}
	for i := len(chain) - 1; i >= 0; i-- {
		next, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			return ip.String()
		}
		ip = next.Unmap()
		if !isTrusted(ip) {
			break
		}
	}
	return ip.String()
}

func trafficRoutes(owned map[string][]Route) map[string]TrafficEntry {
	out := map[string]TrafficEntry{}
	for workload, routes := range owned {
		for _, r := range routes {
			config := newIngressConfig()
			config.addRoute(workload, r)
			for name := range config.HTTP.Routers {
				out[name+"@file"] = TrafficEntry{Workload: workload, Domain: r.Domain}
			}
		}
	}
	return out
}

func parseTrafficLines(data []byte, container string, routes map[string]TrafficEntry, settings TrafficSettings) []TrafficEntry {
	out := []TrafficEntry{}
	occurrences := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		stamp, body, ok := strings.Cut(line, " ")
		if !ok || len(body) > 65536 {
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		var access struct {
			RouterName, ClientHost          string
			DownstreamStatus                int
			DownstreamContentSize, Duration int64
			Forwarded                       string `json:"request_X-Forwarded-For"`
		}
		if json.Unmarshal([]byte(body), &access) != nil || access.DownstreamStatus < 100 || access.DownstreamStatus > 599 || access.DownstreamContentSize < 0 || access.Duration < 0 {
			continue
		}
		entry, known := routes[access.RouterName]
		if !known {
			continue
		}
		key := container + "\n" + line
		occurrence := occurrences[key]
		occurrences[key]++
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d", key, occurrence)))
		entry.ID = hex.EncodeToString(sum[:])
		entry.Timestamp = timestamp.UTC()
		entry.Status = access.DownstreamStatus
		entry.Bytes = access.DownstreamContentSize
		entry.DurationMS = float64(access.Duration) / 1e6
		entry.ClientIP = realTrafficIP(access.ClientHost, access.Forwarded, settings.TrustedProxies)
		out = append(out, entry)
	}
	return out
}

func (e *Engine) CollectTraffic(ctx context.Context, since time.Time) (*TrafficReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	result := &TrafficReport{SampledAt: now, Entries: []TrafficEntry{}}
	settings, err := e.trafficSettings()
	if err != nil {
		return nil, err
	}
	if !settings.Enabled {
		return result, nil
	}
	metadata, err := e.output(ctx, "docker", "inspect", "--format", `{"id":{{json .Id}},"labels":{{json .Config.Labels}},"cmd":{{json .Config.Cmd}}}`, "cove-ingress")
	if err != nil {
		return nil, errors.New("managed proxy unavailable")
	}
	var proxy struct {
		ID     string
		Labels map[string]string
		Cmd    []string
	}
	if json.Unmarshal([]byte(metadata), &proxy) != nil || proxy.Labels["cove.managed"] != "ingress" {
		return nil, errors.New("proxy is not managed by Cove")
	}
	configured := false
	for _, arg := range proxy.Cmd {
		if arg == "--accesslog=true" {
			configured = true
		}
	}
	if !configured {
		return result, nil
	}
	if since.IsZero() {
		since = now.Add(-time.Minute)
	}
	if since.Before(now.Add(-24 * time.Hour)) {
		since = now.Add(-24 * time.Hour)
		result.Limited = true
	}
	if since.After(now) {
		return nil, errors.New("traffic cursor is in the future")
	}
	owned, err := e.loadIngress()
	if err != nil {
		return nil, err
	}
	out := &limitedBuffer{Limit: 8 * 1024 * 1024}
	if e.Run(ctx, e.Root, []string{"docker", "logs", "--timestamps", "--since", since.Format(time.RFC3339Nano), "--until", now.Format(time.RFC3339Nano), "--tail", "2001", "cove-ingress"}, nil, out) != nil {
		return nil, errors.New("proxy access logs unavailable")
	}
	lines := strings.Split(strings.TrimSuffix(out.data.String(), "\n"), "\n")
	if len(lines) > 2000 {
		result.Limited = true
		lines = lines[len(lines)-2000:]
	}
	result.Entries = parseTrafficLines([]byte(strings.Join(lines, "\n")), proxy.ID, trafficRoutes(owned), settings)
	result.Available = true
	return result, nil
}
