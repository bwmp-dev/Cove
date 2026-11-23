package worker

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func exposureConfigFixture() IngressExposureSettings {
	return IngressExposureSettings{
		Public:    &IngressPublicBindings{HTTP: []IngressBinding{{Address: "0.0.0.0", Port: 80}}, HTTPS: []IngressBinding{{Address: "0.0.0.0", Port: 8443}, {Address: "::", Port: 8443}}},
		HTTP3:     true,
		Listeners: []IngressListener{{ID: "private", Protocol: "https", Visibility: "lan", Bindings: []IngressBinding{{Address: "192.168.1.2", Port: 9443}}}, {ID: "datagrams", Protocol: "udp", Visibility: "public", Bindings: []IngressBinding{{Address: "::", Port: 5353}}}},
	}
}

func TestIngressExposureCLIOnlyChangesOwnedEntrypoints(t *testing.T) {
	input := ingressSnapshotFixture()
	config := input["Config"].(map[string]any)
	command := config["Cmd"].([]string)
	command = append(command, "--certificatesresolvers.cove.acme.storage=/data/traefik-acme.json", "--entrypoints.cove-listener-old.address=:20000", "--entrypoints.websecure.http3=false")
	config["Cmd"] = command
	raw, _ := json.Marshal([]any{input})
	snapshot, err := inspectIngressProxy(raw)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := ingressExposureCLI(snapshot, exposureConfigFixture(), map[string]int{"private": 20001, "datagrams": 20002}, map[string]int{"old": 20000})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{command[0], command[1], command[2], "--certificatesresolvers.cove.acme.storage=/data/traefik-acme.json", "--entrypoints.websecure.http3.advertisedport=8443", "--entrypoints.cove-listener-private.address=:20001/tcp", "--entrypoints.cove-listener-datagrams.address=:20002/udp"} {
		if !slices.Contains(updated, arg) {
			t.Fatal("existing startup, ACME storage or planned listener was lost")
		}
	}
	if slices.Contains(updated, "--entrypoints.websecure.http3=false") || slices.Contains(updated, "--entrypoints.cove-listener-old.address=:20000") {
		t.Fatal("old owned static settings remained active")
	}
	if _, err := ingressExposureCLI(snapshot, exposureConfigFixture(), map[string]int{"private": 20001, "datagrams": 20002}, nil); err == nil {
		t.Fatal("unowned reserved entrypoint was adopted")
	}
}

func TestIngressExposureStaticFilePreservesPanelAndCertificateConfiguration(t *testing.T) {
	source := []byte(`entryPoints:
  web:
    address: ':80'
    http:
      redirections:
        entryPoint: {to: websecure, scheme: https}
  websecure: {address: ':443', http3: {advertisedPort: 443}, transport: {respondingTimeouts: {idleTimeout: 180s}}}
  traefik: {address: '127.0.0.1:8080'}
  cove-listener-old: {address: ':20000/tcp'}
api: {insecure: true, dashboard: false}
providers: {file: {directory: /data/dynamic, watch: true}}
certificatesResolvers: {cove: {acme: {storage: /data/traefik-acme.json, httpChallenge: {entryPoint: web}}}}
`)
	updated, err := ingressExposureStaticFile(source, exposureConfigFixture(), map[string]int{"private": 20001, "datagrams": 20002}, map[string]int{"old": 20000})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if yaml.Unmarshal(updated, &decoded) != nil {
		t.Fatal("updated static file is unreadable")
	}
	for _, key := range []string{"api", "providers", "certificatesResolvers"} {
		var original map[string]any
		yaml.Unmarshal(source, &original)
		before, _ := json.Marshal(original[key])
		after, _ := json.Marshal(decoded[key])
		if string(before) != string(after) {
			t.Fatal("existing panel/provider/certificate settings were changed")
		}
	}
	points, _ := asMap(decoded["entryPoints"])
	secure, _ := asMap(points["websecure"])
	if secure["transport"] == nil || points["traefik"] == nil || points["web"] == nil || points["cove-listener-old"] != nil || points["cove-listener-private"] == nil || !strings.Contains(string(updated), "advertisedPort: 8443") {
		t.Fatal("existing or desired entrypoints were not preserved")
	}
	if _, err := ingressExposureStaticFile(source, exposureConfigFixture(), map[string]int{"private": 20001, "datagrams": 20002}, nil); err == nil {
		t.Fatal("unowned reserved entrypoint was overwritten")
	}
	unsafe := append(append([]byte{}, source...), []byte("providers: {docker: {endpoint: unix:///var/run/docker.sock}}\n")...)
	if _, err := ingressExposureStaticFile(unsafe, exposureConfigFixture(), map[string]int{"private": 20001, "datagrams": 20002}, map[string]int{"old": 20000}); err == nil {
		t.Fatal("unsafe or ambiguous static configuration was accepted")
	}
}
