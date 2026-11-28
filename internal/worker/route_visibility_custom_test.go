package worker

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestPrivateRouteCustomReservationsRejectPublicTCPAndPriorityBypass(t *testing.T) {
	for _, test := range []struct {
		config  string
		allowed bool
	}{
		{"tcp: {routers: {intercept: {rule: 'HostSNI(`*`)', entryPoints: [web], service: app}}}", false},
		{"tcp: {routers: {intercept: {rule: 'HostSNI(`*`)', service: app}}}", false},
		{"tcp: {routers: {intercept: {rule: 'HostSNI(`public.example.test`)', entryPoints: [websecure], service: app}}}", false},
		{"tcp: {routers: {dedicated: {rule: 'HostSNI(`*`)', entryPoints: [cove-listener-database], service: app}}}", true},
		{fmt.Sprintf("http: {routers: {intercept: {rule: 'PathPrefix(`/`)', entryPoints: [websecure], service: app, priority: %d}}}", privateRouteDenialPriority), false},
		{"http: {routers: {intercept: {rule: 'PathPrefix(`/`)', service: app, priority: 18446744073709551615}}}", false},
		{"http: {routers: {intercept: {rule: 'PathPrefix(`/`)', service: app, priority: 9.223372036854775e18}}}", false},
		{"http: {routers: {ordinary: {rule: 'Host(`public.example.test`)', entryPoints: [websecure], service: app, priority: 100}}}", true},
		{"http: {routers: {ordinary: {rule: 'Host(`public.example.test`)', entryPoints: [websecure], service: app}}}", true},
	} {
		err := validatePrivateCustomConfig(test.config, true)
		if (err == nil) != test.allowed {
			t.Fatal(test.config, err)
		}
		if err := validatePrivateCustomConfig(test.config, false); err != nil {
			t.Fatal("ordinary custom route behavior changed without private hosts", err)
		}
	}
}

func TestPrivateRouteLiveReservationsKeepPriorityPrecisionAndOwnGuards(t *testing.T) {
	guard := privateRouteDenial("office.example.test")
	guards := map[string]ingressRouter{"cove-private-deny-owned": guard}
	data, err := json.Marshal(map[string]any{"routers": map[string]any{"cove-private-deny-owned@file": guard, "unrelated@file": map[string]any{"rule": "Host(`public.example.test`)", "entryPoints": []string{"websecure"}, "service": "app", "priority": 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateLiveData(data, guards, nil); err != nil {
		t.Fatal("owned denial was treated as a foreign priority override", err)
	}
	if err := validatePrivateLiveData(data, nil, nil); err == nil {
		t.Fatal("a foreign file impersonated the reserved guard")
	}
	guard.Service = "app"
	data, err = json.Marshal(map[string]any{"routers": map[string]any{"cove-private-deny-owned@file": guard}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateLiveData(data, guards, nil); err == nil {
		t.Fatal("changed denial was accepted")
	}
	data = []byte(fmt.Sprintf(`{"routers":{"bypass@file":{"entryPoints":["websecure"],"priority":%d,"service":"app"}}}`, privateRouteDenialPriority-1))
	if err := validatePrivateLiveData(data, nil, nil); err != nil {
		t.Fatal("safe adjacent priority lost integer precision", err)
	}
	data = []byte(fmt.Sprintf(`{"routers":{"bypass@file":{"entryPoints":["websecure"],"priority":%d,"service":"app"}}}`, privateRouteDenialPriority+1))
	if err := validatePrivateLiveData(data, nil, nil); err == nil {
		t.Fatal("adjacent reserved priority was rounded past the guard")
	}
	if err := validatePrivateLiveData([]byte(`{"tcpRouters":{"intercept@file":{"rule":"HostSNI(`+"`*`"+`)","service":"app"}}}`), nil, nil); err == nil {
		t.Fatal("live TCP precedence was ignored")
	}
}

func TestPrivateRouteGuardRejectsTLSAndRuntimeTampering(t *testing.T) {
	guard := privateRouteDenial("office.example.test")
	for _, field := range []string{"tls", "status", "middlewares"} {
		data, _ := json.Marshal(guard)
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		switch field {
		case "tls":
			object[field] = nil
		case "status":
			object[field] = "disabled"
		case "middlewares":
			object[field] = []string{"bypass"}
		}
		raw, _ := json.Marshal(map[string]any{"routers": map[string]any{"guard@file": object}})
		if err := validatePrivateLiveData(raw, map[string]ingressRouter{"guard": guard}, nil); err == nil {
			t.Fatal("changed private denial accepted", field)
		}
	}
}

func TestPrivateRouteRejectsImplicitOrDedicatedCustomHTTPBypasses(t *testing.T) {
	for _, body := range []map[string]any{{"rule": "PathPrefix(`/`)", "service": "app"}, {"rule": "PathPrefix(`/`)", "service": "app", "entryPoints": []string{"cove-listener-office"}}} {
		raw, _ := json.Marshal(map[string]any{"routers": map[string]any{"custom@file": body}})
		if err := validatePrivateLiveData(raw, nil, nil); err == nil {
			t.Fatal("custom private-listener bypass accepted")
		}
	}
}
