package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

func (e *Engine) cleanupDrill(ctx context.Context, id string) error {
	if !identifier.MatchString(id) {
		return errors.New("invalid drill recovery ID")
	}
	for _, label := range []string{"dev.cove.drill=", "dev.cove.operation="} {
		raw, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label="+label+id)
		if err != nil {
			return err
		}
		if ids := strings.Fields(raw); len(ids) > 0 {
			if err = e.Run(ctx, e.Root, append([]string{"docker", "rm", "--force", "--volumes"}, ids...), nil, io.Discard); err != nil {
				return err
			}
		}
	}
	stage := filepath.Join(e.Root, "drills", id)
	if err := os.RemoveAll(stage); err != nil {
		if !os.IsPermission(err) {
			return err
		}
		info, inspectErr := os.Lstat(stage)
		if inspectErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("invalid temporary drill directory")
		}
		// Restic preserves original ownership. A non-root worker cannot unlink every restored directory.
		image := "restic/restic:0.19.1"
		if _, err := e.output(ctx, "docker", "image", "inspect", remoteBackupImage); err == nil {
			image = remoteBackupImage
		}
		args := []string{"docker", "run", "--rm", "--pull", "never", "--name", "cove-drill-cleanup-" + id, "--label", "dev.cove.drill=" + id, "--network", "none", "--read-only", "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--mount", "type=bind,source=" + stage + ",target=/drill", "--entrypoint", "sh", image, "-ec", "find /drill -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +"}
		if cleanupErr := e.Run(ctx, e.Root, args, nil, io.Discard); cleanupErr != nil {
			return cleanupErr
		}
		if err = os.RemoveAll(stage); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(e.Root, "drills", id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (e *Engine) recoverDrills(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(e.Root, "drills"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			if err = e.cleanupDrill(ctx, strings.TrimSuffix(entry.Name(), ".json")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) performDrill(ctx context.Context, r Request, out io.Writer) (result error) {
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	if err := e.recoverDrills(ctx); err != nil {
		return errors.New("previous restore drill cleanup requires repair")
	}
	manifest, err := e.loadBackupManifest(ctx, r, out)
	if err != nil {
		return err
	}
	if manifest.Record.Workload != r.Workload || manifest.Record.Repository != r.Backup.Repository || manifest.Record.ID != r.BackupID || !snapshotIdentifier.MatchString(manifest.Record.Snapshot) || !reflect.DeepEqual(manifest.Record.Database, r.Backup.Database) {
		return errors.New("backup does not match this resource and policy")
	}
	if len(manifest.Mounts) == 0 {
		return errors.New("backup has no restorable storage")
	}
	root := filepath.Join(e.Root, "drills")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(root, r.ID+".json"), map[string]string{"workload": r.Workload}); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		cleanupErr := e.cleanupDrill(cleanup, r.ID)
		if cleanupErr != nil {
			fmt.Fprintln(out, "Temporary restore drill cleanup is incomplete. Its recovery journal is retained and verification is not marked successful.")
		}
		result = errors.Join(result, cleanupErr)
		if result == nil {
			now := time.Now().UTC()
			manifest.Record.VerifiedAt = &now
			manifest.Record.Verification = "files"
			if manifest.Record.Database != nil {
				manifest.Record.Verification = "database"
			}
			result = writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "backups", r.BackupID+".json"), manifest)
		}
	}()
	stage := filepath.Join(root, r.ID)
	mounts := make([]backupMount, 0, len(manifest.Mounts))
	for i := range manifest.Mounts {
		path := filepath.Join(stage, fmt.Sprintf("%03d", i))
		if err = os.MkdirAll(path, 0700); err != nil {
			return err
		}
		mounts = append(mounts, backupMount{Type: "bind", Source: path, RW: true})
	}
	if r.Backup.Remote != nil {
		if err = e.remoteBackupHelper(ctx, out); err != nil {
			return err
		}
	}
	if err = e.restic(ctx, r, nil, false, out, "check", "--read-data"); err != nil {
		return errors.New("backup repository integrity check failed")
	}
	if err = e.restic(ctx, r, mounts, true, out, "restore", manifest.Record.Snapshot, "--target", "/", "--include", "/data", "--verify"); err != nil {
		return errors.New("backup could not be restored into temporary storage")
	}
	fmt.Fprintln(out, "Backup restored and verified in isolated temporary storage; production mounts were not used.")
	if manifest.Record.Database == nil {
		fmt.Fprintln(out, "File restore verified. Application startup is not tested by this volume drill.")
		return nil
	}
	d := manifest.Record.Database
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+r.Workload, "--filter", "label=com.docker.compose.service="+d.Service)
	if err != nil || len(strings.Fields(ids)) != 1 {
		return errors.New("database drill requires the original database service container to identify its image")
	}
	image, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", strings.Fields(ids)[0])
	if err != nil || !strings.HasPrefix(image, "sha256:") {
		return errors.New("could not identify the database image")
	}
	name := "cove-drill-" + r.ID
	password := rand.Text()
	env := []string{"POSTGRES_PASSWORD=" + password, "POSTGRES_USER=postgres", "POSTGRES_DB=" + d.Name, "PGPASSWORD=" + password, "MARIADB_ROOT_PASSWORD=" + password, "MARIADB_DATABASE=" + d.Name, "MYSQL_PWD=" + password}
	args := []string{"docker", "run", "--detach", "--pull", "never", "--name", name, "--label", "dev.cove.drill=" + r.ID, "--network", "none", "--restart", "no", "--memory", "512m", "--cpus", "1", "--pids-limit", "256"}
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		args = append(args, "-e", key)
	}
	args = append(args, image)
	if err = e.Run(ctx, e.Root, args, env, io.Discard); err != nil {
		return errors.New("could not start the isolated database drill container")
	}
	ready := false
	for attempt := 0; attempt < 60; attempt++ {
		command := []string{"docker", "exec", name, "pg_isready", "--host", "127.0.0.1", "--username", "postgres", "--dbname", d.Name}
		if d.Engine == "mariadb" {
			command = []string{"docker", "exec", name, "mariadb", "--host=127.0.0.1", "--user=root", "--database", d.Name, "--execute", "SELECT 1"}
		}
		if e.Run(ctx, e.Root, command, nil, io.Discard) == nil {
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
		return errors.New("isolated database did not become ready")
	}
	dump := filepath.Join(stage, "000", "database.dump")
	info, err := os.Lstat(dump)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("restored database dump is missing or is not a regular file")
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", dump, name + ":/tmp/cove-drill.dump"}, nil, io.Discard); err != nil {
		return errors.New("could not stage the isolated database dump")
	}
	command := []string{"docker", "exec", name, "pg_restore", "--username", "postgres", "--dbname", d.Name, "--no-owner", "--no-acl", "--single-transaction", "--exit-on-error", "/tmp/cove-drill.dump"}
	if d.Engine == "mariadb" {
		command = []string{"docker", "exec", name, "sh", "-ec", "exec mariadb --user=root < /tmp/cove-drill.dump"}
	}
	if err = e.Run(ctx, e.Root, command, nil, io.Discard); err != nil {
		return errors.New("database restore drill failed; production data was not touched")
	}
	fmt.Fprintln(out, "Database dump restored successfully in an isolated container. Database ownership, grants, and external application behavior are not verified.")
	return nil
}
