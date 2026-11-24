package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"
)

type AutomaticScan struct {
	Origin   string              `json:"origin"`
	Database string              `json:"database"`
	Images   map[string][]string `json:"images"`
}

type ScanObservation struct {
	Eligible bool                `json:"eligible"`
	Images   map[string][]string `json:"images"`
	Message  string              `json:"message,omitempty"`
}

type ScanDatabaseStatus struct {
	Fresh       bool         `json:"fresh"`
	Database    ScanDatabase `json:"database"`
	Refreshing  bool         `json:"refreshing"`
	NextAttempt time.Time    `json:"next_attempt"`
	Message     string       `json:"message,omitempty"`
}

type scanDatabaseRefresh struct {
	ID          string    `json:"id"`
	Requested   bool      `json:"requested"`
	NextAttempt time.Time `json:"next_attempt"`
	Message     string    `json:"message,omitempty"`
}

func scanImageIdentity(image, platform string) string {
	digest := sha256.Sum256([]byte(image + "\n" + platform))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (e *Engine) scanPlatform(ctx context.Context, container string) string {
	var descriptor struct{ Digest string }
	if err := e.imageJSON(ctx, nil, &descriptor, "docker", "inspect", "--format", "{{json .ImageManifestDescriptor}}", container); err != nil || !imageDigest.MatchString(descriptor.Digest) {
		return ""
	}
	return descriptor.Digest
}

func (a *AutomaticScan) validate(r Request) error {
	if a == nil {
		return nil
	}
	if r.Action != "image_scan" || (a.Origin != "post_deploy" && a.Origin != "scheduled") || !imageDigest.MatchString(a.Database) || len(a.Images) == 0 || len(a.Images) > 64 {
		return errors.New("invalid automatic image scan")
	}
	seen := map[string]bool{}
	total := 0
	for service, images := range a.Images {
		if !serviceIdentifier.MatchString(service) || len(images) == 0 || len(images) > 64 {
			return errors.New("invalid automatic image identities")
		}
		for _, image := range images {
			if !imageDigest.MatchString(image) || seen[service+"/"+image] {
				return errors.New("invalid automatic image identities")
			}
			seen[service+"/"+image] = true
			total++
		}
	}
	if total > 64 {
		return errors.New("automatic image scan exceeds its coverage limit")
	}
	return nil
}

func (e *Engine) observeScan(ctx context.Context, r Request) (ScanObservation, error) {
	result := ScanObservation{Images: map[string][]string{}}
	if !scannerEnabled() || !workloadIdentifier.MatchString(r.Workload) || (r.Kind != "service" && r.Kind != "job") {
		return result, errors.New("automatic image scanning is unavailable")
	}
	e.mu.Lock()
	busy := e.busy(r.Workload)
	e.mu.Unlock()
	if busy {
		result.Message = "A foreground operation is active."
		return result, nil
	}
	suspended, err := e.readSuspension(r.Workload)
	if err != nil {
		return result, err
	}
	if suspended != nil && suspended.Phase != "awake" {
		result.Message = "The resource is stopped, paused or sleeping."
		return result, nil
	}
	idle, err := e.IdleStatus(ctx, r.Workload)
	if err != nil {
		return result, err
	}
	if idle != nil && idle.Enabled && idle.Phase != "awake" {
		result.Message = "The resource is sleeping or changing idle state."
		return result, nil
	}
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil || len(containers) == 0 || len(containers) > 64 {
		result.Message = "Retained image coverage is unavailable."
		return result, err
	}
	jobPresent := r.Kind != "job"
	running := false
	seen := map[string]bool{}
	for _, container := range containers {
		var detail struct {
			Image, Service string
			State          struct{ Status string }
		}
		if err = e.imageJSON(ctx, nil, &detail, "docker", "inspect", "--format", `{"image":{{json .Image}},"service":{{json (or (index .Config.Labels "dev.cove.service") (index .Config.Labels "com.docker.compose.service"))}},"state":{{json .State}}}`, container.ID); err != nil {
			return result, err
		}
		if !imageDigest.MatchString(detail.Image) || !serviceIdentifier.MatchString(detail.Service) {
			return result, errors.New("retained image identity is unavailable")
		}
		running = running || detail.State.Status == "running"
		if detail.State.Status != "running" && detail.State.Status != "exited" {
			result.Message = "Retained container state is unavailable."
			return result, nil
		}
		jobPresent = jobPresent || detail.Service == r.JobService
		identity := scanImageIdentity(detail.Image, e.scanPlatform(ctx, container.ID))
		if key := detail.Service + "/" + identity; !seen[key] {
			result.Images[detail.Service] = append(result.Images[detail.Service], identity)
			seen[key] = true
		}
	}
	for service := range result.Images {
		sort.Strings(result.Images[service])
	}
	result.Eligible = jobPresent && (r.Kind == "job" || running)
	return result, nil
}

func (e *Engine) cachedScanDatabase() (scanDatabaseCache, error) {
	var saved scanDatabaseCache
	data, err := readScanState(filepath.Join(e.Root, "scanner", "current.json"))
	if err != nil || len(data) > 16384 || json.Unmarshal(data, &saved) != nil || !imageDigest.MatchString(saved.Digest) || !scanCacheName.MatchString(saved.Path) || saved.Path != "db-"+saved.Digest[len("sha256:"):] {
		return saved, errors.New("vulnerability database metadata is unavailable")
	}
	if !saved.Fresh(time.Now().UTC()) {
		return saved, errors.New("vulnerability database is missing, stale or future-dated")
	}
	for _, name := range []string{"trivy.db", "metadata.json"} {
		info, err := os.Lstat(filepath.Join(e.Root, "scanner", saved.Path, "db", name))
		limit := scanDatabaseLimit
		if name == "metadata.json" {
			limit = 16384
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
			return saved, errors.New("cached vulnerability database files are unavailable")
		}
	}
	return saved, nil
}

func readScanState(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 16385))
}

func (e *Engine) readScanRefresh() (scanDatabaseRefresh, error) {
	var state scanDatabaseRefresh
	data, err := readScanState(filepath.Join(e.Root, "scanner", "automatic-refresh.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil || len(data) > 16384 || json.Unmarshal(data, &state) != nil || state.ID != "" && !identifier.MatchString(state.ID) {
		return state, errors.New("automatic database refresh state is unavailable")
	}
	return state, nil
}

func (e *Engine) scanDatabaseStatus(request bool) (ScanDatabaseStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state, err := e.readScanRefresh()
	if err != nil {
		return ScanDatabaseStatus{}, err
	}
	database, cacheErr := e.cachedScanDatabase()
	if request && scannerEnabled() && cacheErr != nil && !state.Requested && state.ID == "" && !time.Now().Before(state.NextAttempt) {
		state.Requested = true
		if err = os.MkdirAll(filepath.Join(e.Root, "scanner"), 0700); err == nil {
			err = writeJSON(filepath.Join(e.Root, "scanner", "automatic-refresh.json"), state)
		}
		if err != nil {
			return ScanDatabaseStatus{}, err
		}
	}
	result := ScanDatabaseStatus{Fresh: cacheErr == nil, Database: database.ScanDatabase, Refreshing: state.Requested || state.ID != "", NextAttempt: state.NextAttempt, Message: state.Message}
	if cacheErr != nil && result.Message == "" {
		result.Message = "Vulnerability database coverage is unknown until a bounded refresh succeeds."
	}
	return result, nil
}

func (e *Engine) dispatchScanDatabase(ctx context.Context, jobs *sync.WaitGroup) error {
	if !scannerEnabled() || !e.scanMu.TryLock() {
		return nil
	}
	e.mu.Lock()
	state, err := e.readScanRefresh()
	if err != nil || !state.Requested || state.ID != "" || time.Now().Before(state.NextAttempt) {
		e.mu.Unlock()
		e.scanMu.Unlock()
		return err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err == nil {
		state.ID, state.Requested = hex.EncodeToString(id[:]), false
		state.NextAttempt = time.Now().UTC().Add(time.Hour)
		err = writeJSON(filepath.Join(e.Root, "scanner", "automatic-refresh.json"), state)
	}
	e.mu.Unlock()
	if err != nil {
		e.scanMu.Unlock()
		return err
	}
	jobs.Go(func() {
		defer e.scanMu.Unlock()
		refresh, cancel := context.WithTimeout(ctx, 11*time.Minute)
		defer cancel()
		owner, failure := e.buildOwner()
		if failure == nil {
			dir := filepath.Join(e.Root, "scanner", "refresh-input-"+state.ID)
			failure = os.Mkdir(dir, 0700)
			if failure == nil {
				defer os.RemoveAll(dir)
				var image string
				image, failure = e.ensureScannerImage(refresh, dir)
				if failure == nil {
					_, failure = e.scanDatabase(refresh, owner, state.ID, image)
				}
			}
		}
		state.ID, state.Message = "", ""
		if failure != nil {
			state.Message = "The bounded vulnerability database refresh did not succeed; coverage remains unknown. It will retry after the cooldown."
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := writeJSON(filepath.Join(e.Root, "scanner", "automatic-refresh.json"), state); err != nil {
			fmt.Fprintln(os.Stderr, "Automatic vulnerability database refresh status could not be saved; automatic refresh needs recovery.")
		}
	})
	return nil
}

func (e *Engine) recoverScanRefresh(ctx context.Context) error {
	state, err := e.readScanRefresh()
	if err != nil || state.ID == "" {
		return err
	}
	owner, err := e.buildOwner()
	if err == nil {
		err = e.removeScanContainer(ctx, "cove-scan-db-"+owner+"-"+state.ID, owner)
	}
	if err != nil {
		return err
	}
	if err = os.RemoveAll(filepath.Join(e.Root, "scanner", "refresh-input-"+state.ID)); err != nil {
		return err
	}
	state.ID, state.Requested = "", false
	state.Message = "A database refresh was interrupted; coverage remains unknown until a later bounded retry."
	return writeJSON(filepath.Join(e.Root, "scanner", "automatic-refresh.json"), state)
}

func (e *Engine) automaticScanDatabase(ctx context.Context, r Request) (scanDatabaseCache, error) {
	observed, err := e.observeScan(ctx, r)
	if err != nil || !observed.Eligible || !reflect.DeepEqual(observed.Images, r.AutomaticScan.Images) {
		return scanDatabaseCache{}, errors.New("automatic scan inputs or resource state changed")
	}
	saved, err := e.cachedScanDatabase()
	if err != nil || saved.Digest != r.AutomaticScan.Database {
		return saved, errors.New("automatic scan database generation is unavailable or changed")
	}
	verified, err := validateScanDatabase(filepath.Join(e.Root, "scanner", saved.Path))
	if err != nil || verified != saved.ScanDatabase {
		return saved, errors.New("automatic scan database verification failed")
	}
	return saved, nil
}

func (e *Engine) retireImageScan(ctx context.Context, r Request) error {
	if r.AutomaticScan == nil || ValidateRequest(r) != nil {
		return errors.New("invalid automatic scan ownership")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, running := e.automaticScans[r.ID]; running {
		return errors.New("automatic scan is still running")
	}
	j, err := e.read(r.ID)
	if !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		if j.Request.Workload != r.Workload || j.Request.Action != "image_scan" || j.Request.AutomaticScan == nil || j.Operation.State == "running" || j.Operation.State == "queued" {
			return errors.New("automatic scan ownership is unavailable or active")
		}
		if err := e.recoverImageScan(ctx, j.Request); err != nil {
			return err
		}
	}
	for _, filename := range []string{e.path(r.ID), filepath.Join(e.Root, "operations", r.ID+".log")} {
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
