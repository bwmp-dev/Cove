package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type ownedDNSProvider struct {
	mu              sync.Mutex
	records         map[string]cloudflareRecord
	created         int
	deleted         []string
	fail            bool
	loseCreateReply bool
	beforeGet       func(*cloudflareRecord)
}

func (p *ownedDNSProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := 200
	var result any = map[string]any{}
	id := strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/"):], "/")
	if p.fail {
		status = 503
	} else {
		switch r.Method {
		case "GET":
			if id != "dns_records" {
				record, exists := p.records[id]
				if !exists {
					status = 404
				} else {
					if p.beforeGet != nil {
						p.beforeGet(&record)
						p.records[id] = record
					}
					result = record
				}
			} else {
				list := []cloudflareRecord{}
				for _, record := range p.records {
					if name := r.URL.Query().Get("name"); name != "" && record.Name != name {
						continue
					}
					if marker := r.URL.Query().Get("comment.contains"); marker != "" && !strings.Contains(record.Comment, marker) {
						continue
					}
					list = append(list, record)
				}
				sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
				result = list
			}
		case "POST":
			var record cloudflareRecord
			if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
				return nil, err
			}
			p.created++
			record.ID = fmt.Sprintf("%032x", p.created)
			p.records[record.ID] = record
			if p.loseCreateReply {
				p.loseCreateReply = false
				return nil, errors.New("injected lost create acknowledgement")
			}
			result = record
		case "PATCH":
			record, exists := p.records[id]
			if !exists {
				status = 404
				break
			}
			var fields map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
				return nil, err
			}
			previous, _ := json.Marshal(record)
			var merged map[string]json.RawMessage
			json.Unmarshal(previous, &merged)
			for key, value := range fields {
				merged[key] = value
			}
			updated, _ := json.Marshal(merged)
			json.Unmarshal(updated, &record)
			p.records[id] = record
			result = record
		case "DELETE":
			if _, exists := p.records[id]; !exists {
				status = 404
			} else {
				p.deleted = append(p.deleted, id)
				delete(p.records, id)
			}
		default:
			return nil, fmt.Errorf("unexpected provider method %s", r.Method)
		}
	}
	data, _ := json.Marshal(map[string]any{"success": status == 200, "result": result})
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func ownedDNSFixture(t *testing.T) (*accountFixture, cloudflareZone, []byte, *ownedDNSProvider) {
	t.Helper()
	f := newAccountFixture(t)
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 8), Enabled: true, Nodes: map[string]string{"dns-node": "8.8.8.8"}}
	raw, err := f.s.sealJSON(zone)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,'bootstrap',$3)`, zone.ZoneID, zone.Domain, raw)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['*.example.test'])`, zone.ZoneID)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('dns-node','DNS node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('dns-project','DNS project','default')`)
	provider := &ownedDNSProvider{records: map[string]cloudflareRecord{}}
	f.s.CloudflareTransport = provider
	return f, zone, raw, provider
}

func addOwnedDNSWorkload(t *testing.T, f *accountFixture, id, domain string) {
	t.Helper()
	routes := []worker.Route{{Domain: domain, Service: "web", Port: 80}}
	config, err := f.s.sealJSON(workloadInput{Name: id, NodeID: "dns-node", Kind: "service", Routes: routes, Compose: "services: {}"})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO workloads(id,name,node_id,configuration,project_id,team_id) VALUES($1,$1,'dns-node',$2,'dns-project','default')`, id, config)
}

func addOwnedDNSPreview(t *testing.T, f *accountFixture, host string) {
	t.Helper()
	addOwnedDNSWorkload(t, f, "dns-preview-app", "unused.example.test")
	routes, _ := json.Marshal([]worker.Route{{Domain: host, Service: "web", Port: 80}})
	f.sql(`INSERT INTO environments(id,project_id,name) VALUES('dns-base','dns-project','Base')`)
	f.sql(`INSERT INTO environment_previews(id,project_id,team_id,base_environment_id,suffix,slug,state,stage,configuration,recipe_version)
	 VALUES('dns-preview','dns-project','default','dns-base','example.test','preview','creating','publish',$1,1)`, []byte{})
	f.sql(`INSERT INTO environment_preview_resources(preview_id,base_id,resource_id,name,kind,baseline,isolation,routes)
	 VALUES('dns-preview','base','dns-preview-app','Preview','service',$1,'{}',$2)`, []byte{}, routes)
}

func TestCloudflareDNSCleanupKeepsPendingAndDeployedRoutes(t *testing.T) {
	for _, state := range []string{"queued", "running", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			f, zone, raw, provider := ownedDNSFixture(t)
			addOwnedDNSWorkload(t, f, "app", "other.example.test")
			request := worker.Request{ID: "claim", Workload: "app", Action: "routes", Routes: []worker.Route{{Domain: "app.example.test", Service: "web", Port: 80}}}
			payload, _ := f.s.sealJSON(request)
			f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('claim','app','routes',$1,$2)`, state, payload)
			if err := f.s.reconcileDNSRecord(t.Context(), zone, "app.example.test", "8.8.8.8"); err != nil {
				t.Fatal(err)
			}
			if err := f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw); err != nil || len(provider.records) != 1 {
				t.Fatal("lost the pending or last deployed hostname", err)
			}
			if state == "succeeded" {
				request.ID = "released"
				request.Routes = nil
				payload, _ = f.s.sealJSON(request)
				f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,updated_at) VALUES('released','app','routes','succeeded',$1,now()+interval '1 second')`, payload)
			} else {
				f.sql(`UPDATE operations SET state='cancelled' WHERE id='claim'`)
			}
			if err := f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw); err != nil || len(provider.records) != 0 {
				t.Fatal("obsolete operation retained the hostname forever", err)
			}
		})
	}
}

func TestCloudflareDNSCleanupBoundsAndAuthorization(t *testing.T) {
	for _, hold := range []string{"disabled", "revoked-owner", "changed-zone", "unreadable-claims", "no-shares"} {
		t.Run(hold, func(t *testing.T) {
			f, zone, raw, provider := ownedDNSFixture(t)
			if err := f.s.reconcileDNSRecord(t.Context(), zone, "app.example.test", "8.8.8.8"); err != nil {
				t.Fatal(err)
			}
			switch hold {
			case "disabled", "changed-zone":
				zone.Enabled = false
				changed, _ := f.s.sealJSON(zone)
				f.sql(`UPDATE cloudflare_zones SET configuration=$1`, changed)
				if hold == "disabled" {
					raw = changed
				}
			case "revoked-owner":
				f.sql(`UPDATE cloudflare_zones SET owner_id=NULL`)
			case "unreadable-claims":
				addOwnedDNSWorkload(t, f, "app", "other.example.test")
				f.sql(`UPDATE workloads SET configuration=''::bytea WHERE id='app'`)
			case "no-shares":
				f.sql(`DELETE FROM cloudflare_zone_teams`)
			}
			var err error
			if hold == "no-shares" {
				err = f.s.reconcileDNS(t.Context())
			} else {
				err = f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw)
			}
			if hold == "unreadable-claims" {
				if err == nil || len(provider.records) != 1 {
					t.Fatal("unknown route inventory did not fail closed", err)
				}
			} else if hold == "no-shares" {
				if err != nil || len(provider.records) != 0 {
					t.Fatal("last share removal stranded a proven orphan", err)
				}
			} else if err != nil || len(provider.records) != 1 {
				t.Fatal("unauthorized or changed zone performed cleanup", err)
			}
		})
	}
	f, zone, raw, provider := ownedDNSFixture(t)
	for index := range 40 {
		host := fmt.Sprintf("app%d.example.test", index)
		if err := f.s.reconcileDNSRecord(t.Context(), zone, host, "8.8.8.8"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw); err != nil || len(provider.deleted) != 32 || len(provider.records) != 8 {
		t.Fatal("cleanup exceeded its per-pass mutation bound", err, len(provider.deleted))
	}
	if err := f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw); err != nil || len(provider.records) != 0 {
		t.Fatal("later pass did not finish bounded cleanup", err)
	}
}

func TestCloudflareOwnedDNSCleanupLifecycle(t *testing.T) {
	for _, removal := range []string{"route", "workload", "removed-registry", "preview-orphan", "lost-create-acknowledgement"} {
		t.Run(removal, func(t *testing.T) {
			f, zone, raw, provider := ownedDNSFixture(t)
			ctx := t.Context()
			host := "app.example.test"
			if removal != "lost-create-acknowledgement" && removal != "preview-orphan" {
				addOwnedDNSWorkload(t, f, "app", host)
			}
			if removal == "preview-orphan" {
				addOwnedDNSPreview(t, f, host)
			}
			provider.loseCreateReply = removal == "lost-create-acknowledgement"
			err := f.s.reconcileDNSRecord(ctx, zone, host, "8.8.8.8")
			if (err != nil) != (removal == "lost-create-acknowledgement") {
				t.Fatal("unexpected create acknowledgement", err)
			}
			if len(provider.records) != 1 || provider.created != 1 {
				t.Fatal("record not created")
			}
			if removal != "lost-create-acknowledgement" && removal != "preview-orphan" {
				if err := f.s.reconcileDNSCleanup(ctx, zone.ZoneID, raw); err != nil || len(provider.records) != 1 {
					t.Fatal("deleted a routed hostname", err)
				}
			}
			switch removal {
			case "route":
				config, _ := f.s.sealJSON(workloadInput{Name: "app", NodeID: "dns-node", Kind: "service", Compose: "services: {}"})
				f.sql(`UPDATE workloads SET configuration=$1 WHERE id='app'`, config)
			case "workload":
				f.sql(`DELETE FROM workloads WHERE id='app'`)
			case "preview-orphan":
				if err := f.s.reconcileDNSCleanup(ctx, zone.ZoneID, raw); err != nil || len(provider.records) != 1 {
					t.Fatal("deleted a reserved preview hostname before route submission", err)
				}
				f.sql(`DELETE FROM environment_previews WHERE id='dns-preview'`)
				f.sql(`DELETE FROM workloads WHERE id='dns-preview-app'`)
			case "removed-registry":
				f.sql(`INSERT INTO managed_registries(id,team_id,hostname,state,credentials,retention) VALUES('app','default',$1,'removed',''::bytea,'{}')`, host)
			}
			if err := f.s.reconcileDNS(ctx); err != nil || len(provider.records) != 0 || len(provider.deleted) != 1 {
				t.Fatal("orphan survived ordinary reconciliation", err, provider.records, provider.deleted)
			}
		})
	}
}

func TestCloudflareDNSCleanupRespectsProviderEditsAndRetry(t *testing.T) {
	for _, change := range []string{"unmarked", "edited-comment", "edited-content", "fresh-read-edit", "key-rotation", "provider-outage"} {
		t.Run(change, func(t *testing.T) {
			f, zone, raw, provider := ownedDNSFixture(t)
			if err := f.s.reconcileDNSRecord(t.Context(), zone, "app.example.test", "8.8.8.8"); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("%032x", 1)
			record := provider.records[id]
			switch change {
			case "unmarked":
				record.Comment = ""
			case "edited-comment":
				record.Comment = "Keep this record"
			case "edited-content":
				record.Content = "8.8.4.4"
			case "fresh-read-edit":
				provider.beforeGet = func(r *cloudflareRecord) { r.Comment = "Keep this record" }
			case "key-rotation":
				f.s.Key = bytes.Repeat([]byte{3}, 32)
				raw, _ = f.s.sealJSON(zone)
				f.sql(`UPDATE cloudflare_zones SET configuration=$1`, raw)
			case "provider-outage":
				provider.fail = true
			}
			provider.records[id] = record
			err := f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw)
			if change == "provider-outage" {
				if err == nil {
					t.Fatal("outage was hidden")
				}
				provider.fail = false
				if err = f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw); err != nil || len(provider.records) != 0 {
					t.Fatal("retry lost orphan ownership", err)
				}
			} else if err != nil || len(provider.records) != 1 || len(provider.deleted) != 0 {
				t.Fatal("changed or unowned record deleted or retried forever", err, provider.deleted)
			}
		})
	}
}

func TestCloudflareHostnameTransferHasNoDeleteCreateGap(t *testing.T) {
	f, zone, raw, provider := ownedDNSFixture(t)
	addOwnedDNSWorkload(t, f, "source", "app.example.test")
	if err := f.s.reconcileDNSRecord(t.Context(), zone, "app.example.test", "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	f.sql(`DELETE FROM workloads WHERE id='source'`)
	addOwnedDNSWorkload(t, f, "destination", "app.example.test")
	f.sql(`INSERT INTO teams(id,name) VALUES('other-team','Other team')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('other-project','Other project','other-team')`)
	f.sql(`UPDATE workloads SET team_id='other-team',project_id='other-project' WHERE id='destination'`)
	if err := f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw); err != nil {
		t.Fatal(err)
	}
	if err := f.s.reconcileDNSRecord(t.Context(), zone, "app.example.test", "8.8.4.4"); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%032x", 1)
	if provider.created != 1 || len(provider.deleted) != 0 || provider.records[id].Content != "8.8.4.4" || !f.s.ownsDNSRecord(zone, provider.records[id]) {
		t.Fatal("transfer replaced DNS or lost ownership", provider.created, provider.deleted)
	}
}

func TestCloudflareCleanupWaitsForHostnameClaimTransaction(t *testing.T) {
	f, zone, raw, provider := ownedDNSFixture(t)
	if err := f.s.reconcileDNSRecord(t.Context(), zone, "app.example.test", "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1)`, routeClaimLock); err != nil {
		t.Fatal(err)
	}
	config, _ := f.s.sealJSON(workloadInput{Name: "Destination", NodeID: "dns-node", Kind: "service", Routes: []worker.Route{{Domain: "app.example.test", Service: "web", Port: 80}}})
	if _, err = tx.Exec(t.Context(), `INSERT INTO workloads(id,name,node_id,configuration,project_id,team_id) VALUES('destination','Destination','dns-node',$1,'dns-project','default')`, config); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw) }()
	blocked := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err = f.s.DB.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted)`, routeClaimLock).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatal("cleanup bypassed the hostname claim lock", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !blocked {
		t.Fatal("cleanup never reached the hostname claim lock")
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil || len(provider.records) != 1 || len(provider.deleted) != 0 {
		t.Fatal("cleanup missed the committed destination claim", err, provider.deleted)
	}
}

func TestCloudflarePreviewDeletionRetriesDNSAfterWorkerRemoval(t *testing.T) {
	f := newPreviewFixture(t)
	recipe := f.setup()
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "preview.example.com", Token: strings.Repeat("fixture", 8), Enabled: true, Nodes: map[string]string{"n1": "8.8.8.8"}}
	raw, err := f.s.sealJSON(zone)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,'bootstrap',$3)`, zone.ZoneID, zone.Domain, raw)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['*.preview.example.com'])`, zone.ZoneID)
	provider := &ownedDNSProvider{records: map[string]cloudflareRecord{}}
	f.s.CloudflareTransport = provider
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	f.branches["101:staging/cleanup"] = sha("cleanup")
	if status := f.push("dns-cleanup", 101, "staging/cleanup", false, 41); status != 202 {
		t.Fatal("preview signal refused", status)
	}
	f.settle()
	var id, slug, state string
	if err = f.s.DB.QueryRow(t.Context(), `SELECT id,slug,state FROM environment_previews WHERE suffix='cleanup'`).Scan(&id, &slug, &state); err != nil || state != "ready" || len(provider.records) == 0 {
		t.Fatal("preview did not publish owned DNS", state, err)
	}
	for _, record := range provider.records {
		if !f.s.ownsDNSRecord(zone, record) {
			t.Fatal("preview created unowned DNS unexpectedly")
		}
	}
	manualID := strings.Repeat("f", 32)
	provider.records[manualID] = cloudflareRecord{ID: manualID, Name: "manual.preview.example.com", Type: "A", Content: "8.8.8.8", TTL: 1}
	provider.fail = true
	f.call("root", "default", "DELETE", "/api/environment-previews/"+id, map[string]string{"confirm": slug}, 202)
	f.settle()
	var count int
	if err = f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM environment_previews WHERE id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("provider outage blocked worker/record removal", count, err)
	}
	if err = f.s.reconcileDNS(t.Context()); err != nil {
		t.Fatal(err)
	}
	var warning string
	if err = f.s.DB.QueryRow(t.Context(), `SELECT last_error FROM cloudflare_zones WHERE zone_id=$1`, zone.ZoneID).Scan(&warning); err != nil || warning == "" {
		t.Fatal("deferred provider cleanup failure was not visible", warning, err)
	}
	provider.fail = false
	f.sql(`UPDATE cloudflare_zones SET checked_at=NULL`)
	if err = f.s.reconcileDNS(t.Context()); err != nil || len(provider.records) != 1 || provider.records[manualID].ID != manualID || len(provider.deleted) == 0 {
		t.Fatal("retry failed or deleted an unmarked record", err, provider.records)
	}
}

func TestCloudflareDNSCleanupPreservesPanelHostname(t *testing.T) {
	f, zone, raw, provider := ownedDNSFixture(t)
	f.s.PublicURL = "https://APP.example.test/control/cove"
	if err := f.s.reconcileDNSRecord(t.Context(), zone, "app.example.test", "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.reconcileDNSCleanup(t.Context(), zone.ZoneID, raw); err != nil || len(provider.records) != 1 || len(provider.deleted) != 0 {
		t.Fatal("cleanup removed the panel's hostname after its resource was deleted", err)
	}
}
