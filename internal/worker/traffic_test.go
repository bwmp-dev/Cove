package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTrafficClientTrustAndSanitizedRoutes(t *testing.T) {
	for _, c := range []struct {
		peer, forwarded string
		trusted         []string
		want            string
	}{
		{"203.0.113.10", "1.1.1.1", nil, "203.0.113.10"},
		{"192.0.2.10", "1.1.1.1, 198.51.100.2", []string{"192.0.2.0/24"}, "198.51.100.2"},
		{"192.0.2.10", "1.1.1.1, 192.0.2.11", []string{"192.0.2.0/24"}, "1.1.1.1"},
		{"192.0.2.10", "bad", []string{"192.0.2.0/24"}, "192.0.2.10"},
	} {
		if got := realTrafficIP(c.peer, c.forwarded, c.trusted); got != c.want {
			t.Fatal(c, got)
		}
	}
	routes := trafficRoutes(map[string][]Route{"service": {{Domain: "app.example.test", Service: "web", Port: 80}}})
	var name string
	for key := range routes {
		name = key
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	line := stamp + ` {"RouterName":"` + name + `","ClientHost":"203.0.113.4","DownstreamStatus":201,"DownstreamContentSize":42,"Duration":123000000,"RequestPath":"/private?token=secret","request_Authorization":"secret","request_X-Forwarded-For":"1.1.1.1"}`
	entries := parseTrafficLines([]byte(line+"\n"+line+"\n"+stamp+` {"RouterName":"other@file","DownstreamStatus":200}`), "proxy", routes, TrafficSettings{})
	if len(entries) != 2 || entries[0].ID == entries[1].ID || entries[0].Domain != "app.example.test" || entries[0].ClientIP != "203.0.113.4" || entries[0].DurationMS != 123 {
		t.Fatal(entries)
	}
	data, _ := json.Marshal(entries)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "private") {
		t.Fatal("request secrets escaped sanitized parser")
	}
	for _, cidr := range []string{"0.0.0.0/0", "::/0", "host.example"} {
		if validTrafficSettings(TrafficSettings{TrustedProxies: []string{cidr}}) {
			t.Fatal(cidr)
		}
	}
	if _, err := trafficCommand([]any{"--entrypoints.web.address=:80", "--entrypoints.web.forwardedheaders.insecure=true"}, TrafficSettings{Enabled: true}); err == nil {
		t.Fatal("insecure existing header trust bypassed client IP verification")
	}
}
