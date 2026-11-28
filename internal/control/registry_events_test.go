package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistryNotificationsSelectOnlyTaggedManifestPushes(t *testing.T) {
	payload := []byte(`{"events":[
{"id":"one","action":"push","target":{"mediaType":"application/vnd.oci.image.index.v1+json","repository":"team/app","tag":"stable","digest":"untrusted","url":"https://127.0.0.1/private"}},
{"id":"one","action":"push","target":{"mediaType":"application/vnd.oci.image.index.v1+json","repository":"team/app","tag":"stable","digest":"different"}},
{"id":"blob","action":"push","target":{"mediaType":"application/octet-stream","repository":"team/app","tag":"stable"}},
{"id":"deleted","action":"delete","target":{"mediaType":"application/vnd.oci.image.index.v1+json","repository":"team/app","tag":"stable"}},
{"id":"untagged","action":"push","target":{"mediaType":"application/vnd.oci.image.manifest.v1+json","repository":"team/app"}}
]}`)
	events, err := parseRegistryPushEvents(payload)
	if err != nil || len(events) != 1 || len(events[0].Key) != 64 || events[0].Repository != "team/app" || events[0].Tag != "stable" {
		t.Fatal(events, err)
	}
	encoded, _ := json.Marshal(events)
	for _, excluded := range []string{"untrusted", "different", "127.0.0.1", "digest", "url"} {
		if strings.Contains(strings.ToLower(string(encoded)), excluded) {
			t.Fatal("notification carried provider-controlled fetch or digest data")
		}
	}
}

func TestRegistryNotificationsRejectAmbiguousOrUnboundedDelivery(t *testing.T) {
	event := `{"id":"same","action":"push","target":{"mediaType":"application/vnd.oci.image.manifest.v1+json","repository":"team/app","tag":"stable"}}`
	for _, body := range []string{
		`{}`, `{"events":null}`, `{"events":[]} {"events":[]}`,
		`{"events":[` + event + `,` + strings.Replace(event, "stable", "other", 1) + `]}`,
		`{"events":[` + strings.TrimSuffix(strings.Repeat(event+",", 65), ",") + `]}`,
		`{"events":[` + strings.Replace(event, `"same"`, `""`, 1) + `]}`,
		`{"events":[` + strings.Replace(event, "same", `line\nfeed`, 1) + `]}`,
		`{"events":[` + strings.Replace(event, "stable", "*", 1) + `]}`,
		`{"events":[` + strings.Replace(event, "team/app", "../app", 1) + `]}`,
		strings.Repeat(" ", 256*1024+1),
	} {
		if _, err := parseRegistryPushEvents([]byte(body)); err == nil {
			t.Fatal("ambiguous or unbounded notification accepted")
		}
	}
	if events, err := parseRegistryPushEvents([]byte(`{"events":[]}`)); err != nil || len(events) != 0 {
		t.Fatal("empty notification batch refused", err)
	}
}
