package worker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (e *Engine) ScanComposeProjects(ctx context.Context) ([]ComposeProject, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out := &limitedBuffer{Limit: 1 << 20}
	if err := e.Run(ctx, e.Root, []string{"docker", "ps", "-a", "--filter", "label=com.docker.compose.project", "--format", `{{.Label "com.docker.compose.project"}}\t{{.Label "com.docker.compose.service"}}`}, nil, out); err != nil {
		return nil, errors.New("could not scan Compose projects")
	}
	projects := map[string]*ComposeProject{}
	for _, line := range strings.Split(out.data.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 2 || !composeProjectIdentifier.MatchString(parts[0]) || !serviceIdentifier.MatchString(parts[1]) {
			continue
		}
		project := projects[parts[0]]
		if project == nil {
			if len(projects) >= 128 {
				return nil, errors.New("scan exceeds 128 Compose projects")
			}
			project = &ComposeProject{Project: parts[0], Services: []string{}, Blockers: []string{}}
			projects[parts[0]] = project
		}
		project.Containers++
		found := false
		for _, service := range project.Services {
			found = found || service == parts[1]
		}
		if !found {
			project.Services = append(project.Services, parts[1])
		}
	}
	result := []ComposeProject{}
	e.adoptionMu.RLock()
	defer e.adoptionMu.RUnlock()
	for _, project := range projects {
		if !validAdoptionProject(project.Project) {
			project.Blockers = append(project.Blockers, "System and Cove projects cannot be adopted.")
		}
		for _, registered := range e.adoptedProjects {
			if registered.Project == project.Project {
				project.Blockers = append(project.Blockers, "This Compose project is already adopted.")
			}
		}
		sort.Strings(project.Services)
		result = append(result, *project)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Project < result[j].Project })
	return result, nil
}

type adoptionContainer struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Image  string            `json:"Image"`
	} `json:"Config"`
	HostConfig json.RawMessage `json:"HostConfig"`
	Mounts     json.RawMessage `json:"Mounts"`
	State      struct {
		Status string `json:"Status"`
	} `json:"State"`
}

func (e *Engine) PlanComposeAdoption(ctx context.Context, project string) (*AdoptionPlan, error) {
	e.adoptionMu.Lock()
	defer e.adoptionMu.Unlock()
	for _, entry := range e.adoptedProjects {
		if entry.Project == project {
			return nil, errors.New("project is already adopted")
		}
	}
	plan, _, err := e.planComposeAdoption(ctx, project)
	return plan, err
}

func (e *Engine) planComposeAdoption(ctx context.Context, project string) (*AdoptionPlan, map[string]any, error) {
	if !validAdoptionProject(project) {
		return nil, nil, errors.New("system and Cove projects cannot be adopted")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return nil, nil, errors.New("could not inspect source project")
	}
	containers := strings.Fields(ids)
	if len(containers) == 0 || len(containers) > 128 {
		return nil, nil, errors.New("adoption requires 1-128 existing containers")
	}
	for _, id := range containers {
		if _, err = hex.DecodeString(id); err != nil || len(id) != 64 {
			return nil, nil, errors.New("invalid source container identity")
		}
	}
	inspected := &limitedBuffer{Limit: 2 << 20}
	if err = e.Run(ctx, e.Root, append([]string{"docker", "inspect"}, containers...), nil, inspected); err != nil {
		return nil, nil, errors.New("could not inspect source containers")
	}
	var details []adoptionContainer
	if json.Unmarshal(inspected.data.Bytes(), &details) != nil || len(details) != len(containers) {
		return nil, nil, errors.New("invalid source inspection")
	}
	selected := map[string]bool{}
	for _, id := range containers {
		selected[id] = true
	}
	images := map[string]string{}
	hashes := map[string]string{}
	workingDir, files := "", ""
	for _, container := range details {
		labels := container.Config.Labels
		service := labels["com.docker.compose.service"]
		if !selected[container.ID] || labels["com.docker.compose.project"] != project || !serviceIdentifier.MatchString(service) || !strings.EqualFold(labels["com.docker.compose.oneoff"], "false") {
			return nil, nil, errors.New("source contains one-off or inconsistent containers")
		}
		delete(selected, container.ID)
		if !strings.HasPrefix(container.Image, "sha256:") || len(container.Image) != 71 || images[service] != "" && images[service] != container.Image {
			return nil, nil, errors.New("source replicas use different images")
		}
		if container.State.Status != "running" && container.State.Status != "exited" && container.State.Status != "created" {
			return nil, nil, errors.New("source containers must be stable before adoption")
		}
		if strings.Contains(container.Config.Image, "coollabsio/coolify") || strings.Contains(container.Config.Image, "coolify-proxy") || strings.Contains(container.Config.Image, "portainer/portainer") || strings.Contains(container.Config.Image, "caprover/caprover") {
			return nil, nil, errors.New("manager containers cannot be adopted")
		}
		if labels["com.docker.compose.project.environment_file"] != "" {
			return nil, nil, errors.New("projects with a custom Compose environment file require a flattened source file before adoption")
		}
		if labels["com.docker.compose.config-hash"] == "" || hashes[service] != "" && hashes[service] != labels["com.docker.compose.config-hash"] {
			return nil, nil, errors.New("source configuration hashes are missing or inconsistent")
		}
		images[service], hashes[service] = container.Image, labels["com.docker.compose.config-hash"]
		if workingDir != "" && workingDir != labels["com.docker.compose.project.working_dir"] || files != "" && files != labels["com.docker.compose.project.config_files"] {
			return nil, nil, errors.New("source containers disagree about their Compose files")
		}
		workingDir, files = labels["com.docker.compose.project.working_dir"], labels["com.docker.compose.project.config_files"]
	}
	if !filepath.IsAbs(workingDir) || files == "" {
		return nil, nil, errors.New("original Compose files and working directory are required for adoption")
	}
	args := []string{"docker", "compose", "--project-name", project, "--project-directory", workingDir}
	paths := strings.Split(files, ",")
	if len(paths) > 8 {
		return nil, nil, errors.New("source uses more than eight Compose files")
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, nil, errors.New("source Compose paths must be absolute")
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, nil, errors.New("original Compose files are unavailable to this worker; mount them read-only or use configuration import")
		}
		args = append(args, "--file", path)
	}
	args = append(args, "--profile", "*")
	hashOutput := &limitedBuffer{Limit: 64 << 10}
	if err = e.Run(ctx, workingDir, append(append([]string{}, args...), "config", "--hash", "*"), nil, hashOutput); err != nil {
		return nil, nil, errors.New("source Compose hashes could not be verified")
	}
	verified := map[string]bool{}
	for _, line := range strings.Split(hashOutput.data.String(), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && hashes[parts[0]] == parts[1] {
			verified[parts[0]] = true
		}
	}
	for service := range images {
		if !verified[service] {
			return nil, nil, errors.New("source Compose configuration differs from existing containers; reconcile it in the source manager first")
		}
	}
	resolvedOutput := &limitedBuffer{Limit: 1 << 20}
	if err = e.Run(ctx, workingDir, append(append([]string{}, args...), "config", "--format", "json"), nil, resolvedOutput); err != nil {
		return nil, nil, errors.New("source Compose configuration could not be resolved")
	}
	var resolved map[string]any
	if unmarshalCompose(resolvedOutput.data.Bytes(), &resolved) != nil {
		return nil, nil, errors.New("invalid source Compose configuration")
	}
	for _, kind := range []string{"configs", "secrets"} {
		if objects, ok := resolved[kind].(map[string]any); ok {
			for _, raw := range objects {
				if object, ok := raw.(map[string]any); ok && object["environment"] != nil {
					return nil, nil, errors.New("process-environment-backed configs and secrets cannot be recovered from container creation metadata; preserve them explicitly before adopting")
				}
			}
		}
	}
	services, ok := resolved["services"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("source has no services")
	}
	for name, raw := range services {
		if images[name] == "" {
			delete(services, name)
			continue
		}
		service, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, errors.New("invalid source service")
		}
		service["image"], service["pull_policy"] = images[name], "never"
		delete(service, "build")
		delete(service, "profiles")
	}
	if len(services) != len(images) {
		return nil, nil, errors.New("source containers are absent from the Compose declaration")
	}
	sort.Strings(containers)
	sort.Slice(details, func(i, j int) bool { return details[i].ID < details[j].ID })
	fingerprint, err := e.adoptionFingerprint(struct {
		Project       string
		Containers    []adoptionContainer
		Configuration map[string]any
	}{project, details, resolved})
	if err != nil {
		return nil, nil, err
	}
	copyData, _ := json.Marshal(resolved)
	var privateCopy map[string]any
	_ = json.Unmarshal(copyData, &privateCopy)
	protected, err := protectImportedCompose(privateCopy)
	if err != nil {
		return nil, nil, err
	}
	return &AdoptionPlan{Project: project, Fingerprint: fingerprint, Configuration: *protected}, resolved, nil
}
