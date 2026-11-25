package worker

import (
	"cove/internal/outbound"
	"path/filepath"
	"testing"
)

func TestBuildpacksMergeInheritedProxy(t *testing.T) {
	for _, source := range []string{"environment", "saved", "request", "variable"} {
		t.Run(source, func(t *testing.T) {
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY"} {
				t.Setenv(key, "")
			}
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			proxy := outbound.Config{HTTP: "http://proxy.example.test:8080", HTTPS: "http://proxy.example.test:8080", NoProxy: "example.test"}
			r := Request{}
			switch source {
			case "environment":
				t.Setenv("HTTPS_PROXY", proxy.HTTPS)
			case "saved":
				if err := writeJSON(filepath.Join(e.Root, "outbound-proxy.json"), proxy); err != nil {
					t.Fatal(err)
				}
			case "request":
				r.OutboundProxy = &proxy
			case "variable":
				r.Environment = map[string]string{"HTTPS_PROXY": proxy.HTTPS}
			}
			got, err := e.buildpacksProxies(r, map[string]string{"HTTPS_PROXY": "http://different.example.test:8080"})
			if err != nil || got["HTTPS_PROXY"] != proxy.HTTPS {
				t.Fatal("CNB bypassed inherited proxy", got, err)
			}
		})
	}
}
