package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"cove/internal/catalog"
	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type objectBucketConfig struct {
	BackupDestination bool                `json:"backup_destination"`
	Request           worker.ObjectBucket `json:"request"`
	Endpoint          string              `json:"endpoint"`
}

func (s *Server) objectBuckets(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,storage_resource_id,COALESCE(storage_id,''),configuration,state,created_at FROM object_buckets WHERE workload_id=$1 ORDER BY created_at`, id)
		if err != nil {
			http.Error(w, "buckets unavailable", 503)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var bucket, storage, connection, state string
			var raw []byte
			var created time.Time
			if rows.Scan(&bucket, &storage, &connection, &raw, &state, &created) != nil {
				http.Error(w, "buckets unavailable", 503)
				return
			}
			var config objectBucketConfig
			if s.unsealJSON(raw, &config) != nil {
				http.Error(w, "bucket configuration unavailable", 503)
				return
			}
			out = append(out, map[string]any{"id": bucket, "storage_resource_id": storage, "storage_id": connection, "bucket": config.Request.Bucket, "endpoint": config.Endpoint, "region": "garage", "state": state, "created_at": created, "warning": "Storage on the same node is not offsite. Keep an independent backup on another failure domain."})
		}
		if rows.Err() != nil {
			http.Error(w, "buckets unavailable", 503)
			return
		}
		respond(w, 200, out)
		return
	}
	if !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator required to provision scoped storage credentials", 403)
		return
	}
	if r.Method == "DELETE" {
		s.removeObjectBucket(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/retry") {
		s.completeObjectBucket(w, r, r.PathValue("bucket"))
		return
	}
	var in objectBucketInput
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm || in.Storage == id {
		http.Error(w, "select separate Garage storage and acknowledge that same-node storage is not offsite", 400)
		return
	}
	if !s.requireAccess(w, r, "workload", in.Storage, 3) || !s.requirePermission(w, r, "workload", in.Storage, "read_secrets") {
		return
	}
	var checkedRaw []byte
	var checkedNode string
	var ready bool
	if err := s.DB.QueryRow(r.Context(), `SELECT w.configuration,w.node_id,COALESCE((n.capabilities->>'managed_databases')::boolean,false) AND NOT w.manually_stopped AND EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state='succeeded' AND action IN ('deploy','rollback')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2`, in.Storage, actor(r).Team).Scan(&checkedRaw, &checkedNode, &ready); err != nil {
		http.Error(w, "storage resource unavailable", 503)
		return
	}
	var checkedStorage workloadInput
	if s.unsealJSON(checkedRaw, &checkedStorage) != nil {
		http.Error(w, "storage configuration unavailable", 503)
		return
	}
	token := checkedStorage.Environment["ADMIN_TOKEN"]
	_, tokenErr := hex.DecodeString(token)
	if !ready || len(token) != 64 || tokenErr != nil || token != strings.ToLower(token) {
		http.Error(w, "deploy Garage with its generated administration token on an upgraded worker before provisioning a bucket", 409)
		return
	}
	if _, err := s.nodeRPC(r.Context(), checkedNode, worker.RPCRequest{Method: "object_bucket", Bucket: &worker.ObjectBucket{Workload: in.Storage, AdminToken: token, CheckOnly: true}}); err != nil {
		http.Error(w, "deploy Garage with its saved administration token and verify its health before provisioning a bucket", 409)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	var node string
	if err = tx.QueryRow(r.Context(), `SELECT configuration,node_id FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE`, in.Storage, actor(r).Team).Scan(&raw, &node); err != nil {
		http.Error(w, "storage resource unavailable", 503)
		return
	}
	if databaseRevision(raw) != databaseRevision(checkedRaw) || node != checkedNode {
		http.Error(w, "Garage configuration changed; review and retry provisioning", 409)
		return
	}
	var storage workloadInput
	if s.unsealJSON(raw, &storage) != nil {
		http.Error(w, "storage configuration unavailable", 503)
		return
	}
	index, _, err := catalog.Builtin()
	if err != nil {
		http.Error(w, "catalogue unavailable", 503)
		return
	}
	var compose string
	for _, t := range index.Templates {
		if t.ID == "garage" {
			compose = t.Compose
		}
	}
	if storage.Compose != compose || storage.Source != nil || len(storage.Environment["ADMIN_TOKEN"]) != 64 {
		http.Error(w, "select an unmodified Garage catalogue resource with its generated administration token", 409)
		return
	}
	domain := strings.ToLower(strings.TrimSpace(in.Domain))
	found := false
	for _, route := range storage.Routes {
		if route.IsPublicHTTPRoute() && !strings.Contains(domain, "*") && route.Domain == domain && route.Service == "garage" && route.Port == 3900 {
			found = true
		}
	}
	if !found {
		http.Error(w, "add and deploy a public exact HTTPS route for the Garage service on port 3900, then select that hostname", 409)
		return
	}
	var capable, preview bool
	if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'managed_databases')::boolean,false) AND NOT w.manually_stopped,EXISTS(SELECT 1 FROM previews WHERE workload_id=w.id) OR EXISTS(SELECT 1 FROM environment_preview_resources WHERE resource_id=w.id) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, in.Storage).Scan(&capable, &preview); err != nil || !capable || preview {
		http.Error(w, "select an upgraded worker and running non-preview Garage resource", 409)
		return
	}
	var targetRaw []byte
	if err = tx.QueryRow(r.Context(), `SELECT configuration FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE`, id, actor(r).Team).Scan(&targetRaw); err != nil {
		http.Error(w, "consumer unavailable", 503)
		return
	}
	var target workloadInput
	if s.unsealJSON(targetRaw, &target) != nil || target.Database != nil {
		http.Error(w, "select an application workload", 400)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id IN ($1,$2) AND state IN ('queued','running'))", id, in.Storage).Scan(&active); err != nil {
		http.Error(w, "operations unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for application and Garage operations before provisioning a bucket", 409)
		return
	}
	for _, key := range objectBucketKeys {
		if _, exists := target.Environment[key]; exists {
			http.Error(w, "remove existing S3 variables before provisioning a bound bucket", 409)
			return
		}
	}
	bucket := resourceID()
	bytes := make([]byte, 44)
	if _, err = rand.Read(bytes); err != nil {
		http.Error(w, "could not generate scoped credentials", 503)
		return
	}
	config := objectBucketConfig{BackupDestination: in.Backup, Endpoint: "https://" + domain, Request: worker.ObjectBucket{Workload: in.Storage, Bucket: "cove-" + bucket, AccessKey: "GK" + hex.EncodeToString(bytes[:12]), SecretKey: hex.EncodeToString(bytes[12:]), AdminToken: storage.Environment["ADMIN_TOKEN"]}}
	raw, err = s.sealJSON(config)
	if err != nil {
		http.Error(w, "could not protect bucket credentials", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO object_buckets(id,workload_id,storage_resource_id,storage_id,configuration) VALUES($1,$2,$3,NULLIF($4,''),$5)`, bucket, id, in.Storage, "", raw); err != nil {
		http.Error(w, "a bucket is already configured for this workload", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'provision object bucket',$2,202,$3)`, actor(r).ID, id, actor(r).Team); err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "could not save bucket request", 503)
		return
	}
	s.completeObjectBucket(w, r, bucket)
}

var objectBucketKeys = []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_REGION"}

func (s *Server) completeObjectBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	var raw []byte
	var storage, node string
	err := s.DB.QueryRow(r.Context(), `SELECT b.configuration,b.storage_resource_id,w.node_id FROM object_buckets b JOIN workloads w ON w.id=b.storage_resource_id WHERE b.id=$1 AND b.workload_id=$2`, bucket, r.PathValue("id")).Scan(&raw, &storage, &node)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "bucket unavailable", 503)
		return
	}
	if !s.requireAccess(w, r, "workload", storage, 3) {
		return
	}
	var config objectBucketConfig
	if s.unsealJSON(raw, &config) != nil {
		http.Error(w, "bucket configuration unavailable", 503)
		return
	}
	if _, err = s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "object_bucket", Bucket: &config.Request}); err != nil {
		http.Error(w, "bucket provisioning was not confirmed; retry the retained request", 502)
		return
	}
	if err = s.bindObjectBucket(r.Context(), bucket, r.PathValue("id"), storage, config); err != nil {
		http.Error(w, "bucket exists; retry to finish variable bindings", 409)
		return
	}
	respond(w, 201, map[string]string{"id": bucket, "state": "ready"})
}

func (s *Server) bindObjectBucket(ctx context.Context, bucket, target, storage string, c objectBucketConfig) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err = tx.QueryRow(ctx, "SELECT state FROM object_buckets WHERE id=$1 FOR UPDATE", bucket).Scan(&state); err != nil {
		return err
	}
	if state == "ready" {
		return nil
	}
	if _, err = tx.Exec(ctx, "UPDATE object_buckets SET state='ready' WHERE id=$1", bucket); err != nil {
		return err
	}
	configs := map[string]workloadInput{}
	for _, id := range []string{storage, target} {
		var raw []byte
		var config workloadInput
		if err = tx.QueryRow(ctx, "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", id).Scan(&raw); err != nil {
			return err
		}
		if err = s.unsealJSON(raw, &config); err != nil {
			return err
		}
		if config.Environment == nil {
			config.Environment = map[string]string{}
		}
		configs[id] = config
	}
	source, dest := configs[storage], configs[target]
	values := []string{c.Endpoint, c.Request.Bucket, c.Request.AccessKey, c.Request.SecretKey, "garage"}
	for i, key := range objectBucketKeys {
		if _, exists := dest.Environment[key]; exists {
			return errors.New("consumer S3 variables changed while provisioning")
		}
		sourceKey := "COVE_BUCKET_" + bucket + "_" + key
		source.Environment[sourceKey] = values[i]
		dest.Environment[key] = "{{resource." + key + "}}"
		if _, err = tx.Exec(ctx, `INSERT INTO resource_variable_bindings(workload_id,name,source_id,source_key) VALUES($1,$2,$3,$4)`, target, key, storage, sourceKey); err != nil {
			return err
		}
	}
	for id, config := range map[string]workloadInput{storage: source, target: dest} {
		raw, err := s.sealJSON(config)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE workloads SET configuration=$2 WHERE id=$1", id, raw); err != nil {
			return err
		}
	}
	connection := ""
	if c.BackupDestination {
		connection = resourceID()
		config := connectionConfig{Name: "Garage bucket " + bucket, Storage: &worker.StorageLocation{Protocol: "s3", Endpoint: c.Endpoint, Path: c.Request.Bucket, AccessKey: c.Request.AccessKey, SecretKey: c.Request.SecretKey, Region: "garage"}}
		raw, err := s.sealJSON(config)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO infrastructure_connections(id,team_id,name,kind,configuration) SELECT $1,team_id,$2,'storage',$3 FROM workloads WHERE id=$4`, connection, config.Name, raw, target); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE object_buckets SET state='ready',storage_id=NULLIF($2,'') WHERE id=$1", bucket, connection); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) removeObjectBucket(w http.ResponseWriter, r *http.Request) {
	var authorizedStorage string
	if err := s.DB.QueryRow(r.Context(), "SELECT storage_resource_id FROM object_buckets WHERE id=$1 AND workload_id=$2", r.PathValue("bucket"), r.PathValue("id")).Scan(&authorizedStorage); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "bucket unavailable", 503)
		}
		return
	}
	if !s.requireAccess(w, r, "workload", authorizedStorage, 3) {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		http.Error(w, "confirm key revocation; bucket data will be retained", 400)
		return
	}
	proxy, err := s.outboundConfig(r.Context())
	if err != nil {
		http.Error(w, "worker transport configuration unavailable", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	var storage, connection, node string
	err = tx.QueryRow(r.Context(), `SELECT b.configuration,b.storage_resource_id,COALESCE(b.storage_id,''),w.node_id FROM object_buckets b JOIN workloads w ON w.id=b.storage_resource_id WHERE b.id=$1 AND b.workload_id=$2 FOR UPDATE OF b`, r.PathValue("bucket"), r.PathValue("id")).Scan(&raw, &storage, &connection, &node)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "bucket unavailable", 503)
		return
	}
	if storage != authorizedStorage {
		http.Error(w, "bucket changed; review it again", 409)
		return
	}
	if connection != "" {
		var locked string
		if err = tx.QueryRow(r.Context(), "SELECT id FROM infrastructure_connections WHERE id=$1 FOR UPDATE", connection).Scan(&locked); err != nil {
			http.Error(w, "destination unavailable", 503)
			return
		}
		rows, err := tx.Query(r.Context(), "SELECT configuration FROM workloads WHERE team_id=$1", actor(r).Team)
		if err != nil {
			http.Error(w, "backup policies unavailable", 503)
			return
		}
		used := false
		for rows.Next() {
			var enc []byte
			var config workloadInput
			if rows.Scan(&enc) != nil || s.unsealJSON(enc, &config) != nil {
				rows.Close()
				http.Error(w, "backup policies unavailable", 503)
				return
			}
			if config.Backup != nil && config.Backup.StorageID == connection {
				used = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			http.Error(w, "backup policies unavailable", 503)
			return
		}
		if used {
			http.Error(w, "move backup policies to another destination before revoking this key", 409)
			return
		}
	}
	var config objectBucketConfig
	if s.unsealJSON(raw, &config) != nil {
		http.Error(w, "bucket configuration unavailable", 503)
		return
	}
	var others bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM resource_variable_bindings WHERE source_id=$1 AND workload_id<>$2 AND starts_with(source_key,$3))`, storage, r.PathValue("id"), "COVE_BUCKET_"+r.PathValue("bucket")+"_").Scan(&others); err != nil || others {
		http.Error(w, "remove other workload bindings to this bucket before revoking it", 409)
		return
	}
	config.Request.Revoke = true
	if _, err = s.nodeRPCUsing(r.Context(), tx, node, worker.RPCRequest{Method: "object_bucket", Bucket: &config.Request}, proxy); err != nil {
		http.Error(w, "key revocation was not confirmed; retry before removing the record", 502)
		return
	}
	for _, id := range []string{storage, r.PathValue("id")} {
		var raw []byte
		var current workloadInput
		if err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", id).Scan(&raw); err != nil || s.unsealJSON(raw, &current) != nil {
			http.Error(w, "configuration unavailable; retry key cleanup", 503)
			return
		}
		for _, key := range objectBucketKeys {
			sourceKey := "COVE_BUCKET_" + r.PathValue("bucket") + "_" + key
			if id == storage {
				delete(current.Environment, sourceKey)
			} else if current.Environment[key] == "{{resource."+key+"}}" {
				delete(current.Environment, key)
			}
		}
		raw, err = s.sealJSON(current)
		if err != nil {
			http.Error(w, "configuration unavailable", 503)
			return
		}
		if _, err = tx.Exec(r.Context(), "UPDATE workloads SET configuration=$2 WHERE id=$1", id, raw); err != nil {
			http.Error(w, "configuration unavailable; retry key cleanup", 503)
			return
		}
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM resource_variable_bindings WHERE workload_id=$1 AND source_id=$2 AND starts_with(source_key,$3)`, r.PathValue("id"), storage, "COVE_BUCKET_"+r.PathValue("bucket")+"_"); err != nil {
		http.Error(w, "bindings unavailable", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM object_buckets WHERE id=$1", r.PathValue("bucket")); err != nil {
		http.Error(w, "bucket unavailable", 503)
		return
	}
	if connection != "" {
		if _, err = tx.Exec(r.Context(), "DELETE FROM infrastructure_connections WHERE id=$1", connection); err != nil {
			http.Error(w, "destination is still in use; retry cleanup after removing its attachments", 409)
			return
		}
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'revoke object bucket key',$2,200,$3)`, actor(r).ID, r.PathValue("id"), actor(r).Team); err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "cleanup was not recorded; retry", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true, "data_retained": true})
}

type objectBucketInput struct {
	Storage string `json:"storage_resource_id"`
	Domain  string `json:"domain"`
	Confirm bool   `json:"confirm_same_node"`
	Backup  bool   `json:"backup_destination"`
}
