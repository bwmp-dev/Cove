package worker

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"cove/internal/outbound"
)

func TestTunnelMutationsShareNodeMaintenanceLease(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	if err := e.writeCloudflarePublishing(cloudflarePublishingState{Candidate: &cloudflareGeneration{Revision: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := e.ApplyCloudflarePublishing(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "another proxy, move or host maintenance change") {
		t.Fatalf("publishing bypassed the node maintenance lease: %v", err)
	}
	if err := e.recoverCloudflarePublishing(context.Background()); err == nil || !strings.Contains(err.Error(), "another proxy, move or host maintenance change") {
		t.Fatalf("recovery bypassed the node maintenance lease: %v", err)
	}
}

func TestTunnelCannotBypassHTTPOutboundProxy(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	proxy := &outbound.Config{HTTPS: "http://127.0.0.1:9"}
	if err := writeJSON(filepath.Join(e.Root, "outbound-proxy.json"), proxy); err != nil {
		t.Fatal(err)
	}
	value := &CloudflarePublishing{ID: strings.Repeat("a", 32), AccountID: strings.Repeat("b", 32), TunnelID: "11111111-2222-3333-4444-555555555555", TunnelSecret: base64.StdEncoding.EncodeToString(make([]byte, 32)), Hostnames: []string{"app.example.test"}, Image: DefaultCloudflaredImage}
	if _, err := e.ApplyCloudflarePublishing(context.Background(), value); err == nil || !strings.Contains(err.Error(), "cannot use Cove's HTTP outbound proxy") {
		t.Fatalf("connector bypassed proxy policy: %v", err)
	}
	if err := e.writeCloudflarePublishing(cloudflarePublishingState{Desired: value, Phase: "preparing"}); err != nil {
		t.Fatal(err)
	}
	if err := e.configureOutbound(context.Background(), proxy); err == nil || !strings.Contains(err.Error(), "finish Cloudflare Tunnel cleanup") {
		t.Fatalf("proxy was enabled over an existing connector: %v", err)
	}
	if err := e.writeCloudflarePublishing(cloudflarePublishingState{Phase: "disabled"}); err != nil {
		t.Fatal(err)
	}
	// Invalid replacement settings cannot prevent a local shutdown request.
	response := e.RPC(context.Background(), RPCRequest{Method: "cloudflare_publishing_disable", Proxy: &outbound.Config{HTTPS: "invalid"}})
	if response.Error != "" || response.Publishing == nil || response.Publishing.Phase != "disabled" {
		t.Fatalf("outbound settings blocked emergency local shutdown: %s", response.Error)
	}
}
