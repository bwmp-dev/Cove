package control

import (
	"strings"
	"testing"
)

func TestRegistryWebhookFiltersAreBoundedAndExplicit(t *testing.T) {
	config := registryWebhookConfiguration{Provider: "distribution", Enabled: true, RegistryID: "registry", Filters: []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}, {Repository: "team/api", Tag: "*"}}}
	if err := validateRegistryWebhook(config); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		repository, tag string
		want            bool
	}{{"team/app", "stable", true}, {"team/app", "canary", false}, {"team/api", "1.2.3", true}, {"other/app", "stable", false}} {
		if got := registryPushMatches(config, registryPushEvent{Repository: test.repository, Tag: test.tag}); got != test.want {
			t.Fatal(test.repository, test.tag, got)
		}
	}
	config.Enabled = false
	if registryPushMatches(config, registryPushEvent{Repository: "team/api", Tag: "1.2.3"}) {
		t.Fatal("disabled hook matched a push")
	}
	for _, filter := range []registryWebhookFilter{{Repository: "https://registry.example/team/app", Tag: "stable"}, {Repository: "../app", Tag: "stable"}, {Repository: "team/app", Tag: ""}, {Repository: "team/app", Tag: strings.Repeat("a", 129)}} {
		config.Filters = []registryWebhookFilter{filter}
		if validateRegistryWebhook(config) == nil {
			t.Fatal("invalid filter accepted")
		}
	}
	config.Filters = []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}, {Repository: "team/app", Tag: "stable"}}
	if validateRegistryWebhook(config) == nil {
		t.Fatal("duplicate filters accepted")
	}
	config.Filters = make([]registryWebhookFilter, 33)
	if validateRegistryWebhook(config) == nil {
		t.Fatal("unbounded filter list accepted")
	}
}
