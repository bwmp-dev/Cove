package worker

import (
	"strings"
	"testing"
	"time"
)

func registryBuildFixtureTag(operation string) string {
	request := Request{ID: operation, Workload: strings.Repeat("a", 32), Registry: &Registry{Server: "registry.example.test"}, Build: &BuildOptions{Repository: "team/app"}}
	return strings.TrimPrefix(buildImageTag(request, "app"), "registry.example.test/team/app:")
}

func TestManagedRegistryRetentionProtectsReleasesAndIndexChildren(t *testing.T) {
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	digest := func(s string) string { return "sha256:" + strings.Repeat(s, 64) }
	m := func(d string, age int) ManagedRegistryManifest {
		return ManagedRegistryManifest{Repository: "team/app", Digest: digest(d), Tags: []string{registryBuildFixtureTag("fixture")}, CreatedAt: now.Add(-time.Duration(age) * 24 * time.Hour)}
	}
	newest, expired, release, child, external, unknown := m("a", 1), m("b", 40), m("c", 40), m("d", 40), m("e", 40), m("f", 40)
	release.Children = []string{child.Digest}
	external.Tags = append(external.Tags, "stable")
	unknown.CreatedAt = time.Time{}
	inventory := []ManagedRegistryManifest{newest, expired, release, child, external, unknown}
	p := ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}
	remove, err := PlanManagedRegistryRetention(p, inventory, []string{"team/app@" + release.Digest}, now)
	if err != nil || len(remove) != 1 || remove[0].Digest != expired.Digest {
		t.Fatalf("unsafe retention plan: %+v %v", remove, err)
	}
	if _, err = PlanManagedRegistryRetention(p, append(inventory, expired), nil, now); err == nil {
		t.Fatal("duplicate inventory accepted")
	}
	if _, err = PlanManagedRegistryRetention(p, inventory, []string{"team/app:mutable"}, now); err == nil {
		t.Fatal("mutable release reference accepted")
	}
	release.Children = []string{digest("0")}
	if _, err = PlanManagedRegistryRetention(p, []ManagedRegistryManifest{newest, release}, []string{"team/app@" + release.Digest}, now); err == nil {
		t.Fatal("missing retained index child accepted")
	}
}

func TestManagedRegistryRetentionIsBoundedAndKeepsPerRepository(t *testing.T) {
	now := time.Now().UTC()
	var inventory []ManagedRegistryManifest
	for _, repo := range []string{"team/app", "team/other"} {
		for _, d := range []string{"a", "b"} {
			inventory = append(inventory, ManagedRegistryManifest{Repository: repo, Digest: "sha256:" + strings.Repeat(d, 64), Tags: []string{registryBuildFixtureTag("fixture")}, CreatedAt: now.Add(-30 * 24 * time.Hour)})
		}
	}
	p := ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}
	remove, err := PlanManagedRegistryRetention(p, inventory, nil, now)
	if err != nil || len(remove) != 2 || remove[0].Digest != "sha256:"+strings.Repeat("b", 64) || remove[1].Repository != "team/other" {
		t.Fatalf("incorrect per-repository retention: %+v %v", remove, err)
	}
	if _, err = PlanManagedRegistryRetention(p, make([]ManagedRegistryManifest, 8193), nil, now); err == nil {
		t.Fatal("unbounded inventory accepted")
	}
	for _, p := range []ManagedRegistryRetention{{0, 7}, {101, 7}, {1, 0}, {1, 3651}} {
		if p.Validate() == nil {
			t.Fatal("invalid retention policy accepted")
		}
	}
}
