package control

import (
	"cove/internal/buildinfo"
	"strings"
	"testing"
	"time"
)

func TestDowngradeConfirmationAndSchemaRefusalIntegration(t *testing.T) {
	f := newAccountFixture(t)
	before, oldTime := buildinfo.Version, buildinfo.RevisionTime
	oldGeneration, oldRoot := buildinfo.SourceGeneration, buildinfo.SourceRoot
	buildinfo.SourceGeneration = "2"
	buildinfo.SourceRoot = strings.Repeat("c", 40)
	buildinfo.Version = "beta-" + strings.Repeat("a", 40)
	buildinfo.RevisionTime = time.Now().Format(time.RFC3339)
	defer func() {
		buildinfo.Version = before
		buildinfo.RevisionTime = oldTime
		buildinfo.SourceGeneration = oldGeneration
		buildinfo.SourceRoot = oldRoot
	}()
	f.sql("UPDATE control_settings SET update_channel='beta'")
	f.sql("INSERT INTO panel_channel_observations(channel,version,revision,schema_hash,revision_time,source_root,source_generation) VALUES('release','v1.0.0',$1,$2,now()-interval '1 day',$3,1)", strings.Repeat("a", 40), schemaFingerprint(), strings.Repeat("c", 40))
	f.call("root", "", "PUT", "/api/updates", map[string]any{"channel": "release"}, 409)
	f.call("root", "", "PUT", "/api/updates", map[string]any{"channel": "release", "confirm_downgrade": true, "target_revision": strings.Repeat("b", 40)}, 409)
	f.call("root", "", "PUT", "/api/updates", map[string]any{"channel": "release", "confirm_downgrade": true, "target_revision": strings.Repeat("a", 40)}, 200)
	f.sql("UPDATE control_settings SET update_channel='beta'")
	f.sql("UPDATE panel_channel_observations SET schema_hash=$1", strings.Repeat("b", 64))
	f.call("root", "", "PUT", "/api/updates", map[string]any{"channel": "release", "confirm_downgrade": true, "target_revision": strings.Repeat("a", 40)}, 409)
	f.sql("UPDATE panel_channel_observations SET checked_at=now()-interval '3 hours'")
	f.call("root", "", "PUT", "/api/updates", map[string]any{"channel": "release", "confirm_downgrade": true, "target_revision": strings.Repeat("a", 40)}, 409)
}

func TestBetaReleaseOrderUsesSourceGeneration(t *testing.T) {
	oldGeneration, oldRoot := buildinfo.SourceGeneration, buildinfo.SourceRoot
	defer func() { buildinfo.SourceGeneration = oldGeneration; buildinfo.SourceRoot = oldRoot }()
	buildinfo.SourceGeneration = "20"
	buildinfo.SourceRoot = strings.Repeat("c", 40)
	now := time.Now()
	target := panelTarget{Version: "v1.0.0", Time: now.Add(time.Hour), Root: strings.Repeat("c", 40), Generation: 19}
	if down, known := panelDowngrade("beta-"+strings.Repeat("a", 40), target); !known || !down {
		t.Fatal("older release not recognized")
	}
}
