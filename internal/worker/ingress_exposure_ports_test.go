package worker

import "testing"

func TestIngressExposureAllocationPreservesExistingPeersWhenAddingListeners(t *testing.T) {
	settings := exposureConfigFixture()
	entries := []IngressEntrypoint{{Name: "web", Address: ":80"}, {Name: "websecure", Address: ":443"}, {Name: "unmanaged", Address: ":20001/tcp"}, {Name: ingressListenerName("private"), Address: ":20000/tcp"}}
	ports, err := allocateIngressListenerPorts(settings, map[string]int{"private": 20000}, entries)
	if err != nil {
		t.Fatal(err)
	}
	if ports["private"] != 20000 || ports["datagrams"] != 20002 {
		t.Fatal("new listener displaced a saved allocation or reused an unmanaged port")
	}
	if _, err := allocateIngressListenerPorts(settings, nil, entries); err == nil {
		t.Fatal("unowned reserved entrypoint was adopted")
	}
	entries = append(entries, IngressEntrypoint{Name: "quic-conflict", Address: ":443/udp"})
	if _, err := allocateIngressListenerPorts(settings, map[string]int{"private": 20000}, entries); err == nil {
		t.Fatal("HTTP/3 reused an existing raw UDP entrypoint")
	}
}

func TestIngressExposureAllocationRefusesMissingOrChangedSavedEntrypoints(t *testing.T) {
	settings := exposureConfigFixture()
	for _, entries := range [][]IngressEntrypoint{
		{{Name: "websecure", Address: ":443"}},
		{{Name: ingressListenerName("private"), Address: ":20005"}},
		{{Name: ingressListenerName("private"), Address: ":20000"}, {Name: "unmanaged", Address: ":20000/udp"}},
	} {
		if _, err := allocateIngressListenerPorts(settings, map[string]int{"private": 20000}, entries); err == nil {
			t.Fatal("changed or ambiguous original allocation was accepted")
		}
	}
}
