package panelurl

import "testing"

func TestPanelURLPaths(t *testing.T) {
	for _, address := range []string{"https://example.com", "https://example.com/cove", "https://example.com:8443/control/cove-1"} {
		if _, err := Parse(address); err != nil {
			t.Fatal(address, err)
		}
	}
	for _, address := range []string{"http://example.com/cove", "https://example.com/", "https://example.com/cove/", "https://example.com/a//b", "https://example.com/a/../b", "https://example.com/a%2fb", "https://example.com/a?x=1", "https://example.com/a#b", "https://user@example.com/a", "https://example.com/healthz"} {
		if _, err := Parse(address); err == nil {
			t.Fatal("unsafe path accepted", address)
		}
	}
}
