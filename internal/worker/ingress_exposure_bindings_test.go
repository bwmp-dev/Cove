package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func bindingBodyFixture(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	input := ingressSnapshotFixture()
	input["HostConfig"].(map[string]any)["PortBindings"] = map[string]any{
		"80/tcp":    []ingressDockerBinding{{HostIP: "", HostPort: "80"}},
		"443/tcp":   []ingressDockerBinding{{HostIP: "", HostPort: "443"}},
		"18000/tcp": []ingressDockerBinding{{HostIP: "127.0.0.1", HostPort: "18000"}},
		"20000/tcp": []ingressDockerBinding{{HostIP: "192.168.1.2", HostPort: "9443"}},
	}
	raw, _ := json.Marshal([]any{input})
	snapshot, err := inspectIngressProxy(raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ingressProxyCreateBody(snapshot, strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestIngressExposureBindingsPreserveInheritedWildcardAndCustomPublications(t *testing.T) {
	body := bindingBodyFixture(t)
	settings := exposureConfigFixture()
	settings.Public.HTTPS = []IngressBinding{{Address: "0.0.0.0", Port: 443}}
	if err := bindIngressProxy(body, settings, map[string]int{"private": 20001, "datagrams": 20002}, map[string]int{"old": 20000}, false, false); err != nil {
		t.Fatal(err)
	}
	var host struct {
		PortBindings   map[string][]ingressDockerBinding
		ReadonlyRootfs bool
		SecurityOpt    []string
	}
	if json.Unmarshal(body["HostConfig"], &host) != nil {
		t.Fatal("host settings unreadable")
	}
	if len(host.PortBindings["443/tcp"]) != 1 || host.PortBindings["443/tcp"][0].HostIP != "" || host.PortBindings["443/udp"][0].HostIP != "" || len(host.PortBindings["18000/tcp"]) != 1 || len(host.PortBindings["20000/tcp"]) != 0 || !host.ReadonlyRootfs || len(host.SecurityOpt) == 0 {
		t.Fatal("inherited wildcard, custom publication or security settings changed")
	}
	if host.PortBindings["20002/udp"][0].HostIP != "::" || host.PortBindings["20001/tcp"][0].HostIP != "192.168.1.2" {
		t.Fatal("managed IPv6 UDP/private bindings were lost")
	}
}

func TestIngressExposureBindingsCanReplacePublicAddressesExplicitly(t *testing.T) {
	body := bindingBodyFixture(t)
	if err := bindIngressProxy(body, exposureConfigFixture(), map[string]int{"private": 20001, "datagrams": 20002}, map[string]int{"old": 20000}, true, true); err != nil {
		t.Fatal(err)
	}
	var host struct {
		PortBindings map[string][]ingressDockerBinding
	}
	json.Unmarshal(body["HostConfig"], &host)
	if len(host.PortBindings["443/tcp"]) != 2 || host.PortBindings["443/tcp"][1].HostIP != "::" || host.PortBindings["443/tcp"][1].HostPort != "8443" || host.PortBindings["443/udp"][1].HostPort != "8443" {
		t.Fatal("explicit dual-stack HTTPS/QUIC publication was not applied")
	}
	body = bindingBodyFixture(t)
	var raw map[string]json.RawMessage
	json.Unmarshal(body["HostConfig"], &raw)
	raw["PortBindings"] = json.RawMessage(`{"443/udp":[{"HostIP":"0.0.0.0","HostPort":"443"}]}`)
	body["HostConfig"], _ = json.Marshal(raw)
	if err := bindIngressProxy(body, exposureConfigFixture(), map[string]int{"private": 20001, "datagrams": 20002}, map[string]int{"old": 20000}, true, false); err == nil {
		t.Fatal("an unmanaged UDP publication was overwritten")
	}
}
