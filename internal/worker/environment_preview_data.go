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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PreviewInventory describes the storage, images and isolation-relevant options of a resource's
// currently deployed release. It is read without changing the resource.
type PreviewInventory struct {
	Revision         string                    `json:"revision,omitempty"`
	Services         []PreviewInventoryService `json:"services"`
	Volumes          []PreviewInventoryVolume  `json:"volumes"`
	ExternalNetworks []string                  `json:"external_networks,omitempty"`
	Database         *PreviewInventoryDatabase `json:"database,omitempty"`
}

type PreviewInventoryService struct {
	Name      string   `json:"name"`
	Image     string   `json:"image"`
	Tags      []string `json:"tags,omitempty"`
	Immutable bool     `json:"immutable"`
	Unsafe    []string `json:"unsafe,omitempty"`
	Ports     bool     `json:"ports,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

type PreviewInventoryVolume struct {
	Key      string   `json:"key"`
	Type     string   `json:"type"`
	Source   string   `json:"source"`
	Services []string `json:"services"`
	ReadOnly bool     `json:"read_only,omitempty"`
	File     bool     `json:"file,omitempty"`
	Missing  bool     `json:"missing,omitempty"`
}

type PreviewInventoryDatabase struct {
	Engine    string `json:"engine"`
	Service   string `json:"service"`
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
}

// InventoryPreviewSource reads the deployed release of a resource for preview planning.
func (e *Engine) InventoryPreviewSource(ctx context.Context, workload string, database *DatabaseBackup) (*PreviewInventory, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid resource")
	}
	if err := validateDatabaseBackup(database); err != nil {
		return nil, err
	}
	dir := filepath.Join(e.Root, "workloads", workload)
	raw, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if os.IsNotExist(err) {
		return nil, errors.New("the resource has no deployed release on this node")
	}
	if err != nil {
		return nil, err
	}
	var current deployedState
	if err = json.Unmarshal(raw, &current); err != nil {
		return nil, err
	}
	env := []string{}
	for key, value := range current.Environment {
		env = append(env, key+"="+value)
	}
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err = e.Run(ctx, dir, []string{"docker", "compose", "--project-name", "cove-" + workload, "--file", current.ComposePath, "config", "--format", "json"}, env, resolved); err != nil {
		return nil, errors.New("the deployed release configuration could not be read")
	}
	var config struct {
		Services map[string]map[string]any `json:"services"`
		Volumes  map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"volumes"`
		Networks map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"networks"`
	}
	if err = json.Unmarshal(resolved.data.Bytes(), &config); err != nil {
		return nil, errors.New("invalid deployed release configuration")
	}
	inventory := &PreviewInventory{Revision: current.Revision, Services: []PreviewInventoryService{}, Volumes: []PreviewInventoryVolume{}}
	volumes := map[string]*PreviewInventoryVolume{}
	managed := []string{filepath.Join(e.Root, "file-secrets") + string(filepath.Separator), filepath.Join(dir, "releases") + string(filepath.Separator)}
	names := make([]string, 0, len(config.Services))
	for name := range config.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		service := config.Services[name]
		item := PreviewInventoryService{Name: trafficServiceName(current.Traffic, name)}
		item.Image, _ = service["image"].(string)
		item.Immutable = imageDigest.MatchString(item.Image)
		if item.Image != "" {
			tags := &limitedBuffer{Limit: 65536}
			if e.Run(ctx, dir, []string{"docker", "image", "inspect", "--format", "{{json .RepoTags}}", item.Image}, nil, tags) == nil {
				_ = json.Unmarshal(tags.data.Bytes(), &item.Tags)
			}
		}
		for _, field := range unsafePreviewOptions {
			if value := service[field]; value != nil && value != false && !isEmptyCollection(value) {
				item.Unsafe = append(item.Unsafe, field)
			}
		}
		for _, field := range []string{"pid", "ipc", "uts", "cgroup"} {
			if value, _ := service[field].(string); value == "host" || strings.HasPrefix(value, "container:") || strings.HasPrefix(value, "service:") {
				item.Unsafe = append(item.Unsafe, field+" "+value)
			}
		}
		if mode, _ := service["network_mode"].(string); mode != "" && mode != "none" && mode != "bridge" {
			item.Unsafe = append(item.Unsafe, "network_mode "+mode)
		}
		if ports, ok := service["ports"].([]any); ok && len(ports) > 0 {
			item.Ports = true
		}
		if dependencies, ok := service["depends_on"].(map[string]any); ok {
			for dependency := range dependencies {
				item.DependsOn = append(item.DependsOn, trafficServiceName(current.Traffic, dependency))
			}
			sort.Strings(item.DependsOn)
		}
		mounts, _ := service["volumes"].([]any)
		for _, entry := range mounts {
			mount, _ := entry.(map[string]any)
			kind, _ := mount["type"].(string)
			source, _ := mount["source"].(string)
			readOnly, _ := mount["read_only"].(bool)
			var key, actual string
			switch kind {
			case "volume":
				if source == "" {
					continue
				}
				key, actual = "volume:"+source, source
				if definition, ok := config.Volumes[source]; ok && definition.Name != "" {
					actual = definition.Name
				}
			case "bind":
				clean := filepath.Clean(source)
				skip := false
				for _, prefix := range managed {
					if strings.HasPrefix(clean+string(filepath.Separator), prefix) {
						skip = true
					}
				}
				if skip {
					continue
				}
				key, actual = "bind:"+filepath.ToSlash(clean), filepath.ToSlash(clean)
			case "tmpfs":
				continue
			default:
				item.Unsafe = append(item.Unsafe, kind+" storage")
				continue
			}
			v := volumes[key]
			if v == nil {
				v = &PreviewInventoryVolume{Key: key, Type: kind, Source: actual, ReadOnly: readOnly}
				if kind == "bind" {
					info, statErr := os.Stat(actual)
					v.Missing = statErr != nil
					v.File = statErr == nil && !info.IsDir()
				} else if _, inspectErr := e.output(ctx, "docker", "volume", "inspect", "--format", "{{.Name}}", actual); inspectErr != nil {
					v.Missing = true
				}
				volumes[key] = v
			}
			v.ReadOnly = v.ReadOnly && readOnly
			v.Services = append(v.Services, item.Name)
		}
		inventory.Services = append(inventory.Services, item)
	}
	keys := make([]string, 0, len(volumes))
	for key := range volumes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		inventory.Volumes = append(inventory.Volumes, *volumes[key])
	}
	for name, network := range config.Networks {
		if strings.HasPrefix(network.Name, "cove-shared-") || strings.HasPrefix(network.Name, "cove-link-") || network.Name == "cove-ingress" {
			continue
		}
		generationNetwork := current.Generation != "" && strings.HasPrefix(network.Name, "cove-"+workload+"-coveg-")
		if network.External || (network.Name != "" && !strings.HasPrefix(network.Name, "cove-"+workload+"_") && !generationNetwork) {
			inventory.ExternalNetworks = append(inventory.ExternalNetworks, name)
		}
	}
	sort.Strings(inventory.ExternalNetworks)
	if database != nil {
		inventory.Database = e.inspectSourceDatabase(ctx, workload, database)
	}
	return inventory, nil
}

func (e *Engine) inspectSourceDatabase(ctx context.Context, workload string, d *DatabaseBackup) *PreviewInventoryDatabase {
	result := &PreviewInventoryDatabase{Engine: d.Engine, Service: d.Service, Name: d.Name}
	container, err := e.databaseContainer(ctx, Request{Workload: workload, Backup: &BackupConfig{Database: d}})
	if err != nil {
		result.Reason = "the production database service is not running"
		return result
	}
	if err = e.checkLogicalCopy(ctx, container, d); err != nil {
		result.Reason = err.Error()
		return result
	}
	result.Supported = true
	result.Bytes, _ = e.databaseBytes(ctx, container, d)
	return result
}

func (e *Engine) checkLogicalCopy(ctx context.Context, container string, d *DatabaseBackup) error {
	validation := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; psql --username="${POSTGRES_USER:-postgres}" --dbname="$1" -Atc "SELECT count(*) FROM pg_extension WHERE extname='timescaledb'"`
	if d.Engine == "mariadb" {
		validation = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; mariadb --user=root -N -B -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='$1' AND table_type='BASE TABLE' AND engine<>'InnoDB'"`
	}
	output := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", validation, "cove", d.Name}, nil, output); err != nil {
		return errors.New("the production database could not be inspected; check the database name and container credentials")
	}
	if strings.TrimSpace(output.data.String()) != "0" {
		return errors.New("TimescaleDB and non-InnoDB tables cannot be copied consistently with a logical snapshot")
	}
	return nil
}

func (e *Engine) databaseBytes(ctx context.Context, container string, d *DatabaseBackup) (int64, error) {
	query := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; psql --username="${POSTGRES_USER:-postgres}" --dbname="$1" -Atc "SELECT pg_database_size(current_database())"`
	if d.Engine == "mariadb" {
		query = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; mariadb --user=root -N -B -e "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema='$1'"`
	}
	output := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", query, "cove", d.Name}, nil, output); err != nil {
		return 0, err
	}
	var n int64
	_, err := fmt.Sscan(strings.TrimSpace(output.data.String()), &n)
	return n, err
}

type volumeCopyResult struct {
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
	Digest string `json:"digest"`
	Free   int64  `json:"free"`
}

func previewMountArgument(kind, source, destination string, readOnly bool) string {
	argument := fmt.Sprintf("type=%s,src=%s,dst=%s", kind, source, destination)
	if readOnly {
		argument += ",readonly"
	}
	return argument
}

// runVolumeHelper runs the worker binary in a network-less container with the source mounted
// read-only. The helper never receives write access to production storage.
func (e *Engine) runVolumeHelper(ctx context.Context, workload, command string, mounts []string) (volumeCopyResult, error) {
	var result volumeCopyResult
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return result, err
	}
	args := []string{"docker", "run", "--rm", "--network", "none", "--security-opt", "no-new-privileges:true", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--cap-add", "FSETID", "--cap-add", "DAC_READ_SEARCH", "--label", "dev.cove.environment-preview=" + workload}
	for _, mount := range mounts {
		args = append(args, "--mount", mount)
	}
	args = append(args, "--entrypoint", "/cove-worker", image, command)
	output := &limitedBuffer{Limit: 65536}
	if err = e.Run(ctx, e.Root, args, nil, output); err != nil {
		message := strings.TrimSpace(output.data.String())
		if i := strings.LastIndexByte(message, '\n'); i >= 0 {
			message = message[i+1:]
		}
		if len(message) > 300 {
			message = message[:300]
		}
		return result, fmt.Errorf("storage helper failed: %s", message)
	}
	text := strings.TrimSpace(output.data.String())
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	if err = json.Unmarshal([]byte(text), &result); err != nil {
		return result, errors.New("storage helper returned an invalid report")
	}
	return result, nil
}

// performPreviewData copies production storage into preview-owned volumes and takes a logical
// database snapshot. Production volumes are mounted read-only and the database is only read.
func (e *Engine) performPreviewData(ctx context.Context, r Request, out io.Writer) error {
	e.previewDataMu.Lock()
	defer e.previewDataMu.Unlock()
	p := r.EnvironmentPreview
	if err := e.claimEnvironmentPreview(r); err != nil {
		return err
	}
	state, err := e.loadPreviewData(r.Workload)
	if err != nil {
		return err
	}
	if p.Refresh {
		fmt.Fprintln(out, "Refreshing preview data from production. The preview's containers are stopped first; production is only read.")
		containers, err := e.Containers(ctx, r.Workload)
		if err != nil {
			return err
		}
		for _, container := range containers {
			if container.State == "running" || container.State == "restarting" {
				if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", container.ID}, nil, out); err != nil {
					return err
				}
			}
		}
		for _, container := range containers {
			if err = e.Run(ctx, e.Root, []string{"docker", "rm", container.ID}, nil, out); err != nil {
				return err
			}
		}
		for _, v := range p.Volumes {
			if v.Mode == "empty" {
				continue
			}
			if err = e.removeOwnedVolume(ctx, r.Workload, v.Target, out); err != nil {
				return err
			}
			delete(state.Copied, v.Target)
		}
		state.Seeded, state.Dump = nil, nil
		if err = e.savePreviewData(r.Workload, state); err != nil {
			return err
		}
	}
	report := PreviewReport{Volumes: []PreviewVolumeReport{}}
	for _, v := range p.Volumes {
		if v.Mode != "copy" {
			if err = e.ownedPreviewVolume(ctx, r.Workload, v.Target, out); err != nil {
				return err
			}
			report.Volumes = append(report.Volumes, PreviewVolumeReport{Target: v.Target, Mode: v.Mode})
			continue
		}
		if done, ok := state.Copied[v.Target]; ok {
			report.Volumes = append(report.Volumes, done)
			continue
		}
		kind := "volume"
		if strings.HasPrefix(v.Key, "bind:") {
			kind = "bind"
			info, statErr := os.Stat(v.Source)
			if statErr != nil || !info.IsDir() {
				return fmt.Errorf("host directory %s is not available to copy", v.Source)
			}
		} else if _, inspectErr := e.output(ctx, "docker", "volume", "inspect", "--format", "{{.Name}}", v.Source); inspectErr != nil {
			return fmt.Errorf("production volume %s does not exist on this node", v.Source)
		}
		// A partially copied volume from an interrupted attempt is discarded; it never held preview data.
		if err = e.removeOwnedVolume(ctx, r.Workload, v.Target, out); err != nil {
			return err
		}
		if err = e.ownedPreviewVolume(ctx, r.Workload, v.Target, out); err != nil {
			return err
		}
		fmt.Fprintf(out, "Copying %s into %s (read-only source).\n", v.Source, v.Target)
		result, err := e.runVolumeHelper(ctx, r.Workload, "volume-copy", []string{previewMountArgument(kind, v.Source, "/from", true), previewMountArgument("volume", v.Target, "/to", false)})
		if err != nil {
			return err
		}
		done := PreviewVolumeReport{Target: v.Target, Mode: "copy", Files: result.Files, Bytes: result.Bytes, Digest: result.Digest}
		fmt.Fprintf(out, "Copied %d files (%d bytes); source and copy digests match.\n", done.Files, done.Bytes)
		state.Copied[v.Target] = done
		if err = e.savePreviewData(r.Workload, state); err != nil {
			return err
		}
		report.Volumes = append(report.Volumes, done)
	}
	for service, image := range p.Images {
		sum := sha256.Sum256([]byte(service))
		tag := "cove-preview-baseline/" + r.Workload + ":" + hex.EncodeToString(sum[:8])
		if err = e.Run(ctx, e.Root, []string{"docker", "image", "tag", image, tag}, nil, io.Discard); err != nil {
			return fmt.Errorf("the deployed production image for %s is no longer on this node", service)
		}
	}
	if p.Database != nil && state.Seeded == nil {
		snapshot, err := e.snapshotSourceDatabase(ctx, r, out)
		if err != nil {
			return err
		}
		state.Dump = snapshot
		if err = e.savePreviewData(r.Workload, state); err != nil {
			return err
		}
		report.Database = snapshot
	} else if state.Seeded != nil {
		report.Database = state.Seeded
	}
	return e.recordPreviewReport(r.ID, report)
}

func (e *Engine) removeOwnedVolume(ctx context.Context, workload, name string, out io.Writer) error {
	owner, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.environment-preview"}}`, name)
	if err != nil {
		return nil
	}
	if owner != workload {
		return fmt.Errorf("volume %s is not owned by this preview", name)
	}
	return e.Run(ctx, e.Root, []string{"docker", "volume", "rm", name}, nil, out)
}

// snapshotSourceDatabase streams a logical dump from the production database container to the
// worker. pg_dump and mariadb-dump --single-transaction read one consistent snapshot; nothing is
// written to the production container or its storage.
func (e *Engine) snapshotSourceDatabase(ctx context.Context, r Request, out io.Writer) (*PreviewDatabaseReport, error) {
	p := r.EnvironmentPreview
	d := &DatabaseBackup{Engine: p.Database.Engine, Service: p.Database.Service, Name: p.Database.Name}
	container, err := e.databaseContainer(ctx, Request{Workload: p.Source, Backup: &BackupConfig{Database: d}})
	if err != nil {
		return nil, err
	}
	if err = e.checkLogicalCopy(ctx, container, d); err != nil {
		return nil, err
	}
	size, err := e.databaseBytes(ctx, container, d)
	if err != nil {
		return nil, errors.New("could not measure the production database")
	}
	if free, err := freeBytes(e.Root); err == nil && free < size+size/5+512<<20 {
		return nil, fmt.Errorf("not enough free disk for the database snapshot: %d bytes free, about %d needed", free, size+size/5+512<<20)
	}
	for _, v := range p.Volumes {
		if v.Mode != "database" {
			continue
		}
		result, err := e.runVolumeHelper(ctx, r.Workload, "volume-free", []string{previewMountArgument("volume", v.Target, "/to", false)})
		if err != nil {
			return nil, err
		}
		if result.Free < size+size/5+512<<20 {
			return nil, fmt.Errorf("not enough free Docker storage for the database copy: %d bytes free, about %d needed", result.Free, size+size/5+512<<20)
		}
		break
	}
	dir := e.previewDir(r.Workload)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	partial := filepath.Join(dir, "database.dump.partial")
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(partial)
	command := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dump --username="${POSTGRES_USER:-postgres}" --dbname="$1" --format=custom`
	if d.Engine == "mariadb" {
		command = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; exec mariadb-dump --user=root --single-transaction --routines --events --triggers --hex-blob --add-drop-database --databases "$1"`
	}
	started := time.Now().UTC()
	fmt.Fprintf(out, "Taking a logical snapshot of the production %s database at %s.\n", d.Engine, started.Format(time.RFC3339))
	hash := sha256.New()
	stderr := &limitedBuffer{Limit: 65536}
	err = e.stream(ctx, []string{"docker", "exec", container, "sh", "-ec", command, "cove", d.Name}, io.MultiWriter(file, hash), stderr)
	closeErr := file.Close()
	if err != nil {
		return nil, errors.New("the production database snapshot failed; check the database name and container credentials")
	}
	if closeErr != nil {
		return nil, closeErr
	}
	info, err := os.Stat(partial)
	if err != nil || info.Size() == 0 {
		return nil, errors.New("the production database snapshot is empty")
	}
	if err = os.Rename(partial, filepath.Join(dir, "database.dump")); err != nil {
		return nil, err
	}
	if err = syncDirectory(dir); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "Snapshot complete: %d bytes, sha256 %s.\n", info.Size(), hex.EncodeToString(hash.Sum(nil)))
	return &PreviewDatabaseReport{Engine: d.Engine, SnapshotAt: started, DumpBytes: info.Size()}, nil
}

// stream runs a command with separate output streams so binary standard output is not mixed with
// diagnostics.
func (e *Engine) stream(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if e.Stream != nil {
		return e.Stream(ctx, args, stdout, stderr)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	configureCommand(cmd)
	cmd.Dir = e.Root
	cmd.Env = os.Environ()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// removeEnvironmentPreview deletes everything a preview resource owns on this node: containers,
// routes, link attachments, preview-owned volumes and networks, retained images and state.
func (e *Engine) removeEnvironmentPreview(ctx context.Context, r Request, out io.Writer) error {
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	raw, err := os.ReadFile(filepath.Join(dir, "environment-preview.json"))
	if os.IsNotExist(err) {
		if _, currentErr := os.Stat(filepath.Join(dir, "current.json")); currentErr == nil {
			return errors.New("refusing to remove a resource that is not an environment preview")
		}
		return e.removePreviewNetworks(ctx, r, out)
	}
	if err != nil {
		return err
	}
	var owner previewOwnership
	if json.Unmarshal(raw, &owner) != nil || owner.Preview != r.EnvironmentPreview.Preview {
		return errors.New("invalid environment preview ownership")
	}
	if err = e.applyIngress(ctx, r.Workload, nil, out); err != nil {
		return err
	}
	if err = e.removeWorkloadLinks(ctx, r.Workload); err != nil {
		return err
	}
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if err = e.Run(ctx, dir, []string{"docker", "rm", "--force", container.ID}, nil, out); err != nil {
			return err
		}
	}
	ids, err := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+r.Workload)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(ids) {
		if err = e.Run(ctx, dir, []string{"docker", "network", "rm", id}, nil, out); err != nil {
			return err
		}
	}
	volumes, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "label=dev.cove.environment-preview="+r.Workload)
	if err != nil {
		return err
	}
	for _, volume := range strings.Fields(volumes) {
		if err = e.removeOwnedVolume(ctx, r.Workload, volume, out); err != nil {
			return err
		}
	}
	for _, reference := range []string{"cove-release/" + r.Workload + ":*", "cove-preview-baseline/" + r.Workload + ":*"} {
		tags, err := e.output(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+reference)
		if err != nil {
			return err
		}
		for _, tag := range strings.Fields(tags) {
			if err = e.Run(ctx, dir, []string{"docker", "image", "rm", tag}, nil, out); err != nil {
				return err
			}
		}
	}
	if err = e.removePreviewNetworks(ctx, r, out); err != nil {
		return err
	}
	if err = os.RemoveAll(e.previewDir(r.Workload)); err != nil {
		return err
	}
	for _, name := range []string{"releases", "release-states", "current.json"} {
		if err = os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// removePreviewNetworks removes the preview's copies of shared networks once no container uses them.
func (e *Engine) removePreviewNetworks(ctx context.Context, r Request, out io.Writer) error {
	for _, id := range r.EnvironmentPreview.Networks {
		name := "cove-shared-" + id
		info := &limitedBuffer{Limit: 65536}
		if e.Run(ctx, e.Root, []string{"docker", "network", "inspect", "--format", `{{index .Labels "cove.shared-network"}} {{len .Containers}}`, name}, nil, info) != nil {
			continue
		}
		fields := strings.Fields(info.data.String())
		if len(fields) != 2 || fields[0] != id {
			return fmt.Errorf("network %s is not the expected preview network", name)
		}
		if fields[1] != "0" {
			continue
		}
		if err := e.Run(ctx, e.Root, []string{"docker", "network", "rm", name}, nil, out); err != nil {
			return err
		}
	}
	return nil
}
