package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRestoreCopyIsolatesStorageNetworksAndPorts(t *testing.T) {
	var config map[string]any
	if err := json.Unmarshal([]byte(`{"services":{"app":{"image":"sha256:abc","container_name":"production","ports":[{"published":"5432"}],"labels":{"traefik.enable":"true"},"networks":{"shared":{}},"volumes":[{"type":"volume","source":"data","target":"/data"},{"type":"bind","source":"/srv/uploads","target":"/uploads"}]}},"volumes":{"data":{"name":"production-data"}},"networks":{"shared":{"external":true}}}`), &config); err != nil {
		t.Fatal(err)
	}
	manifest := backupManifest{Mounts: []backupMount{{Type: "volume", Name: "production-data"}, {Type: "bind", Source: "/srv/uploads"}}}
	volumes, mounts, err := isolateRestore(config, manifest, "new-resource")
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 || len(mounts) != 2 || mounts[0].Name == mounts[1].Name {
		t.Fatal(volumes, mounts)
	}
	raw, _ := json.Marshal(config)
	for _, forbidden := range []string{"production", "/srv/uploads", "shared", "ports", "traefik"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("source attachment retained: %s", forbidden)
		}
	}
	if !strings.Contains(string(raw), `"internal":true`) {
		t.Fatal("network permits egress")
	}
}

func TestRestoreCopyRejectsExternalMountsAndPrivilege(t *testing.T) {
	for _, field := range []string{`"privileged":true`, `"network_mode":"host"`, `"devices":["/dev/sda"]`, `"volumes":[{"type":"bind","source":"/etc","read_only":true,"target":"/config"}]`} {
		var config map[string]any
		if err := json.Unmarshal([]byte(`{"services":{"app":{`+field+`}}}`), &config); err != nil {
			t.Fatal(err)
		}
		if _, _, err := isolateRestore(config, backupManifest{}, "copy"); err == nil {
			t.Fatal("unsafe copy accepted", field)
		}
	}
}
