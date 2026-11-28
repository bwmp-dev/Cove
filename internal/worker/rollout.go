package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var containerIdentity = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type HTTPHealthCheck struct {
	Port int    `json:"port"`
	Path string `json:"path"`
}
type RolloutOptions struct {
	Strategy        string                     `json:"strategy"`
	Replicas        map[string]int             `json:"replicas,omitempty"`
	HTTPChecks      map[string]HTTPHealthCheck `json:"http_checks,omitempty"`
	ParallelVolumes bool                       `json:"parallel_volumes"`
	DrainSeconds    int                        `json:"drain_seconds"`
	TimeoutSeconds  int                        `json:"timeout_seconds"`
	CanaryPercent   int                        `json:"canary_percent"`
}
type RolloutStatus struct {
	Operation        string         `json:"operation"`
	Release          string         `json:"release,omitempty"`
	Strategy         string         `json:"strategy"`
	Phase            string         `json:"phase"`
	Reason           string         `json:"reason,omitempty"`
	CandidatePercent int            `json:"candidate_percent"`
	Replicas         map[string]int `json:"replicas,omitempty"`
}

func (options RolloutOptions) Normalized() RolloutOptions {
	if options.Strategy == "" {
		options.Strategy = "rolling"
	}
	if options.TimeoutSeconds == 0 {
		options.TimeoutSeconds = 120
	}
	if options.CanaryPercent == 0 {
		options.CanaryPercent = 10
	}
	return options
}

type rolloutChange struct {
	Origin          string         `json:"origin"`
	Phase           string         `json:"phase"`
	Committed       bool           `json:"committed"`
	Staged          bool           `json:"staged"`
	Previous        *deployedState `json:"previous,omitempty"`
	PreviousTraffic trafficState   `json:"previous_traffic"`
	Next            deployedState  `json:"next"`
	Options         RolloutOptions `json:"options"`
	OldContainers   []string       `json:"old_containers"`
	Networks        []string       `json:"networks"`
}

func validateRollout(r Request) error {
	if r.Rollout == nil {
		return nil
	}
	value := r.Rollout.Normalized()
	p := &value
	if p.Strategy == "disabled" {
		return nil
	}
	if r.Kind == "job" || r.Preview || r.EnvironmentPreview != nil {
		return errors.New("rollouts require a service outside managed previews")
	}
	if p.Strategy != "rolling" && p.Strategy != "blue_green" && p.Strategy != "canary" {
		return errors.New("select rolling, blue_green or canary deployment")
	}
	if p.DrainSeconds < 0 || p.DrainSeconds > 120 || p.TimeoutSeconds < 5 || p.TimeoutSeconds > 600 || p.CanaryPercent < 1 || p.CanaryPercent > 99 || len(p.Replicas) > 32 || len(p.HTTPChecks) > 32 {
		return errors.New("invalid rollout drain, health timeout, canary percentage or service count")
	}
	total := 0
	for service, count := range p.Replicas {
		if !serviceIdentifier.MatchString(service) || count < 1 || count > 16 {
			return errors.New("replicas must be between one and 16 per service")
		}
		total += count
	}
	if total > 64 {
		return errors.New("at most 64 replicas are supported")
	}
	for service, check := range p.HTTPChecks {
		validPath := strings.HasPrefix(check.Path, "/") && !strings.HasPrefix(check.Path, "//") && len(check.Path) <= 1024
		for _, ch := range check.Path {
			if ch < 32 || ch == 127 {
				validPath = false
			}
		}
		if !serviceIdentifier.MatchString(service) || check.Port < 1 || check.Port > 65535 || !validPath {
			return errors.New("HTTP health checks require a service, port and absolute path")
		}
	}
	return nil
}

func rolloutEligibility(r Request, config map[string]any) string {
	if r.Rollout == nil || r.Rollout.Strategy == "disabled" {
		return "Zero-downtime replacement is disabled; using guarded Compose replacement."
	}
	if HasTransportRoutes(r.Routes) {
		return "Raw TCP and UDP routes require guarded Compose replacement; HTTP generation traffic handoff is unavailable."
	}
	if len(r.Routes) == 0 {
		return "A managed HTTP route is required for an atomic traffic switch."
	}
	hooks := r.Hooks.Enabled()
	if r.Private != nil || len(r.Networks) > 0 || hooks {
		return "Private connections, shared networks or deployment hooks require guarded Compose replacement."
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 32 {
		return "The Compose service set is unsupported."
	}
	total := 0
	for name, raw := range services {
		count := 1
		if r.Rollout.Replicas[name] > 0 {
			count = r.Rollout.Replicas[name]
		}
		total += count
		service, ok := raw.(map[string]any)
		if !ok {
			return "Invalid Compose service."
		}
		for _, key := range []string{"ports", "devices", "volumes_from", "container_name", "network_mode", "pid", "ipc", "cap_add", "privileged", "post_start", "pre_stop", "links", "profiles"} {
			if value := service[key]; value != nil && value != false {
				return fmt.Sprintf("Service %s uses %s, which cannot run alongside the old generation safely.", name, key)
			}
		}
		if mounts, _ := service["volumes"].([]any); !r.Rollout.ParallelVolumes {
			for _, mount := range mounts {
				m, _ := mount.(map[string]any)
				if m["read_only"] != true {
					return fmt.Sprintf("Service %s has writable storage; assert concurrent writers are safe or use guarded replacement.", name)
				}
			}
		}
	}
	if total > 64 {
		return "At most 64 replicas including services without an override are supported."
	}
	for name := range r.Rollout.Replicas {
		if services[name] == nil {
			return "A replica setting names an absent Compose service."
		}
	}
	for name := range r.Rollout.HTTPChecks {
		if services[name] == nil {
			return "An HTTP check names an absent Compose service."
		}
	}
	networks, _ := config["networks"].(map[string]any)
	for _, raw := range networks {
		network, _ := raw.(map[string]any)
		ipam, _ := network["ipam"].(map[string]any)
		if network["external"] == true || len(ipam) > 0 {
			return "External networks or explicit address pools require guarded replacement."
		}
	}
	return ""
}

func generationName(operation, service string) string {
	sum := sha256.Sum256([]byte(operation + "/" + service))
	return "coveg-" + hex.EncodeToString(sum[:12])
}

func generationConfig(r Request, config map[string]any) (trafficState, []string, error) {
	services := config["services"].(map[string]any)
	declared, _ := config["networks"].(map[string]any)
	networkKeys := map[string]string{}
	for key := range declared {
		networkKeys[key] = generationName(r.ID, "network/"+key)
	}
	names := map[string]string{}
	traffic := trafficState{}
	for name := range services {
		names[name] = generationName(r.ID, name)
		traffic[name] = []trafficTarget{{Service: names[name], Weight: 100}}
	}
	next := map[string]any{}
	for name, raw := range services {
		service := raw.(map[string]any)
		labels, _ := service["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels["dev.cove.service"], labels["dev.cove.generation"] = name, r.ID
		service["labels"] = labels
		if dependencies, ok := service["depends_on"].(map[string]any); ok {
			renamed := map[string]any{}
			for key, value := range dependencies {
				if names[key] == "" {
					return nil, nil, errors.New("unknown generation dependency")
				}
				renamed[names[key]] = value
			}
			service["depends_on"] = renamed
		}
		count := 1
		if n := r.Rollout.Replicas[name]; n > 0 {
			count = n
		}
		service["scale"] = count
		if deploy, ok := service["deploy"].(map[string]any); ok {
			delete(deploy, "replicas")
		}
		attachments, _ := service["networks"].(map[string]any)
		renamedNetworks := map[string]any{}
		for key, value := range attachments {
			attachment, _ := value.(map[string]any)
			if attachment == nil {
				attachment = map[string]any{}
			}
			if attachment["ipv4_address"] != nil || attachment["ipv6_address"] != nil {
				return nil, nil, errors.New("fixed container addresses cannot run in parallel generations")
			}
			aliases, _ := attachment["aliases"].([]any)
			attachment["aliases"] = append(aliases, name)
			if networkKeys[key] == "" {
				return nil, nil, errors.New("unknown generation network")
			}
			renamedNetworks[networkKeys[key]] = attachment
		}
		service["networks"] = renamedNetworks
		next[names[name]] = service
	}
	config["services"] = next
	networks := []string{}
	nextNetworks := map[string]any{}
	for key, raw := range declared {
		network, _ := raw.(map[string]any)
		if network == nil {
			network = map[string]any{}
		}
		name := "cove-" + r.Workload + "-" + networkKeys[key]
		network["name"] = name
		labels, _ := network["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels["dev.cove.generation"] = r.ID
		network["labels"] = labels
		nextNetworks[networkKeys[key]] = network
		networks = append(networks, name)
	}
	config["networks"] = nextNetworks
	sort.Strings(networks)
	return traffic, networks, nil
}

func (e *Engine) updateRolloutJournal(id string, change rolloutChange) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	j.RolloutChange = &change
	return writeJSON(e.path(id), j)
}

func (e *Engine) decideRollout(id string, change rolloutChange, status RolloutStatus) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	if j.CancelRequested {
		return errRolloutCanceled
	}
	j.RolloutChange = &change
	j.Operation.Rollout = &status
	return writeJSON(e.path(id), j)
}

var errRolloutCanceled = errors.New("deployment canceled before the traffic decision committed")

func (e *Engine) pendingRollout(workload string) (string, *rolloutChange, error) {
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "rollout.json"))
	if os.IsNotExist(err) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var id string
	if json.Unmarshal(data, &id) != nil || !identifier.MatchString(id) {
		return "", nil, errors.New("invalid rollout marker")
	}
	j, err := e.read(id)
	if err != nil {
		return "", nil, err
	}
	if j.Request.Workload != workload || j.RolloutChange == nil || !identifier.MatchString(j.RolloutChange.Origin) {
		return "", nil, errors.New("rollout marker does not match its journal")
	}
	return id, j.RolloutChange, nil
}

func (e *Engine) rolloutStatus(workload string) (*RolloutStatus, error) {
	id, change, err := e.pendingRollout(workload)
	if err != nil || change == nil {
		return nil, err
	}
	percent := 0
	if change.Committed {
		percent = 100
	} else if change.Phase == "waiting" || change.Phase == "promoting" || change.Phase == "aborting" {
		if change.Options.Strategy == "canary" {
			percent = change.Options.CanaryPercent
		}
	}
	return &RolloutStatus{Operation: id, Release: change.Origin, Strategy: change.Options.Strategy, Phase: change.Phase, CandidatePercent: percent, Replicas: change.Options.Replicas}, nil
}

func (e *Engine) writeTraffic(workload string, state trafficState) error {
	return writeJSON(filepath.Join(e.Root, "workloads", workload, "traffic.json"), state)
}

func (e *Engine) waitRolloutHealth(ctx context.Context, r Request, traffic trafficState) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.Rollout.TimeoutSeconds)*time.Second)
	defer cancel()
	for {
		ready := true
		for service, targets := range traffic {
			count := 1
			if replicas := r.Rollout.Replicas[service]; replicas > 0 {
				count = replicas
			}
			ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+e.composeProject(r.Workload), "--filter", "label=com.docker.compose.service="+targets[0].Service)
			if err != nil {
				return err
			}
			containers := strings.Fields(ids)
			if len(containers) != count {
				ready = false
			}
			for _, id := range containers {
				if !containerIdentity.MatchString(id) {
					return errors.New("invalid replica identity")
				}
				var status struct {
					Running bool
					Health  string
				}
				if err = e.imageJSON(ctx, nil, &status, "docker", "inspect", "--format", `{"running":{{json .State.Running}},"health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}}}`, id); err != nil {
					return err
				}
				if !status.Running || status.Health == "unhealthy" {
					return fmt.Errorf("service %s failed its health gate", service)
				}
				if status.Health == "starting" {
					ready = false
					continue
				}
				if status.Health == "" {
					check, ok := r.Rollout.HTTPChecks[service]
					if !ok {
						return fmt.Errorf("service %s has no health check", service)
					}
					image, err := e.linkGatewayImage(ctx)
					if err != nil {
						return err
					}
					endpoint := "http://127.0.0.1:" + strconv.Itoa(check.Port) + check.Path
					if err = e.Run(ctx, e.Root, []string{"docker", "run", "--rm", "--network", "container:" + id, "--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges:true", "--pids-limit", "32", "--memory", "48m", "--label", "dev.cove.operation=" + r.ID, "--entrypoint", "/cove-worker", image, "healthcheck", endpoint}, nil, io.Discard); err != nil {
						ready = false
					}
				}
			}
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("replacement health check timed out")
		case <-time.After(time.Second):
		}
	}
}

func HTTPHealth(ctx context.Context, address string) error {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil {
		return errors.New("health checks require a loopback HTTP address")
	}
	client := http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("HTTP health check returned a non-success response")
	}
	return nil
}

func (e *Engine) performRollout(ctx context.Context, r Request, dir string, args, env []string, out io.Writer) (result error) {
	if err := e.validateIdleCandidate(ctx, r, dir, args, env); err != nil {
		return err
	}
	if r.Rollout != nil {
		options := r.Rollout.Normalized()
		r.Rollout = &options
	}
	if _, pending, err := e.pendingRollout(r.Workload); err != nil || pending != nil {
		if err != nil {
			return err
		}
		return errors.New("promote or abort the staged deployment before deploying again")
	}
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return err
	}
	var config map[string]any
	if err := decodeResolvedCompose(resolved.data.Bytes(), &config); err != nil {
		return err
	}
	reason := rolloutEligibility(r, config)
	if reason != "" {
		if r.Scale != nil {
			return errors.New(reason)
		}
		return e.guardedRolloutFallback(ctx, r, dir, args, env, out, reason)
	}
	if r.Scale == nil {
		if err := e.Run(ctx, dir, append(append([]string{}, args...), "pull", "--ignore-buildable"), env, out); err != nil {
			return err
		}
	}
	for name, raw := range config["services"].(map[string]any) {
		service := raw.(map[string]any)
		reference, _ := service["image"].(string)
		if reference == "" {
			reference = "cove-" + r.Workload + "-" + name
		}
		id, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", reference)
		if err != nil || !imageDigest.MatchString(id) {
			return errors.New("replacement image is unavailable")
		}
		declared, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Volumes}}", id)
		if err != nil {
			return err
		}
		if declared != "null" && declared != "{}" && !r.Rollout.ParallelVolumes {
			return e.guardedRolloutFallback(ctx, r, dir, args, env, out, "An image declares writable storage; parallel writers were not asserted safe.")
		}
		health, _ := service["healthcheck"].(map[string]any)
		test, _ := health["test"].([]any)
		hasHealth := len(test) > 0 && test[0] != "NONE" && health["disable"] != true
		if len(test) == 0 && health["disable"] != true {
			var inherited *struct{ Test []string }
			if err := e.imageJSON(ctx, nil, &inherited, "docker", "image", "inspect", "--format", "{{json .Config.Healthcheck}}", id); err != nil {
				return err
			}
			hasHealth = inherited != nil && len(inherited.Test) > 0 && inherited.Test[0] != "NONE"
		}
		if _, ok := r.Rollout.HTTPChecks[name]; !hasHealth && !ok {
			return e.guardedRolloutFallback(ctx, r, dir, args, env, out, fmt.Sprintf("Service %s needs a Docker healthcheck or a configured HTTP check.", name))
		}
		service["image"], service["pull_policy"] = id, "never"
		labels, _ := service["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		if saved, _ := labels["dev.cove.image-reference"].(string); saved == "" {
			labels["dev.cove.image-reference"] = reference
		}
		service["labels"] = labels
		delete(service, "build")
	}
	traffic, networks, err := generationConfig(r, config)
	if err != nil {
		return e.guardedRolloutFallback(ctx, r, dir, args, env, out, err.Error())
	}
	path := filepath.Join(dir, r.ID+".generation.compose.json")
	if err = writeJSON(path, literalCompose(config)); err != nil {
		return err
	}
	previous, released, err := readCurrent(filepath.Join(dir, "current.json"))
	if err != nil {
		return err
	}
	priorTraffic, err := e.loadTraffic(r.Workload)
	if err != nil {
		return err
	}
	old, err := e.Containers(ctx, r.Workload)
	if err != nil {
		return err
	}
	change := rolloutChange{Origin: r.ID, Phase: "preparing", PreviousTraffic: priorTraffic, Options: *r.Rollout, Networks: networks, Next: deployedState{ComposePath: path, Environment: r.Environment, Routes: r.Routes, Traffic: traffic, Generation: r.ID}}
	if released {
		change.Previous = &previous
	}
	for _, container := range old {
		change.OldContainers = append(change.OldContainers, container.ID)
	}
	if err = e.updateRolloutJournal(r.ID, change); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(dir, "rollout.json"), r.ID); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if !change.Staged && (change.Phase != "waiting" || result != nil) {
			if err := e.finishRollout(cleanup, r.Workload, r.ID, &change, out); err != nil {
				result = errors.Join(result, errRolloutUnresolved, err)
			}
		}
	}()
	command := []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", path}
	if err = e.Run(ctx, dir, append(append([]string{}, command...), "up", "--detach", "--no-build", "--pull", "never"), env, out); err != nil {
		return err
	}
	if err = e.waitRolloutHealth(ctx, r, traffic); err != nil {
		return err
	}
	state, err := e.captureRelease(ctx, r, dir, command, env)
	if err != nil {
		return err
	}
	state.Routes, state.Networks, state.Private, state.Traffic, state.Generation, state.Rollout = r.Routes, r.Networks, r.Private, traffic, r.ID, r.Rollout
	state.PublicBuiltins, state.AdditionalSecrets = r.resolvedPublicBuiltins, r.resolvedSecrets
	change.Next = state
	if err = e.updateRolloutJournal(r.ID, change); err != nil {
		return err
	}
	if r.Rollout.Strategy != "rolling" && released {
		mixed := trafficState{}
		percent := 0
		if r.Rollout.Strategy == "canary" {
			percent = r.Rollout.CanaryPercent
		}
		for service, next := range traffic {
			old := priorTraffic[service]
			if len(old) == 0 {
				old = []trafficTarget{{Service: service, Weight: 100}}
			}
			if len(old) != 1 {
				return errors.New("old deployment already has multiple traffic generations")
			}
			mixed[service] = []trafficTarget{{Service: old[0].Service, Weight: 100 - percent}, {Service: next[0].Service, Weight: percent}}
		}
		if err = e.writeTraffic(r.Workload, mixed); err != nil {
			return err
		}
		if err = e.applyIngress(ctx, r.Workload, r.Routes, out); err != nil {
			return err
		}
		change.Phase, change.Staged = "waiting", true
		err = e.decideRollout(r.ID, change, RolloutStatus{Operation: r.ID, Release: r.ID, Strategy: r.Rollout.Strategy, Phase: "waiting", CandidatePercent: percent, Replicas: r.Rollout.Replicas})
		if err != nil {
			change.Staged = false
			if errors.Is(err, errRolloutCanceled) {
				change.Phase = "preparing"
				return err
			}
			change.Phase = "uncertain"
			return errors.Join(errRolloutUnresolved, err)
		}
		return nil
	}
	return e.commitRollout(ctx, r.Workload, r.ID, &change, out)
}

var errRolloutUnresolved = errors.New("deployment traffic recovery is pending")

func (e *Engine) recordRolloutResult(id string, status RolloutStatus) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	j.Operation.Rollout = &status
	return writeJSON(e.path(id), j)
}

func (e *Engine) commitRollout(ctx context.Context, workload, id string, change *rolloutChange, out io.Writer) error {
	if err := e.writeTraffic(workload, change.Next.Traffic); err != nil {
		return err
	}
	if err := e.applyIngress(ctx, workload, change.Next.Routes, out); err != nil {
		return err
	}
	dir := filepath.Join(e.Root, "workloads", workload)
	if err := writeJSON(filepath.Join(dir, "release-states", change.Origin+".json"), change.Next); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), change.Next); err != nil {
		return err
	}
	change.Committed, change.Phase = true, "draining"
	if err := e.decideRollout(id, *change, RolloutStatus{Operation: id, Release: change.Origin, Strategy: change.Options.Strategy, Phase: "completed", CandidatePercent: 100, Replicas: change.Options.Replicas}); err != nil {
		if errors.Is(err, errRolloutCanceled) {
			change.Committed, change.Phase = false, "preparing"
			return err
		}
		change.Committed, change.Phase = false, "uncertain"
		return errors.Join(errRolloutUnresolved, err)
	}
	return nil
}

func (e *Engine) finishRollout(ctx context.Context, workload, id string, change *rolloutChange, out io.Writer) error {
	if !workloadIdentifier.MatchString(workload) || !identifier.MatchString(change.Origin) {
		return errors.New("invalid rollout recovery identity")
	}
	dir := filepath.Join(e.Root, "workloads", workload)
	if change.Phase == "uncertain" {
		return errRolloutUnresolved
	}
	if change.Committed {
		if err := e.writeTraffic(workload, change.Next.Traffic); err != nil {
			return err
		}
		if err := e.applyIngress(ctx, workload, change.Next.Routes, out); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "current.json"), change.Next); err != nil {
			return err
		}
		if change.Options.DrainSeconds > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(change.Options.DrainSeconds) * time.Second):
			}
		}
		var retiring sync.WaitGroup
		failures := make(chan error, len(change.OldContainers))
		concurrency := make(chan struct{}, 16)
		for _, container := range change.OldContainers {
			retiring.Go(func() {
				select {
				case concurrency <- struct{}{}:
				case <-ctx.Done():
					failures <- ctx.Err()
					return
				}
				defer func() { <-concurrency }()
				if err := e.removeRolloutContainer(ctx, workload, container, false, io.Discard); err != nil {
					failures <- err
				}
			})
		}
		retiring.Wait()
		close(failures)
		var failure error
		for err := range failures {
			failure = errors.Join(failure, err)
		}
		if failure != nil {
			return failure
		}
		if change.Previous != nil && change.Previous.Generation != "" {
			networks, err := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project="+e.composeProject(workload), "--filter", "label=dev.cove.generation="+change.Previous.Generation)
			if err != nil {
				return err
			}
			for _, network := range strings.Fields(networks) {
				if err = e.Run(ctx, e.Root, []string{"docker", "network", "rm", network}, nil, out); err != nil {
					return err
				}
			}
		}
	} else {
		suspension, err := e.readSuspension(workload)
		if err != nil {
			return err
		}
		if change.Previous != nil && (suspension == nil || suspension.Phase == "awake") {
			previous := change.Previous
			env := []string{}
			for key, value := range previous.Environment {
				env = append(env, key+"="+value)
			}
			args := []string{"docker", "compose", "--project-name", e.composeProject(workload), "--file", previous.ComposePath, "up", "--detach", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "120"}
			if change.Next.Generation != "" {
				args = append(args, "--no-recreate")
			}
			if err := e.Run(ctx, dir, args, env, io.Discard); err != nil {
				return errors.New("previous deployment could not be recovered")
			}
		}
		if err := e.writeTraffic(workload, change.PreviousTraffic); err != nil {
			return err
		}
		routes := []Route{}
		if change.Previous != nil {
			routes = change.Previous.Routes
		}
		if err := e.applyIngress(ctx, workload, routes, out); err != nil {
			return err
		}
		if change.Previous != nil {
			if err := writeJSON(filepath.Join(dir, "current.json"), change.Previous); err != nil {
				return err
			}
		} else if err := os.Remove(filepath.Join(dir, "current.json")); err != nil && !os.IsNotExist(err) {
			return err
		}
		ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+e.composeProject(workload), "--filter", "label=dev.cove.generation="+change.Origin)
		if err != nil {
			return err
		}
		for _, container := range strings.Fields(ids) {
			if err = e.removeRolloutContainer(ctx, workload, container, true, out); err != nil {
				return err
			}
		}
		for _, network := range change.Networks {
			ids, listErr := e.output(ctx, "docker", "network", "ls", "--format", "{{.Name}}", "--filter", "label=com.docker.compose.project="+e.composeProject(workload), "--filter", "label=dev.cove.generation="+change.Origin)
			if listErr != nil {
				return listErr
			}
			if slices.Contains(strings.Fields(ids), network) {
				if err = e.Run(ctx, e.Root, []string{"docker", "network", "rm", network}, nil, out); err != nil {
					return err
				}
			}
		}
		if !identifier.MatchString(change.Origin) || change.Previous != nil && change.Previous.Generation == change.Origin {
			return errors.New("invalid abandoned generation identity")
		}
		prefix := "cove-release/" + workload + ":" + strings.ToLower(change.Origin) + "-"
		tags, err := e.output(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+prefix+"*")
		if err != nil {
			return err
		}
		for _, tag := range strings.Fields(tags) {
			if !strings.HasPrefix(tag, prefix) {
				return errors.New("unexpected abandoned release tag")
			}
			if err := e.Run(ctx, dir, []string{"docker", "image", "rm", tag}, nil, out); err != nil {
				return err
			}
		}
		for _, suffix := range []string{".json", ".compose.json"} {
			if err := os.Remove(filepath.Join(dir, "release-states", change.Origin+suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		for _, path := range []string{filepath.Join(dir, "releases", change.Origin), filepath.Join(e.Root, "file-secrets", workload, change.Origin)} {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	if change.Committed {
		if err := e.pruneReleases(ctx, workload, out); err != nil {
			return err
		}
	}
	for _, suffix := range []string{".generation.compose.json", ".guarded-replacement.compose.json", ".rollback-input.compose.json"} {
		if err := os.Remove(filepath.Join(dir, change.Origin+suffix)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	err := os.Remove(filepath.Join(dir, "rollout.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (e *Engine) removeRolloutContainer(ctx context.Context, workload, id string, force bool, out io.Writer) error {
	if !containerIdentity.MatchString(id) {
		return errors.New("invalid retired container identity")
	}
	project, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id)
	if err != nil {
		ids, listErr := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "id="+id)
		if listErr != nil {
			return listErr
		}
		if ids == "" {
			return nil
		}
		return err
	}
	if project != e.composeProject(workload) {
		return errors.New("retired container belongs to another workload")
	}
	if !force {
		if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", id}, nil, out); err != nil {
			return err
		}
	}
	return e.Run(ctx, e.Root, []string{"docker", "rm", "--force", "--volumes", id}, nil, out)
}
