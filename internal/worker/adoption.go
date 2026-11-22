package worker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type adoptedProject struct {
	Project     string `json:"project"`
	Fingerprint string `json:"fingerprint"`
}

var composeProjectIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func (e *Engine) composeProject(workload string) string {
	e.adoptionMu.RLock()
	defer e.adoptionMu.RUnlock()
	if adopted, ok := e.adoptedProjects[workload]; ok {
		return adopted.Project
	}
	return "cove-" + workload
}

func (e *Engine) loadAdoptedProjects() error {
	e.adoptedProjects = map[string]adoptedProject{}
	raw, err := os.ReadFile(filepath.Join(e.Root, "adopted-projects.json"))
	if err != nil && !os.IsNotExist(err) || err == nil && (json.Unmarshal(raw, &e.adoptedProjects) != nil || e.adoptedProjects == nil) {
		return errors.New("adopted project registry is unavailable")
	}
	seen := map[string]bool{}
	for workload, entry := range e.adoptedProjects {
		if !workloadIdentifier.MatchString(workload) || !validAdoptionProject(entry.Project) || len(entry.Fingerprint) != 64 || seen[entry.Project] {
			return errors.New("invalid adopted project registry")
		}
		seen[entry.Project] = true
		if _, err = os.Stat(filepath.Join(e.Root, "workloads", workload, "current.json")); err != nil {
			return errors.New("adopted project state is unavailable")
		}
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "workloads"))
	if err != nil {
		return err
	}
	recovered := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(e.Root, "workloads", entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "adoption-intent.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var intent AdoptionRequest
		if json.Unmarshal(data, &intent) != nil || intent.Workload != entry.Name() || !workloadIdentifier.MatchString(intent.Workload) || !validAdoptionProject(intent.Project) || len(intent.Fingerprint) != 64 {
			return errors.New("invalid adoption recovery intent")
		}
		if registered, ok := e.adoptedProjects[intent.Workload]; ok {
			if registered.Project != intent.Project || registered.Fingerprint != intent.Fingerprint {
				return errors.New("adoption registry disagrees with its recovery intent")
			}
			continue
		}
		if _, err = os.Stat(filepath.Join(dir, "current.json")); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if seen[intent.Project] {
			return errors.New("adoption recovery found conflicting project ownership")
		}
		seen[intent.Project] = true
		e.adoptedProjects[intent.Workload] = adoptedProject{intent.Project, intent.Fingerprint}
		recovered = true
	}
	if recovered {
		return writeJSON(filepath.Join(e.Root, "adopted-projects.json"), e.adoptedProjects)
	}
	return nil
}

func validAdoptionProject(project string) bool {
	return composeProjectIdentifier.MatchString(project) && !strings.HasPrefix(project, "cove") && !strings.HasPrefix(project, "coolify") && project != "traefik" && project != "portainer" && project != "dokploy" && project != "captain"
}

type ComposeProject struct {
	Project    string   `json:"project"`
	Services   []string `json:"services"`
	Containers int      `json:"containers"`
	Blockers   []string `json:"blockers"`
}

type AdoptionRequest struct {
	Project     string `json:"project"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Workload    string `json:"workload,omitempty"`
	Release     string `json:"release,omitempty"`
}

type AdoptionPlan struct {
	Project       string          `json:"project"`
	Fingerprint   string          `json:"fingerprint"`
	Configuration ImportedCompose `json:"configuration"`
}

func (e *Engine) adoptionFingerprint(data any) (string, error) {
	path := filepath.Join(e.Root, "adoption-key")
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			err = os.WriteFile(path, key, 0600)
		}
	}
	if err != nil || len(key) != 32 {
		return "", errors.New("adoption review key is unavailable")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	digest := hmac.New(sha256.New, key)
	digest.Write(raw)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (e *Engine) AdoptCompose(ctx context.Context, request AdoptionRequest) error {
	if !workloadIdentifier.MatchString(request.Workload) || !identifier.MatchString(request.Release) || !validAdoptionProject(request.Project) || len(request.Fingerprint) != 64 {
		return errors.New("invalid adoption request")
	}
	e.mu.Lock()
	if e.draining || e.active[request.Workload] {
		e.mu.Unlock()
		return errors.New("worker or resource is busy")
	}
	e.active[request.Workload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, request.Workload); e.mu.Unlock() }()
	e.adoptionMu.Lock()
	err := e.adoptComposeLocked(ctx, request)
	e.adoptionMu.Unlock()
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(e.Root, "workloads", request.Workload, "drift.json")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return e.recordDriftBaseline(ctx, Request{ID: request.Release, Workload: request.Workload, Action: "adopt"})
}

func (e *Engine) adoptComposeLocked(ctx context.Context, request AdoptionRequest) error {
	if existing, ok := e.adoptedProjects[request.Workload]; ok {
		if existing.Project == request.Project && existing.Fingerprint == request.Fingerprint {
			return nil
		}
		return errors.New("resource already owns a different adopted project")
	}
	for _, existing := range e.adoptedProjects {
		if existing.Project == request.Project {
			return errors.New("Compose project is already adopted")
		}
	}
	plan, resolved, err := e.planComposeAdoption(ctx, request.Project)
	if err != nil {
		return err
	}
	if plan.Fingerprint != request.Fingerprint {
		return errors.New("source project changed; scan and review adoption again")
	}
	dir := filepath.Join(e.Root, "workloads", request.Workload)
	if _, err = os.Stat(filepath.Join(dir, "current.json")); err == nil {
		var intent AdoptionRequest
		raw, readErr := os.ReadFile(filepath.Join(dir, "adoption-intent.json"))
		if readErr != nil || json.Unmarshal(raw, &intent) != nil || intent.Project != request.Project || intent.Workload != request.Workload || intent.Release != request.Release {
			return errors.New("resource already has deployment state")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.MkdirAll(filepath.Join(dir, "release-states"), 0700); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(dir, "adoption-intent.json"), request); err != nil {
		return err
	}
	services, _ := resolved["services"].(map[string]any)
	for name, raw := range services {
		service := raw.(map[string]any)
		image, _ := service["image"].(string)
		serviceHash := sha256.Sum256([]byte(name))
		tag := "cove-release/" + request.Workload + ":" + request.Release + "-" + hex.EncodeToString(serviceHash[:8])
		if err = e.Run(ctx, e.Root, []string{"docker", "image", "tag", image, tag}, nil, &limitedBuffer{Limit: 1024}); err != nil {
			return errors.New("could not retain adopted images")
		}
	}
	composePath := filepath.Join(dir, "release-states", request.Release+".compose.json")
	data, err := json.Marshal(literalCompose(resolved))
	if err != nil {
		return err
	}
	if err = os.WriteFile(composePath, data, 0600); err != nil {
		return err
	}
	state := deployedState{ComposePath: composePath, Environment: plan.Configuration.Environment}
	if err = writeJSON(filepath.Join(dir, "release-states", request.Release+".json"), state); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(dir, "current.json"), state); err != nil {
		return err
	}
	next := map[string]adoptedProject{}
	for id, entry := range e.adoptedProjects {
		next[id] = entry
	}
	next[request.Workload] = adoptedProject{request.Project, request.Fingerprint}
	if err = writeJSON(filepath.Join(e.Root, "adopted-projects.json"), next); err != nil {
		return err
	}
	e.adoptedProjects = next
	return nil
}
