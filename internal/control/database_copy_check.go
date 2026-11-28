package control

import (
	"errors"
	"net/http"
	"strings"

	"cove/internal/worker"
)

func databaseCopyCompatibility(source, target *databaseResource, actual *worker.DatabaseInspection) error {
	if !strings.HasPrefix(actual.Version, strings.Split(source.Version, ".")[0]+".") {
		return errors.New("running source version differs from the reviewed configuration")
	}
	if target.Variant == "postgis" && actual.Arch != "x86_64" && actual.Arch != "amd64" {
		return errors.New("the pinned PostGIS image supports Linux amd64 only")
	}
	if source.Engine == "mongodb" && actual.FeatureCompatibility != "7.0" {
		return errors.New("MongoDB copy requires verified source feature compatibility 7.0")
	}
	for _, extension := range actual.Extensions {
		if extension == "plpgsql" || extension == "pg_trgm" || extension == "hstore" || extension == "uuid-ossp" || extension == "pg_stat_statements" {
			continue
		}
		if extension == "vector" && target.Variant == "vector" {
			continue
		}
		if target.Variant == "postgis" && (extension == "postgis" || extension == "postgis_topology" || extension == "fuzzystrmatch" || extension == "postgis_tiger_geocoder") {
			continue
		}
		return errors.New("target image does not provide every installed source extension; choose a compatible variant")
	}
	return nil
}

func databaseCopyTargets(source *databaseResource) []databaseVersion {
	engine, ok := findDatabaseEngine(source.Engine)
	if !ok {
		return []databaseVersion{}
	}
	definition, _, err := databaseTemplate(engine)
	if err != nil {
		return []databaseVersion{}
	}
	options := append([]databaseVersion{{Version: databaseImageVersion(definition.Images[engine.service]), Image: definition.Images[engine.service]}}, databaseAdditionalVersions(engine.id)...)
	result := []databaseVersion{}
	for _, option := range options {
		if databaseCopyAllowed(source, option.Version) {
			result = append(result, option)
		}
	}
	return result
}

func (s *Server) databasePreflight(w http.ResponseWriter, r *http.Request) {
	var in databasePreflightInput
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	var raw []byte
	var node string
	var deployed bool
	if err := s.DB.QueryRow(r.Context(), `SELECT configuration,node_id,NOT manually_stopped AND EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state='succeeded' AND action IN ('deploy','rollback')) FROM workloads w WHERE id=$1 AND team_id=$2`, id, actor(r).Team).Scan(&raw, &node, &deployed); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if in.Revision != databaseRevision(raw) {
		http.Error(w, "database changed; review its current configuration", 409)
		return
	}
	var config workloadInput
	if s.unsealJSON(raw, &config) != nil || config.Database == nil {
		http.Error(w, "select a managed database", 400)
		return
	}
	if !deployed || s.databaseLocked(r.Context(), id) {
		http.Error(w, "deploy the database and finish maintenance before compatibility checks", 409)
		return
	}
	reply, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "database_inspection", Database: &worker.DatabaseInspectionRequest{Workload: id, Engine: config.Database.Engine, Service: config.Database.Service}})
	if err != nil || reply.Database == nil {
		http.Error(w, "database compatibility inspection unavailable", 502)
		return
	}
	if in.Version != "" {
		if !databaseCopyAllowed(config.Database, in.Version) {
			http.Error(w, "select a supported copy target", 400)
			return
		}
		target := &databaseResource{Version: in.Version, Engine: config.Database.Engine}
		for _, option := range databaseCopyTargets(config.Database) {
			if option.Version == in.Version {
				target.Variant = option.Variant
			}
		}
		if err = databaseCopyCompatibility(config.Database, target, reply.Database); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	}
	respond(w, 200, map[string]any{"revision": in.Revision, "runtime": reply.Database, "copy_targets": databaseCopyTargets(config.Database), "backup_required": true, "scope": "Only the app database is copied. Cluster accounts, grants and other databases are excluded. The source stays intact. Writes after the snapshot are not replicated; stop application writes before final cut-over."})
}

type databasePreflightInput struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
}
