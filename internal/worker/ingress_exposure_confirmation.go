package worker

import (
	"bytes"
	"encoding/json"
	"strings"
)

func ingressExposureDifference(live, expected map[string]rawObject) string {
	fields := []string{"rule", "priority", "entryPoints", "service", "middlewares", "tls", "loadBalancer", "weighted", "forwardAuth", "headers", "errors", "ipAllowList", "basicAuth", "redirectRegex"}
	for _, kind := range []string{"http.routers/", "http.services/", "http.middlewares/", "tcp.routers/", "tcp.services/", "tcp.middlewares/", "udp.routers/", "udp.services/"} {
		for key, want := range expected {
			if !strings.HasPrefix(key, kind) {
				continue
			}
			got, found := live[key]
			if !found {
				return strings.TrimSuffix(kind, "/") + " objects are missing"
			}
			if got.Config == want.Config {
				continue
			}
			var before, after map[string]json.RawMessage
			if json.Unmarshal([]byte(want.Config), &before) == nil && json.Unmarshal([]byte(got.Config), &after) == nil {
				for _, field := range fields {
					if !bytes.Equal(before[field], after[field]) {
						if field == "tls" {
							var oldTLS, newTLS map[string]json.RawMessage
							if json.Unmarshal(before[field], &oldTLS) == nil && json.Unmarshal(after[field], &newTLS) == nil {
								for _, name := range []string{"options", "certResolver", "domains"} {
									if !bytes.Equal(oldTLS[name], newTLS[name]) {
										return strings.TrimSuffix(kind, "/") + " definitions differ in tls." + name
									}
								}
							}
						}
						return strings.TrimSuffix(kind, "/") + " definitions differ in " + field
					}
				}
			}
			return strings.TrimSuffix(kind, "/") + " definitions differ"
		}
	}
	return ""
}
