package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// EnvironmentPreview marks an operation on a resource that belongs to a coordinated environment
// preview. Every storage mount of such a resource is rewritten to a volume the preview owns, so a
// preview never mounts, writes or deletes the production resource's storage.
type EnvironmentPreview struct {
	Preview       string            `json:"preview"`
	Source        string            `json:"source,omitempty"`
	Volumes       []PreviewVolume   `json:"volumes,omitempty"`
	Disabled      []string          `json:"disabled,omitempty"`
	Database      *PreviewDatabase  `json:"database,omitempty"`
	Networks      []string          `json:"networks,omitempty"`
	Images        map[string]string `json:"images,omitempty"`
	Refresh       bool              `json:"refresh,omitempty"`
	ImageRegistry string            `json:"image_registry,omitempty"`
}

// PreviewVolume maps one storage mount of the production release to a preview-owned volume.
// Key is "volume:<Compose volume>" or "bind:<absolute host path>".
type PreviewVolume struct {
	Key    string `json:"key"`
	Source string `json:"source,omitempty"`
	Target string `json:"target"`
	Mode   string `json:"mode"`
}

type PreviewDatabase struct {
	Engine  string `json:"engine"`
	Service string `json:"service"`
	Name    string `json:"name"`
}

type PreviewVolumeReport struct {
	Target string `json:"target"`
	Mode   string `json:"mode"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
	Digest string `json:"digest,omitempty"`
}

type PreviewDatabaseReport struct {
	Engine     string           `json:"engine"`
	SnapshotAt time.Time        `json:"snapshot_at"`
	DumpBytes  int64            `json:"dump_bytes"`
	Tables     int              `json:"tables,omitempty"`
	Rows       int64            `json:"rows,omitempty"`
	Counts     map[string]int64 `json:"counts,omitempty"`
	Verified   bool             `json:"verified,omitempty"`
}

// PreviewReport is recorded on preview data and seeding operations.
type PreviewReport struct {
	Volumes  []PreviewVolumeReport  `json:"volumes,omitempty"`
	Database *PreviewDatabaseReport `json:"database,omitempty"`
	Seeded   *PreviewDatabaseReport `json:"seeded,omitempty"`
}

var previewVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
var composeVolumeKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func previewTargetPattern(workload string) *regexp.Regexp {
	return regexp.MustCompile(`^cove-pv-` + regexp.QuoteMeta(workload) + `-[0-9a-z]{1,16}$`)
}

// PreviewVolumeTarget names the preview-owned volume for a mount key that the plan did not list,
// for example a volume a branch adds. It is stable so redeploys reuse the same volume.
func PreviewVolumeTarget(workload, key string) string {
	sum := sha256.Sum256([]byte(key))
	return "cove-pv-" + workload + "-x" + hex.EncodeToString(sum[:4])
}

func validPreviewBind(path string) bool {
	return filepath.IsAbs(path) && strings.HasPrefix(path, "/") && filepath.ToSlash(filepath.Clean(path)) == path && !strings.ContainsAny(path, ",\x00\r\n") && path != "/"
}

func validateEnvironmentPreview(r Request) error {
	p := r.EnvironmentPreview
	if p == nil {
		if r.Action == "preview_data" || r.Action == "environment_preview_remove" {
			return errors.New("environment preview ownership required")
		}
		return nil
	}
	if p.ImageRegistry != "" {
		if r.Action != "deploy" || r.Source != nil || r.Kind == "job" || len(r.ImagePins) == 0 {
			return errors.New("CI preview images require an image-only service deployment")
		}
		if err := ValidatePreviewRegistryImages(r.ImagePins, p.ImageRegistry); err != nil {
			return err
		}
		if r.Registry != nil && r.Registry.Server != p.ImageRegistry {
			return errors.New("CI preview registry credentials do not match its image scope")
		}
	}
	if !identifier.MatchString(p.Preview) {
		return errors.New("invalid environment preview")
	}
	if r.Preview || r.Private != nil || r.Backup != nil || (r.GuardedDeploy != nil && *r.GuardedDeploy) {
		return errors.New("environment previews cannot use pull request previews, Tailscale access, backups or guarded deployment")
	}
	switch r.Action {
	case "deploy", "stop", "start", "restart", "routes", "preview_data", "environment_preview_remove":
	default:
		return errors.New("unsupported environment preview operation")
	}
	if p.Source != "" && (!workloadIdentifier.MatchString(p.Source) || p.Source == r.Workload) {
		return errors.New("invalid environment preview source")
	}
	if r.Action == "preview_data" && p.Source == "" {
		return errors.New("environment preview data requires its production source")
	}
	if len(p.Volumes) > 64 || len(p.Disabled) > 64 || len(p.Networks) > 32 || len(p.Images) > 64 {
		return errors.New("environment preview plan is too large")
	}
	target := previewTargetPattern(r.Workload)
	keys, targets := map[string]bool{}, map[string]bool{}
	databaseVolumes := 0
	for _, v := range p.Volumes {
		kind, value, _ := strings.Cut(v.Key, ":")
		switch kind {
		case "volume":
			if !composeVolumeKey.MatchString(value) {
				return errors.New("invalid preview volume key")
			}
		case "bind":
			if !validPreviewBind(value) {
				return errors.New("invalid preview host mount")
			}
		default:
			return errors.New("invalid preview mount key")
		}
		if !target.MatchString(v.Target) || keys[v.Key] || targets[v.Target] {
			return errors.New("invalid or duplicate preview volume")
		}
		keys[v.Key], targets[v.Target] = true, true
		switch v.Mode {
		case "copy":
			if kind == "volume" && !previewVolumeName.MatchString(v.Source) {
				return errors.New("preview copies require the production volume name")
			}
			if kind == "bind" && v.Source != value {
				return errors.New("preview host mount copies must name their host path")
			}
		case "empty":
			if v.Source != "" {
				return errors.New("empty preview volumes have no source")
			}
		case "database":
			if v.Source != "" || p.Database == nil {
				return errors.New("database volumes require a database copy")
			}
			databaseVolumes++
		default:
			return errors.New("unsupported preview volume policy")
		}
	}
	if p.Database != nil {
		if extendedDatabase(p.Database.Engine) {
			return errors.New("environment preview database refresh supports PostgreSQL and MariaDB; use an isolated backup restore copy for this engine")
		}
		if err := validateDatabaseBackup(&DatabaseBackup{Engine: p.Database.Engine, Service: p.Database.Service, Name: p.Database.Name}); err != nil {
			return err
		}
		if databaseVolumes == 0 {
			return errors.New("a database copy needs a preview-owned database volume")
		}
	}
	seen := map[string]bool{}
	for _, name := range p.Disabled {
		if !serviceIdentifier.MatchString(name) || seen[name] {
			return errors.New("invalid disabled preview service")
		}
		seen[name] = true
	}
	for _, id := range p.Networks {
		if !networkID.MatchString(id) {
			return errors.New("invalid preview network")
		}
	}
	for name, image := range p.Images {
		if !serviceIdentifier.MatchString(name) || !imageDigest.MatchString(image) {
			return errors.New("preview baseline images must be immutable image IDs")
		}
	}
	return nil
}

var unsafePreviewOptions = []string{"privileged", "cap_add", "devices", "device_cgroup_rules", "volumes_from", "use_api_socket", "userns_mode", "cgroup_parent"}

// isolatePreviewCompose rewrites a resolved Compose configuration for an environment preview. It
// returns the preview-owned volumes the configuration references. The production storage, host
// ports, fixed container names and external networks are never carried into the preview.
func isolatePreviewCompose(config map[string]any, p *EnvironmentPreview, workload, releases string) ([]string, error) {
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return nil, errors.New("Compose has no services")
	}
	mapping := map[string]string{}
	for _, v := range p.Volumes {
		mapping[v.Key] = v.Target
	}
	disabled := map[string]bool{}
	for _, name := range p.Disabled {
		if _, ok := services[name]; !ok {
			return nil, fmt.Errorf("disabled preview service %s is not in Compose", name)
		}
		disabled[name] = true
	}
	networks, _ := config["networks"].(map[string]any)
	removedNetworks := map[string]bool{}
	for name, raw := range networks {
		definition, _ := raw.(map[string]any)
		external, _ := definition["external"].(bool)
		custom, _ := definition["name"].(string)
		if external || (custom != "" && !strings.HasPrefix(custom, "cove-"+workload+"_")) {
			removedNetworks[name] = true
			delete(networks, name)
			continue
		}
		// A subnet pinned in production would collide with the preview's copy on the same node.
		delete(definition, "ipam")
	}
	fresh := map[string]any{}
	used := map[string]bool{}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	releasePrefix := filepath.Clean(releases) + string(filepath.Separator)
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			return nil, errors.New("invalid Compose service")
		}
		for _, field := range unsafePreviewOptions {
			if value := service[field]; value != nil && value != false && !isEmptyCollection(value) {
				return nil, fmt.Errorf("preview service %s cannot use %s", name, field)
			}
		}
		for _, field := range []string{"pid", "ipc", "uts", "cgroup"} {
			if value, _ := service[field].(string); value == "host" || strings.HasPrefix(value, "container:") || strings.HasPrefix(value, "service:") {
				return nil, fmt.Errorf("preview service %s cannot share the host or another container's %s namespace", name, field)
			}
		}
		if mode, _ := service["network_mode"].(string); mode != "" && mode != "none" && mode != "bridge" {
			return nil, fmt.Errorf("preview service %s cannot use network_mode %s", name, mode)
		}
		for _, field := range []string{"ports", "container_name", "external_links", "mac_address"} {
			delete(service, field)
		}
		if attached, ok := service["networks"].(map[string]any); ok {
			for network := range attached {
				if removedNetworks[network] {
					delete(attached, network)
				}
			}
			if len(attached) == 0 && service["network_mode"] == nil {
				attached["default"] = nil
				if networks == nil {
					networks = map[string]any{}
					config["networks"] = networks
				}
				if _, ok := networks["default"]; !ok {
					networks["default"] = map[string]any{}
				}
			}
		}
		labels, _ := service["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels["dev.cove.environment-preview"] = workload
		service["labels"] = labels
		if disabled[name] {
			service["scale"] = 0
			if deploy, ok := service["deploy"].(map[string]any); ok {
				delete(deploy, "replicas")
			}
		}
		if dependencies, ok := service["depends_on"].(map[string]any); ok {
			for dependency := range dependencies {
				if disabled[dependency] {
					delete(dependencies, dependency)
				}
			}
		}
		mounts, _ := service["volumes"].([]any)
		for _, entry := range mounts {
			mount, ok := entry.(map[string]any)
			if !ok {
				return nil, errors.New("invalid Compose mount")
			}
			kind, _ := mount["type"].(string)
			source, _ := mount["source"].(string)
			var key string
			switch kind {
			case "tmpfs":
				continue
			case "volume":
				if source == "" {
					continue
				}
				key = "volume:" + source
			case "bind":
				clean := filepath.Clean(source)
				if strings.HasPrefix(clean+string(filepath.Separator), releasePrefix) {
					if readOnly, _ := mount["read_only"].(bool); !readOnly {
						return nil, errors.New("writable repository mounts are not allowed")
					}
					continue
				}
				key = "bind:" + filepath.ToSlash(clean)
				if mapping[key] == "" {
					return nil, fmt.Errorf("host mount %s is not part of this preview's plan", source)
				}
			default:
				return nil, fmt.Errorf("preview service %s uses unsupported %s storage", name, kind)
			}
			target := mapping[key]
			if target == "" {
				target = PreviewVolumeTarget(workload, key)
			}
			alias := "cove-preview-" + strings.TrimPrefix(target, "cove-pv-"+workload+"-")
			fresh[alias] = map[string]any{"external": true, "name": target}
			used[target] = true
			mount["type"] = "volume"
			mount["source"] = alias
			delete(mount, "bind")
		}
	}
	for _, field := range []string{"secrets", "configs"} {
		items, _ := config[field].(map[string]any)
		for itemName, raw := range items {
			definition, _ := raw.(map[string]any)
			if external, _ := definition["external"].(bool); external {
				return nil, fmt.Errorf("preview cannot use external %s %s", strings.TrimSuffix(field, "s"), itemName)
			}
			if file, _ := definition["file"].(string); file != "" && !strings.HasPrefix(filepath.Clean(file)+string(filepath.Separator), releasePrefix) {
				return nil, fmt.Errorf("preview %s %s must come from the repository", strings.TrimSuffix(field, "s"), itemName)
			}
		}
	}
	config["volumes"] = fresh
	result := make([]string, 0, len(used))
	for target := range used {
		result = append(result, target)
	}
	sort.Strings(result)
	return result, nil
}

func isEmptyCollection(value any) bool {
	switch v := value.(type) {
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	case string:
		return v == ""
	}
	return false
}

func (e *Engine) previewDir(workload string) string {
	return filepath.Join(e.Root, "environment-previews", workload)
}

type previewOwnership struct {
	Preview string `json:"preview"`
	Source  string `json:"source"`
}

// claimEnvironmentPreview records that a resource belongs to an environment preview. A resource that
// was deployed before without the marker is never converted, so production resources cannot be
// passed through preview cleanup or data replacement.
func (e *Engine) claimEnvironmentPreview(r Request) error {
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	marker := filepath.Join(dir, "environment-preview.json")
	raw, err := os.ReadFile(marker)
	if err == nil {
		var owner previewOwnership
		if json.Unmarshal(raw, &owner) != nil || owner.Preview != r.EnvironmentPreview.Preview {
			return errors.New("resource belongs to another environment preview")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if _, err = os.Stat(filepath.Join(dir, "current.json")); err == nil {
		return errors.New("refusing to convert an existing deployment into an environment preview")
	}
	return writeJSON(marker, previewOwnership{Preview: r.EnvironmentPreview.Preview, Source: r.EnvironmentPreview.Source})
}

func (e *Engine) ownedPreviewVolume(ctx context.Context, workload, name string, out io.Writer) error {
	owner, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.environment-preview"}}`, name)
	if err == nil {
		if owner != workload {
			return fmt.Errorf("volume %s is not owned by this preview", name)
		}
		return nil
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "volume", "create", "--label", "dev.cove.environment-preview=" + workload, name}, nil, out); err != nil {
		return err
	}
	owner, err = e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.environment-preview"}}`, name)
	if err != nil || owner != workload {
		return errors.New("could not verify preview volume ownership")
	}
	return nil
}

type previewDataState struct {
	Copied map[string]PreviewVolumeReport `json:"copied"`
	Dump   *PreviewDatabaseReport         `json:"dump,omitempty"`
	Seeded *PreviewDatabaseReport         `json:"seeded,omitempty"`
}

func (e *Engine) loadPreviewData(workload string) (previewDataState, error) {
	state := previewDataState{Copied: map[string]PreviewVolumeReport{}}
	raw, err := os.ReadFile(filepath.Join(e.previewDir(workload), "data.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.Copied == nil {
		state.Copied = map[string]PreviewVolumeReport{}
	}
	return state, nil
}

func (e *Engine) savePreviewData(workload string, state previewDataState) error {
	if err := os.MkdirAll(e.previewDir(workload), 0700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(e.previewDir(workload), "data.json"), state)
}

// preparePreviewStorage isolates the configuration, creates or verifies every preview-owned volume
// and refuses to start until each planned copy completed.
func (e *Engine) preparePreviewStorage(ctx context.Context, r Request, dir string, args, env []string, out io.Writer) (string, error) {
	if err := e.claimEnvironmentPreview(r); err != nil {
		return "", err
	}
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return "", errors.New("could not resolve Compose for preview isolation")
	}
	var config map[string]any
	if err := unmarshalCompose(resolved.data.Bytes(), &config); err != nil {
		return "", errors.New("invalid resolved Compose configuration")
	}
	volumes, err := isolatePreviewCompose(config, r.EnvironmentPreview, r.Workload, filepath.Join(dir, "releases"))
	if err != nil {
		return "", err
	}
	state, err := e.loadPreviewData(r.Workload)
	if err != nil {
		return "", err
	}
	for _, v := range r.EnvironmentPreview.Volumes {
		if v.Mode == "copy" {
			if _, ok := state.Copied[v.Target]; !ok {
				return "", errors.New("preview data has not been copied yet; run the preview data step first")
			}
		}
	}
	if db := r.EnvironmentPreview.Database; db != nil && state.Seeded == nil && state.Dump == nil {
		return "", errors.New("the production database snapshot for this preview is missing; refresh the preview data")
	}
	for _, volume := range volumes {
		if err = e.ownedPreviewVolume(ctx, r.Workload, volume, out); err != nil {
			return "", err
		}
	}
	path := filepath.Join(dir, r.ID+".environment-preview.compose.json")
	return path, writeJSON(path, literalCompose(config))
}

// seedPreviewDatabase restores the production snapshot into the preview's own database service
// before any application service starts. It runs once; later deployments never replace the data.
func (e *Engine) seedPreviewDatabase(ctx context.Context, r Request, dir string, args, env []string, out io.Writer) error {
	db := r.EnvironmentPreview.Database
	if db == nil {
		return nil
	}
	state, err := e.loadPreviewData(r.Workload)
	if err != nil {
		return err
	}
	if state.Seeded != nil {
		return nil
	}
	dump := filepath.Join(e.previewDir(r.Workload), "database.dump")
	info, err := os.Lstat(dump)
	if err != nil || !info.Mode().IsRegular() || state.Dump == nil {
		return errors.New("the production database snapshot for this preview is missing; refresh the preview data")
	}
	fmt.Fprintf(out, "Restoring the production database snapshot taken %s into the preview database.\n", state.Dump.SnapshotAt.UTC().Format(time.RFC3339))
	up := append(append([]string{}, args...), "up", "--detach", "--no-build", "--no-deps", "--wait", "--wait-timeout", "300", db.Service)
	if err = e.Run(ctx, dir, up, env, out); err != nil {
		return errors.New("the preview database service did not start")
	}
	ids, err := e.output(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project="+e.composeProject(r.Workload), "--filter", "label=com.docker.compose.service="+db.Service)
	if err != nil || len(strings.Fields(ids)) != 1 {
		return errors.New("the preview database must run exactly one container")
	}
	container := strings.Fields(ids)[0]
	if err = e.waitForDatabase(ctx, container, db); err != nil {
		return err
	}
	remote := "/tmp/cove-preview-" + r.ID + ".dump"
	if err = e.Run(ctx, dir, []string{"docker", "cp", dump, container + ":" + remote}, nil, io.Discard); err != nil {
		return errors.New("could not stage the database snapshot")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = e.Run(cleanup, dir, []string{"docker", "exec", container, "rm", "-f", remote}, nil, io.Discard)
	}()
	expected := map[string]int64{}
	if db.Engine == "postgresql" {
		counter := newCopyCounter()
		if err = e.Run(ctx, dir, []string{"docker", "exec", container, "pg_restore", "--data-only", "--file=-", remote}, nil, counter); err != nil {
			return errors.New("could not read the database snapshot")
		}
		expected = counter.counts()
		command := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_restore --username="${POSTGRES_USER:-postgres}" --dbname="$1" --clean --if-exists --no-owner --no-acl --single-transaction --exit-on-error "$2"`
		if err = e.Run(ctx, dir, []string{"docker", "exec", container, "sh", "-ec", command, "cove", db.Name, remote}, nil, out); err != nil {
			return errors.New("restoring the database snapshot failed; no application service was started")
		}
	} else {
		file, err := os.Open(dump)
		if err != nil {
			return err
		}
		expected, err = countMariaDBRows(file, db.Name)
		file.Close()
		if err != nil {
			return err
		}
		command := `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; exec mariadb --user=root < "$1"`
		if err = e.Run(ctx, dir, []string{"docker", "exec", container, "sh", "-ec", command, "cove", remote}, nil, out); err != nil {
			return errors.New("restoring the database snapshot failed; no application service was started")
		}
	}
	actual, err := e.countDatabaseRows(ctx, container, db, expected)
	if err != nil {
		return err
	}
	report := &PreviewDatabaseReport{Engine: db.Engine, SnapshotAt: state.Dump.SnapshotAt, DumpBytes: state.Dump.DumpBytes, Counts: actual, Tables: len(actual), Verified: true}
	for table, count := range expected {
		report.Rows += count
		if actual[table] != count {
			report.Verified = false
			fmt.Fprintf(out, "Row count mismatch in %s: snapshot has %d rows, preview has %d.\n", table, count, actual[table])
		}
	}
	if !report.Verified {
		return errors.New("the restored database does not match its snapshot")
	}
	fmt.Fprintf(out, "Restored %d tables and %d rows; every table matches the snapshot.\n", report.Tables, report.Rows)
	state.Seeded = report
	state.Dump = nil
	if err = e.savePreviewData(r.Workload, state); err != nil {
		return err
	}
	if err = os.Remove(dump); err != nil && !os.IsNotExist(err) {
		return err
	}
	return e.recordPreviewReport(r.ID, PreviewReport{Seeded: report})
}

func (e *Engine) recordPreviewReport(id string, report PreviewReport) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	if j.Operation.Preview == nil {
		j.Operation.Preview = &PreviewReport{}
	}
	if report.Volumes != nil {
		j.Operation.Preview.Volumes = report.Volumes
	}
	if report.Database != nil {
		j.Operation.Preview.Database = report.Database
	}
	if report.Seeded != nil {
		j.Operation.Preview.Seeded = report.Seeded
	}
	return writeJSON(e.path(id), j)
}

func (e *Engine) waitForDatabase(ctx context.Context, container string, db *PreviewDatabase) error {
	command := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; psql --host=127.0.0.1 --username="${POSTGRES_USER:-postgres}" --dbname="$1" --command='SELECT 1'`
	if db.Engine == "mariadb" {
		command = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; mariadb --host=127.0.0.1 --user=root --execute='SELECT 1'`
	}
	for attempt := 0; attempt < 120; attempt++ {
		if e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", command, "cove", db.Name}, nil, io.Discard) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("the preview database did not accept connections")
}

var countIdentifier = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,128}$`)

func (e *Engine) countDatabaseRows(ctx context.Context, container string, db *PreviewDatabase, expected map[string]int64) (map[string]int64, error) {
	tables := make([]string, 0, len(expected))
	for table := range expected {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	result := map[string]int64{}
	for start := 0; start < len(tables); start += 200 {
		end := min(start+200, len(tables))
		parts := []string{}
		for _, table := range tables[start:end] {
			schema, name, ok := strings.Cut(table, ".")
			if !ok || !countIdentifier.MatchString(schema) || !countIdentifier.MatchString(name) {
				return nil, fmt.Errorf("unsupported table name %q in the snapshot", table)
			}
			if db.Engine == "postgresql" {
				parts = append(parts, fmt.Sprintf(`SELECT '%s', count(*) FROM "%s"."%s"`, table, schema, name))
			} else {
				parts = append(parts, fmt.Sprintf("SELECT '%s', COUNT(*) FROM `%s`.`%s`", table, schema, name))
			}
		}
		query := strings.Join(parts, " UNION ALL ")
		command := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec psql --host=127.0.0.1 --username="${POSTGRES_USER:-postgres}" --dbname="$1" -At -F "$(printf '\t')" --command="$2"`
		if db.Engine == "mariadb" {
			command = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; exec mariadb --host=127.0.0.1 --user=root -N -B --execute="$2"`
		}
		output := &limitedBuffer{Limit: 4 << 20}
		if err := e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", command, "cove", db.Name, query}, nil, output); err != nil {
			return nil, errors.New("could not count rows in the preview database")
		}
		for _, line := range strings.Split(strings.TrimSpace(output.data.String()), "\n") {
			if line == "" {
				continue
			}
			table, count, ok := strings.Cut(line, "\t")
			if !ok {
				return nil, errors.New("unexpected row count output")
			}
			var n int64
			if _, err := fmt.Sscan(count, &n); err != nil {
				return nil, errors.New("unexpected row count output")
			}
			result[table] = n
		}
	}
	return result, nil
}
