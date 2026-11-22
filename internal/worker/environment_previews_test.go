package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

const previewWorkload = "0123456789abcdef0123456789abcdef"

func previewConfig(t *testing.T, raw string) map[string]any {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestIsolatePreviewComposeRewritesStorageAndNetworks(t *testing.T) {
	config := previewConfig(t, `{
 "services": {
  "app": {"image": "app", "ports": [{"target": 80, "published": "8080"}], "container_name": "prod-app",
   "networks": {"default": null, "legacy": null},
   "volumes": [{"type": "volume", "source": "uploads", "target": "/data"},
               {"type": "bind", "source": "/srv/app/config", "target": "/config"},
               {"type": "bind", "source": "/var/lib/cove/workloads/x/releases/op/nginx.conf", "target": "/etc/nginx.conf", "read_only": true},
               {"type": "tmpfs", "target": "/tmp"},
               {"type": "volume", "source": "added", "target": "/added"}],
   "depends_on": {"db": {"condition": "service_healthy"}, "worker": {"condition": "service_started"}}},
  "db": {"image": "postgres", "volumes": [{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data"}]},
  "worker": {"image": "app", "deploy": {"replicas": 2}}
 },
 "volumes": {"uploads": {"name": "prod_uploads"}, "pgdata": {"name": "cove-prod_pgdata"}, "added": {}},
 "networks": {"default": {"name": "cove-`+previewWorkload+`_default", "ipam": {"config": [{"subnet": "10.31.0.0/24"}]}}, "legacy": {"external": true, "name": "legacy"}}
}`)
	plan := &EnvironmentPreview{Preview: "preview1", Volumes: []PreviewVolume{
		{Key: "volume:uploads", Source: "prod_uploads", Target: "cove-pv-" + previewWorkload + "-00", Mode: "copy"},
		{Key: "bind:/srv/app/config", Source: "/srv/app/config", Target: "cove-pv-" + previewWorkload + "-01", Mode: "empty"},
		{Key: "volume:pgdata", Target: "cove-pv-" + previewWorkload + "-02", Mode: "database"},
	}, Disabled: []string{"worker"}, Database: &PreviewDatabase{Engine: "postgresql", Service: "db", Name: "app"}}
	volumes, err := isolatePreviewCompose(config, plan, previewWorkload, "/var/lib/cove/workloads/x/releases")
	if err != nil {
		t.Fatal(err)
	}
	added := PreviewVolumeTarget(previewWorkload, "volume:added")
	want := []string{"cove-pv-" + previewWorkload + "-00", "cove-pv-" + previewWorkload + "-01", "cove-pv-" + previewWorkload + "-02", added}
	if strings.Join(volumes, ",") != strings.Join(sortedCopy(want), ",") {
		t.Fatalf("volumes %v want %v", volumes, want)
	}
	encoded, _ := json.Marshal(config)
	text := string(encoded)
	for _, forbidden := range []string{"prod_uploads", "cove-prod_pgdata", `"ports"`, "prod-app", `"legacy"`, "/srv/app/config", "10.31.0.0"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("isolated configuration still references %s: %s", forbidden, text)
		}
	}
	services := config["services"].(map[string]any)
	if services["worker"].(map[string]any)["scale"] != 0 {
		t.Fatal("disabled service must be scaled to zero")
	}
	if _, ok := services["worker"].(map[string]any)["deploy"].(map[string]any)["replicas"]; ok {
		t.Fatal("disabled service replicas must be removed")
	}
	app := services["app"].(map[string]any)
	if _, ok := app["depends_on"].(map[string]any)["worker"]; ok {
		t.Fatal("dependencies on disabled services must be removed")
	}
	if app["labels"].(map[string]any)["dev.cove.environment-preview"] != previewWorkload {
		t.Fatal("preview label missing")
	}
	mounts := app["volumes"].([]any)
	if mounts[2].(map[string]any)["type"] != "bind" {
		t.Fatal("read-only repository mounts are kept")
	}
	for name, raw := range config["volumes"].(map[string]any) {
		definition := raw.(map[string]any)
		if definition["external"] != true || !strings.HasPrefix(definition["name"].(string), "cove-pv-"+previewWorkload+"-") || !strings.HasPrefix(name, "cove-preview-") {
			t.Fatalf("volume %s is not preview-owned: %v", name, definition)
		}
	}
}

func sortedCopy(values []string) []string {
	out := append([]string{}, values...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestIsolatePreviewComposeRejectsHostAccess(t *testing.T) {
	cases := map[string]string{
		"privileged":      `{"services":{"a":{"image":"x","privileged":true}}}`,
		"host network":    `{"services":{"a":{"image":"x","network_mode":"host"}}}`,
		"host pid":        `{"services":{"a":{"image":"x","pid":"host"}}}`,
		"docker socket":   `{"services":{"a":{"image":"x","volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/var/run/docker.sock"}]}}}`,
		"unplanned bind":  `{"services":{"a":{"image":"x","volumes":[{"type":"bind","source":"/srv/data","target":"/data"}]}}}`,
		"writable repo":   `{"services":{"a":{"image":"x","volumes":[{"type":"bind","source":"/r/releases/op/data","target":"/data"}]}}}`,
		"external secret": `{"services":{"a":{"image":"x"}},"secrets":{"s":{"external":true}}}`,
		"host secret":     `{"services":{"a":{"image":"x"}},"secrets":{"s":{"file":"/etc/shadow"}}}`,
	}
	for name, raw := range cases {
		if _, err := isolatePreviewCompose(previewConfig(t, raw), &EnvironmentPreview{Preview: "p"}, previewWorkload, "/r/releases"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestEnvironmentPreviewRequestValidation(t *testing.T) {
	base := func() Request {
		return Request{ID: "op1", Workload: previewWorkload, Action: "preview_data", EnvironmentPreview: &EnvironmentPreview{Preview: "p1", Source: "fedcba9876543210fedcba9876543210", Volumes: []PreviewVolume{{Key: "volume:data", Source: "prod_data", Target: "cove-pv-" + previewWorkload + "-00", Mode: "copy"}}}}
	}
	if err := ValidateRequest(base()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Request){
		"foreign target": func(r *Request) { r.EnvironmentPreview.Volumes[0].Target = "prod_data" },
		"other preview owner": func(r *Request) {
			r.EnvironmentPreview.Volumes[0].Target = "cove-pv-fedcba9876543210fedcba9876543210-00"
		},
		"copy without source": func(r *Request) { r.EnvironmentPreview.Volumes[0].Source = "" },
		"relative bind":       func(r *Request) { r.EnvironmentPreview.Volumes[0].Key = "bind:data" },
		"bind with comma":     func(r *Request) { r.EnvironmentPreview.Volumes[0].Key = "bind:/a,b" },
		"self source":         func(r *Request) { r.EnvironmentPreview.Source = previewWorkload },
		"missing source":      func(r *Request) { r.EnvironmentPreview.Source = "" },
		"backup":              func(r *Request) { r.Backup = &BackupConfig{} },
		"rollback":            func(r *Request) { r.Action = "rollback"; r.Release = "x" },
		"database volume without database": func(r *Request) {
			r.EnvironmentPreview.Volumes[0].Mode = "database"
			r.EnvironmentPreview.Volumes[0].Source = ""
		},
		"mutable image": func(r *Request) { r.EnvironmentPreview.Images = map[string]string{"app": "nginx:latest"} },
		"no ownership":  func(r *Request) { r.EnvironmentPreview = nil },
	}
	for name, mutate := range mutations {
		r := base()
		mutate(&r)
		if err := ValidateRequest(r); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPostgresCopyCounter(t *testing.T) {
	counter := newCopyCounter()
	stream := "SET statement_timeout = 0;\nCOPY public.items (id, name) FROM stdin;\n1\tone\n2\ttwo with \\n escaped newline\n\\.\nCOPY \"Odd\".\"Ta\"\"ble\" (id) FROM stdin;\n\\.\nCOPY auth.users (id) FROM stdin;\n7\n"
	// Writes split mid-line must still count correctly.
	for i := 0; i < len(stream); i += 5 {
		end := min(i+5, len(stream))
		if _, err := counter.Write([]byte(stream[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := counter.Write([]byte("8\n\\.\n")); err != nil {
		t.Fatal(err)
	}
	counts := counter.counts()
	if counts["public.items"] != 2 || counts[`Odd.Ta"ble`] != 0 || counts["auth.users"] != 2 || len(counts) != 3 {
		t.Fatalf("unexpected counts %v", counts)
	}
}

func TestMariaDBRowCounter(t *testing.T) {
	dump := "CREATE DATABASE `app`;\nUSE `app`;\nCREATE TABLE `empty` (\n  `id` int\n);\nCREATE TABLE `items` (\n  `id` int\n);\n" +
		"INSERT INTO `items` VALUES (1,'a (b)'),(2,'it''s'),(3,'back\\\\slash \\' quote'),(4,NULL);\n" +
		"INSERT INTO `items` VALUES (5,'x;y');\nINSERT INTO `logs` VALUES (1,'(nested) \"text\"');\n"
	counts, err := countMariaDBRows(strings.NewReader(dump), "app")
	if err != nil {
		t.Fatal(err)
	}
	if counts["app.items"] != 5 || counts["app.logs"] != 1 || counts["app.empty"] != 0 {
		t.Fatalf("unexpected counts %v", counts)
	}
	// MariaDB 11 writes one tuple per line after VALUES.
	modern := "USE `shop`;\nCREATE TABLE `orders` (\n  `id` int\n);\nLOCK TABLES `orders` WRITE;\nINSERT INTO `orders` VALUES\n(1,'order-1'),\n(2,'line;\\nbreak'),\n(3,'x');\nUNLOCK TABLES;\nINSERT INTO `orders` VALUES\n(4,'y');\n"
	counts, err = countMariaDBRows(strings.NewReader(modern), "shop")
	if err != nil || counts["shop.orders"] != 4 || len(counts) != 1 {
		t.Fatalf("unexpected counts for one-tuple-per-line dumps %v %v", counts, err)
	}
}

func TestProtectedRoutes(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("fixture password"), 10)
	if err != nil {
		t.Fatal(err)
	}
	auth := "preview:" + string(hash)
	var data string
	if err := ValidateRoutes([]Route{{Domain: "web-x.preview.example.com", Service: "web", Port: 80, Auth: auth}}, "service"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"preview:plaintext", "preview:$apr1$abc$def", "Bad User:" + auth[8:], "preview:" + auth[8:] + "\n"} {
		if ValidateRoutes([]Route{{Domain: "a.example.com", Service: "web", Port: 80, Auth: invalid}}, "service") == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	data, err = ingressConfig(map[string][]Route{"w": {{Domain: "web-x.preview.example.com", Service: "web", Port: 80, Auth: auth}, {Domain: "open.example.com", Service: "web", Port: 80}}})
	if err != nil {
		t.Fatal(err)
	}
	var config dynamicIngress
	if err = json.Unmarshal([]byte(data), &config); err != nil {
		t.Fatal(err)
	}
	protected := 0
	for _, router := range config.HTTP.Routers {
		if router.Rule == "Host(`web-x.preview.example.com`)" {
			if len(router.Middlewares) != 1 || len(config.HTTP.Middlewares[router.Middlewares[0]].BasicAuth.Users) != 1 || config.HTTP.Middlewares[router.Middlewares[0]].BasicAuth.Users[0] != auth {
				t.Fatal("protected route lacks its authentication middleware")
			}
			protected++
		} else if len(router.Middlewares) != 0 {
			t.Fatal("public route gained a middleware")
		}
	}
	if protected != 1 {
		t.Fatal("protected route missing")
	}
	// A live router without the middleware must not count as applied.
	for name, router := range config.HTTP.Routers {
		live := []map[string]any{}
		for other, r := range config.HTTP.Routers {
			entry := map[string]any{"name": other + "@file", "status": "enabled", "rule": r.Rule, "service": r.Service}
			if other != name {
				middlewares := []string{}
				for _, m := range r.Middlewares {
					middlewares = append(middlewares, m+"@file")
				}
				entry["middlewares"] = middlewares
			}
			live = append(live, entry)
		}
		encoded, _ := json.Marshal(live)
		accepted := ingressAccepted(string(encoded), config)
		if len(router.Middlewares) > 0 && accepted {
			t.Fatal("route without enforced authentication was accepted")
		}
	}
}
