package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"cove/internal/outbound"
)

func TestImageUpdatesMergeRejectsForeignTokenRealmBeforeAnyRequest(t *testing.T) {
	calls := 0
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "" {
			t.Error("foreign realm received registry credentials")
		}
		fmt.Fprint(w, `{"token":"foreign"}`)
	}))
	defer foreign.Close()
	c := newRegistryTagClient()
	c.client.Transport = foreign.Client().Transport
	origin, err := url.Parse("https://registry.example.test/v2/team/app/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	for _, credentials := range []*Registry{nil, {Server: "registry.example.test", Username: "fixture", Password: "fixture-only"}} {
		if _, err := c.token(t.Context(), origin, "team/app", `Bearer realm="`+foreign.URL+`/token",service="fixture"`, credentials); err == nil {
			t.Fatal("foreign token realm accepted")
		}
	}
	if calls != 0 {
		t.Fatal("untrusted token realm was contacted", calls)
	}
}

func TestImageUpdatesMergeTagRequestsUseSelectedProxy(t *testing.T) {
	calls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Host != "registry.example.invalid" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("unexpected proxy target or credential")
		}
		fmt.Fprint(w, `{"name":"team/app","tags":["1.2.0"]}`)
	}))
	defer proxy.Close()
	c := newRegistryTagClient(outbound.Config{HTTP: proxy.URL})
	request, err := http.NewRequestWithContext(context.Background(), "GET", "http://registry.example.invalid/v2/team/app/tags/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Tags []string `json:"tags"`
	}
	if err := registryJSON(response, &result); err != nil || calls != 1 || len(result.Tags) != 1 {
		t.Fatal("tag transport bypassed selected proxy", calls, result, err)
	}
}

func TestImageUpdatesMergeRefusesManagedDatabaseDiscovery(t *testing.T) {
	r := Request{ID: "check", Workload: "database", Action: "image_check", Kind: "service", ManagedDatabase: true, ImageUpdates: &ImageUpdateOptions{Images: map[string]string{"app": "postgres:17"}, Policy: ImageUpdatePolicy{Enabled: true, Mode: "notify", Rules: []ImageUpdateRule{{Service: "app"}}}}}
	if err := ValidateRequest(r); err == nil {
		t.Fatal("managed database admitted to stateless image policies")
	}
}
