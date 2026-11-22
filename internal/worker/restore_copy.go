package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

type RestoreCopy struct {
	Source      string            `json:"source"`
	Compose     string            `json:"compose"`
	Environment map[string]string `json:"environment"`
	Volumes     []string          `json:"volumes"`
	Mounts      []backupMount     `json:"mounts"`
}

func isolateRestore(config map[string]any, manifest backupManifest, target string) ([]string, []backupMount, error) {
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return nil, nil, errors.New("snapshot has no services")
	}
	volumes, _ := config["volumes"].(map[string]any)
	fresh := map[string]any{}
	mapping := map[string]string{}
	names := []string{}
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			return nil, nil, errors.New("invalid snapshot service")
		}
		for _, field := range []string{"privileged", "devices", "device_cgroup_rules", "cap_add", "pid", "ipc", "volumes_from", "use_api_socket", "secrets", "configs", "post_start", "pre_stop", "provider"} {
			if value := service[field]; value != nil && value != false {
				return nil, nil, fmt.Errorf("restore copy requires removing unsafe or external service option %s", field)
			}
		}
		if mode, _ := service["network_mode"].(string); mode != "" && mode != "none" {
			return nil, nil, errors.New("restore copy does not support host or shared container networking")
		}
		for _, field := range []string{"build", "ports", "container_name", "networks", "network_mode", "links", "external_links", "mac_address", "labels"} {
			delete(service, field)
		}
		service["networks"] = map[string]any{"default": nil}
		mounts, _ := service["volumes"].([]any)
		for _, entry := range mounts {
			mount, ok := entry.(map[string]any)
			if !ok {
				return nil, nil, errors.New("invalid snapshot mount")
			}
			kind, _ := mount["type"].(string)
			source, _ := mount["source"].(string)
			if kind == "tmpfs" {
				continue
			}
			if kind != "bind" && kind != "volume" {
				return nil, nil, errors.New("unsupported snapshot mount")
			}
			if readOnly, _ := mount["read_only"].(bool); readOnly {
				return nil, nil, errors.New("restore copy cannot inherit read-only host or volume mounts; remove them from a dedicated recovery configuration")
			}
			actual := source
			if kind == "volume" {
				definition, _ := volumes[source].(map[string]any)
				if volumeName, ok := definition["name"].(string); ok {
					actual = volumeName
				}
			}
			key := kind + ":" + actual
			replacement := mapping[key]
			if replacement == "" {
				replacement = fmt.Sprintf("cove-%s-restored-%03d", target, len(mapping))
				mapping[key] = replacement
				fresh[replacement] = map[string]any{"external": true, "name": replacement}
			}
			mount["type"] = "volume"
			mount["source"] = replacement
			delete(mount, "bind")
			delete(mount, "volume")
		}
	}
	restored := []backupMount{}
	if manifest.Record.Database == nil {
		for _, mount := range manifest.Mounts {
			source := mount.Source
			if mount.Type == "volume" {
				source = mount.Name
			}
			name := mapping[mount.Type+":"+source]
			if name == "" {
				return nil, nil, errors.New("backup storage does not match its Compose snapshot")
			}
			restored = append(restored, backupMount{Type: "volume", Name: name, RW: true})
		}
		if len(restored) != len(mapping) {
			return nil, nil, errors.New("snapshot contains storage absent from the backup")
		}
	}
	delete(config, "name")
	delete(config, "secrets")
	delete(config, "configs")
	config["volumes"] = fresh
	config["networks"] = map[string]any{"default": map[string]any{"internal": true}}
	result := []string{}
	for name := range fresh {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, restored, nil
}

func (e *Engine) PlanRestoreCopy(ctx context.Context, r Request, source string) (*RestoreCopy, error) {
	if !workloadIdentifier.MatchString(r.Workload) || !workloadIdentifier.MatchString(source) || source == r.Workload || r.Backup == nil || !identifier.MatchString(r.BackupID) {
		return nil, errors.New("invalid restore copy source")
	}
	sourceRequest := r
	sourceRequest.Workload = source
	manifest, err := e.loadBackupManifest(ctx, sourceRequest, io.Discard)
	if err != nil {
		return nil, err
	}
	if manifest.Record.Workload != source || manifest.Record.Repository != r.Backup.Repository || manifest.Record.ID != r.BackupID || !snapshotIdentifier.MatchString(manifest.Record.Snapshot) || !reflect.DeepEqual(manifest.Record.Database, r.Backup.Database) {
		return nil, errors.New("backup source does not match")
	}
	prefix := filepath.Join(e.Root, "workloads", source, "release-states") + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(manifest.State.ComposePath), prefix) || !strings.HasSuffix(manifest.State.ComposePath, ".compose.json") {
		return nil, errors.New("invalid backup release path")
	}
	raw, err := os.ReadFile(manifest.State.ComposePath)
	if err != nil {
		return nil, errors.New("backup release configuration is no longer available on this node")
	}
	var config map[string]any
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	volumes, mounts, err := isolateRestore(config, manifest, r.Workload)
	if err != nil {
		return nil, err
	}
	// Snapshot values were already escaped for Compose. Restore references before returning configuration to the panel.
	keys := []string{}
	for key, value := range manifest.State.Environment {
		if value != "" {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(manifest.State.Environment[keys[i]]) > len(manifest.State.Environment[keys[j]])
	})
	pairs := []string{}
	for _, key := range keys {
		pairs = append(pairs, strings.ReplaceAll(manifest.State.Environment[key], "$", "$$"), "${"+key+"}")
	}
	replacer := strings.NewReplacer(pairs...)
	var hide func(any) any
	hide = func(value any) any {
		switch value := value.(type) {
		case string:
			return replacer.Replace(value)
		case []any:
			for i, v := range value {
				value[i] = hide(v)
			}
		case map[string]any:
			for k, v := range value {
				value[k] = hide(v)
			}
		}
		return value
	}
	raw, err = json.Marshal(hide(config))
	if err != nil {
		return nil, err
	}
	return &RestoreCopy{Source: source, Compose: string(raw), Environment: manifest.State.Environment, Volumes: volumes, Mounts: mounts}, nil
}

func (e *Engine) performRestoreCopy(ctx context.Context, r Request, out io.Writer) (result error) {
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	if err := e.recoverDrills(ctx); err != nil {
		return err
	}
	plan, err := e.PlanRestoreCopy(ctx, r, r.RestoreCopy.Source)
	if err != nil {
		return err
	}
	if plan.Compose != r.Compose {
		return errors.New("backup recovery plan changed; create a new restore copy")
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	if _, err = os.Stat(filepath.Join(dir, "current.json")); err == nil {
		return errors.New("restore copy requires a resource that has never been deployed")
	} else if !os.IsNotExist(err) {
		return err
	}
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil {
		return err
	}
	if len(containers) > 0 {
		return errors.New("restore copy target already has containers")
	}
	marker := filepath.Join(dir, "copy-recovery.json")
	if err = writeJSON(marker, map[string]string{"operation": r.ID}); err != nil {
		return err
	}
	root := filepath.Join(e.Root, "drills")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(root, r.ID+".json"), map[string]any{"workload": r.Workload, "database_verify": r.Backup.Database != nil && extendedDatabase(r.Backup.Database.Engine)}); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		result = errors.Join(result, e.cleanupDrill(cleanup, r.ID))
		if result == nil {
			result = os.Remove(marker)
		} else {
			fmt.Fprintln(out, "Restore copy is incomplete. Source storage was not changed. The new resource remains blocked from starting; its new volumes are retained for inspection.")
		}
	}()
	source := r
	source.Workload = plan.Source
	manifest, err := e.loadBackupManifest(ctx, source, out)
	if err != nil {
		return err
	}
	if r.Backup.Remote != nil {
		if err = e.remoteBackupHelper(ctx, out); err != nil {
			return err
		}
	}
	if err = e.restic(ctx, source, nil, false, out, "check", "--read-data"); err != nil {
		return err
	}
	for _, volume := range plan.Volumes {
		owner, inspectErr := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.restore"}}`, volume)
		if inspectErr == nil {
			if owner != r.Workload {
				return errors.New("refusing existing unowned recovery volume")
			}
		} else if err = e.Run(ctx, e.Root, []string{"docker", "volume", "create", "--label", "dev.cove.restore=" + r.Workload, volume}, nil, out); err != nil {
			return err
		}
		owner, err = e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.restore"}}`, volume)
		if err != nil || owner != r.Workload {
			return errors.New("could not verify recovery volume ownership")
		}
	}
	mounts := plan.Mounts
	if manifest.Record.Database != nil {
		stage := filepath.Join(root, r.ID, "000")
		if err = os.MkdirAll(stage, 0700); err != nil {
			return err
		}
		mounts = []backupMount{{Type: "bind", Source: stage, RW: true}}
	}
	if manifest.Record.DataSnapshot != "" {
		err = e.readDatabaseSnapshot(ctx, source, manifest, filepath.Join(root, r.ID, "000", "database.dump"))
	} else {
		err = e.restic(ctx, source, mounts, true, out, "restore", manifest.Record.Snapshot, "--target", "/", "--include", "/data", "--verify")
	}
	if err != nil {
		return err
	}
	if manifest.Record.Database != nil {
		if err = e.restoreCopyDatabase(ctx, r, manifest.Record.Database, filepath.Join(root, r.ID, "000", "database.dump"), out); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "Backup restored into separate storage. The source was not stopped or changed. The new resource has no public routes and remains offline; review its configuration before deploying.")
	return nil
}

func (e *Engine) restoreCopyDatabase(ctx context.Context, r Request, d *DatabaseBackup, dump string, out io.Writer) error {
	info, err := os.Lstat(dump)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("database dump is missing")
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	path := filepath.Join(dir, "restore-copy.compose.json")
	if err = os.WriteFile(path, []byte(r.Compose), 0600); err != nil {
		return err
	}
	env := []string{}
	for key, value := range r.Environment {
		env = append(env, key+"="+value)
	}
	name := "cove-copy-db-" + r.ID
	args := []string{"docker", "compose", "--project-name", "cove-" + r.Workload, "--file", path, "run", "--detach", "--no-deps", "--no-TTY", "--use-aliases", "--name", name, "--label", "dev.cove.drill=" + r.ID, d.Service}
	if err = e.Run(ctx, dir, args, env, out); err != nil {
		return err
	}
	if extendedDatabase(d.Engine) {
		ready := false
		for attempt := 0; attempt < 90; attempt++ {
			command := redisAuth + redisCLI(d.Engine) + " PING"
			if d.Engine == "mongodb" {
				command = `mongosh --nodb --quiet --eval '` + mongoCredentials + `const c=new Mongo("mongodb://127.0.0.1:27017"); if(user)c.getDB("admin").auth(user,password); const a=c.getDB("admin"); const h=a.runCommand({hello:1}),opts=a.runCommand({getCmdLineOpts:1}).parsed,set=opts.replication&&(opts.replication.replSetName||opts.replication.replSet);if(set&&!h.isWritablePrimary){if(!h.setName)a.runCommand({replSetInitiate:{_id:set,members:[{_id:0,host:"` + d.Service + `:27017"}]}});quit(1)} if(!a.runCommand({ping:1}).ok)quit(1);'`
			}
			if e.Run(ctx, dir, []string{"docker", "exec", name, "sh", "-ec", command}, nil, io.Discard) == nil {
				ready = true
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if !ready {
			return errors.New("restored database did not initialize")
		}
		if err = e.restoreExtendedDatabase(ctx, r, name, dump, out); err != nil {
			return err
		}
		return e.Run(ctx, dir, []string{"docker", "stop", "--time", "45", name}, nil, out)
	}
	ready := false
	for attempt := 0; attempt < 90; attempt++ {
		command := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; psql --host=127.0.0.1 --username="${POSTGRES_USER:-postgres}" --dbname="$1" --command='SELECT 1'`
		if d.Engine == "mariadb" {
			command = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; mariadb --host=127.0.0.1 --user=root --execute='SELECT 1'`
		}
		if e.Run(ctx, dir, []string{"docker", "exec", name, "sh", "-ec", command, "cove", d.Name}, nil, io.Discard) == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !ready {
		return errors.New("restored database did not initialize")
	}
	if err = e.Run(ctx, dir, []string{"docker", "cp", dump, name + ":/tmp/cove-restore.dump"}, nil, io.Discard); err != nil {
		return err
	}
	command := `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_restore --username="${POSTGRES_USER:-postgres}" --dbname="$1" --clean --if-exists --no-owner --no-acl --single-transaction --exit-on-error /tmp/cove-restore.dump`
	if d.Engine == "mariadb" {
		command = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; exec mariadb --user=root < /tmp/cove-restore.dump`
	}
	if err = e.Run(ctx, dir, []string{"docker", "exec", name, "sh", "-ec", command, "cove", d.Name}, nil, io.Discard); err != nil {
		return errors.New("database import into the new resource failed")
	}
	return e.Run(ctx, dir, []string{"docker", "stop", "--time", "45", name}, nil, out)
}
