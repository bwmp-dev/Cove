package worker

import "testing"

func TestIngressExposureBindingsKeepPrivateListenersExplicit(t *testing.T) {
	public := &IngressPublicBindings{HTTP: []IngressBinding{{Address: "0.0.0.0", Port: 80}}, HTTPS: []IngressBinding{{Address: "0.0.0.0", Port: 443}, {Address: "::", Port: 443}}}
	for name, listener := range map[string]IngressListener{
		"LAN":          {ID: "lan", Protocol: "https", Visibility: "lan", Bindings: []IngressBinding{{Address: "192.168.50.2", Port: 8443}}},
		"Tailscale v4": {ID: "tail", Protocol: "https", Visibility: "tailscale", Bindings: []IngressBinding{{Address: "100.80.0.2", Port: 8443}}},
		"Tailscale v6": {ID: "tail6", Protocol: "https", Visibility: "tailscale", Bindings: []IngressBinding{{Address: "fd7a:115c:a1e0::2", Port: 8443}}},
		"raw UDP":      {ID: "datagrams", Protocol: "udp", Visibility: "public", Bindings: []IngressBinding{{Address: "::", Port: 5353}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateIngressExposures(IngressExposureSettings{Public: public, HTTP3: true, Listeners: []IngressListener{listener}}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, listener := range map[string]IngressListener{
		"LAN wildcard":              {ID: "lan", Protocol: "https", Visibility: "lan", Bindings: []IngressBinding{{Address: "0.0.0.0", Port: 8443}}},
		"LAN public":                {ID: "lan", Protocol: "https", Visibility: "lan", Bindings: []IngressBinding{{Address: "8.8.8.8", Port: 8443}}},
		"LAN tail mismatch":         {ID: "lan", Protocol: "https", Visibility: "lan", Bindings: []IngressBinding{{Address: "100.80.0.2", Port: 8443}}},
		"tail LAN mismatch":         {ID: "tail", Protocol: "https", Visibility: "tailscale", Bindings: []IngressBinding{{Address: "192.168.1.2", Port: 8443}}},
		"dual stack mapped":         {ID: "lan", Protocol: "tcp", Visibility: "public", Bindings: []IngressBinding{{Address: "::ffff:192.168.1.2", Port: 8443}}},
		"IPv6 interface zone":       {ID: "lan", Protocol: "tcp", Visibility: "public", Bindings: []IngressBinding{{Address: "fe80::1%eth0", Port: 8443}}},
		"public wildcard collision": {ID: "lan", Protocol: "https", Visibility: "lan", Bindings: []IngressBinding{{Address: "192.168.1.2", Port: 443}}},
		"QUIC UDP collision":        {ID: "udp", Protocol: "udp", Visibility: "public", Bindings: []IngressBinding{{Address: "::", Port: 443}}},
		"HTTP options on TCP":       {ID: "tcp", Protocol: "http", Visibility: "public", Bindings: []IngressBinding{{Address: "0.0.0.0", Port: 8080}}},
		"missing bind":              {ID: "tcp", Protocol: "tcp", Visibility: "public"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateIngressExposures(IngressExposureSettings{Public: public, HTTP3: true, Listeners: []IngressListener{listener}}); err == nil {
				t.Fatal("unsafe listener was accepted")
			}
		})
	}
	if err := validateIngressExposures(IngressExposureSettings{HTTP3: true}); err == nil {
		t.Fatal("HTTP/3 was planned without resolving effective existing publications")
	}
	if err := validateIngressExposures(IngressExposureSettings{Public: &IngressPublicBindings{}, HTTP3: true}); err == nil {
		t.Fatal("HTTP/3 was planned without an HTTPS publication")
	}
	if err := validateIngressExposures(IngressExposureSettings{}); err != nil {
		t.Fatal("unchanged legacy settings were refused", err)
	}
}
