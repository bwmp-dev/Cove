package worker

import (
	"strings"
	"testing"
)

func TestIngressExposureConfirmationErrorsNameFieldsWithoutSecretValues(t *testing.T) {
	want := map[string]rawObject{"http.middlewares/private-name": {Config: `{"forwardAuth":{"address":"https://provider.invalid/private-token"}}`}}
	live := map[string]rawObject{"http.middlewares/private-name": {Config: `{"forwardAuth":{"address":"https://changed.invalid/other-token"}}`}}
	difference := ingressExposureDifference(live, want)
	if difference != "http.middlewares definitions differ in forwardAuth" || strings.Contains(difference, "private-name") || strings.Contains(difference, "token") {
		t.Fatal("confirmation diagnostics exposed private configuration", difference)
	}
	if difference := ingressExposureDifference(want, want); difference != "" {
		t.Fatal("unchanged objects reported as different", difference)
	}
}

func TestIngressExposureConfirmationPreservesTLSWhileNormalizingImplicitDefault(t *testing.T) {
	parse := func(tls string) map[string]rawObject {
		t.Helper()
		objects, err := parseRawData([]byte(`{"routers":{"app@file":{"rule":"Host(` + "`" + `app.example.test` + "`" + `)","service":"app","status":"enabled"` + tls + `}}}`))
		if err != nil {
			t.Fatal(err)
		}
		return objects
	}
	implicit := parse(`,"tls":{}`)
	explicit := parse(`,"tls":{"options":"default"}`)
	if !liveMatches(implicit, explicit, nil) || !liveMatches(explicit, implicit, nil) {
		t.Fatal("implicit and explicit default TLS options differ")
	}
	for _, tls := range []string{"", `,"tls":null`, `,"tls":{"options":"default@file"}`, `,"tls":{"options":"strict@file"}`, `,"tls":{"certResolver":"cove"}`, `,"tls":{"domains":[{"main":"other.example.test"}]}`} {
		if liveMatches(parse(tls), implicit, nil) {
			t.Fatal("TLS policy change was hidden", tls)
		}
	}
}
