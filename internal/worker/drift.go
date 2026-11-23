package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type driftContainer struct {
	Service string            `json:"service"`
	Fields  map[string]string `json:"fields"`
}

type driftBaseline struct {
	Operation  string                    `json:"operation"`
	CapturedAt time.Time                 `json:"captured_at"`
	Containers map[string]driftContainer `json:"containers"`
}

type DriftChange struct {
	Container string   `json:"container"`
	Service   string   `json:"service"`
	State     string   `json:"state"`
	Fields    []string `json:"fields"`
}

type DriftReport struct {
	Deferred   string        `json:"deferred,omitempty"`
	Baseline   bool          `json:"baseline"`
	Busy       bool          `json:"busy"`
	Operation  string        `json:"operation"`
	CapturedAt *time.Time    `json:"captured_at"`
	CheckedAt  time.Time     `json:"checked_at"`
	Changes    []DriftChange `json:"changes"`
}

func (e *Engine) runtimeConfiguration(ctx context.Context, workload string) (map[string]driftContainer, error) {
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project="+e.composeProject(workload), "--filter", "label=com.docker.compose.oneoff=False")
	if err != nil {
		return nil, err
	}
	containers := strings.Fields(ids)
	if len(containers) > 128 {
		return nil, errors.New("drift inspection supports at most 128 containers per resource")
	}
	result := map[string]driftContainer{}
	if len(containers) == 0 {
		return result, nil
	}
	for _, id := range containers {
		_, parseErr := hex.DecodeString(id)
		if len(id) != 64 || parseErr != nil {
			return nil, errors.New("invalid container identity during drift inspection")
		}
	}
	output := &limitedBuffer{Limit: 2 << 20}
	if err = e.Run(ctx, e.Root, append([]string{"docker", "inspect"}, containers...), nil, output); err != nil {
		return nil, errors.New("could not inspect resource configuration")
	}
	var inspected []map[string]any
	if json.Unmarshal(output.data.Bytes(), &inspected) != nil || len(inspected) != len(containers) {
		return nil, errors.New("invalid runtime configuration response")
	}
	selected := map[string]bool{}
	for _, id := range containers {
		selected[id] = true
	}
	for _, container := range inspected {
		id, _ := container["Id"].(string)
		config, _ := container["Config"].(map[string]any)
		labels, _ := config["Labels"].(map[string]any)
		service, _ := labels["com.docker.compose.service"].(string)
		if !selected[id] || result[id].Fields != nil || labels["com.docker.compose.project"] != e.composeProject(workload) || !serviceIdentifier.MatchString(service) || config == nil || container["HostConfig"] == nil {
			return nil, errors.New("runtime configuration is not owned by this resource")
		}
		state, _ := container["State"].(map[string]any)
		fields := map[string]any{"image": container["Image"], "container": config, "host": container["HostConfig"], "mounts": container["Mounts"], "state": map[string]any{"running": state["Running"], "restarting": state["Restarting"], "paused": state["Paused"], "dead": state["Dead"], "exit_code": state["ExitCode"]}}
		settings, _ := container["NetworkSettings"].(map[string]any)
		networks, _ := settings["Networks"].(map[string]any)
		attachments := map[string]any{}
		for name, raw := range networks {
			network, _ := raw.(map[string]any)
			aliases, _ := network["Aliases"].([]any)
			canonical := make([]string, 0, len(aliases))
			for _, alias := range aliases {
				if value, ok := alias.(string); ok {
					canonical = append(canonical, value)
				}
			}
			sort.Strings(canonical)
			attachments[name] = map[string]any{"network": network["NetworkID"], "aliases": canonical}
		}
		fields["networks"] = attachments
		digests := map[string]string{}
		for name, field := range fields {
			raw, err := json.Marshal(field)
			if err != nil {
				return nil, errors.New("could not encode runtime configuration")
			}
			digest := sha256.Sum256(raw)
			digests[name] = hex.EncodeToString(digest[:])
		}
		result[id] = driftContainer{Service: service, Fields: digests}
	}
	return result, nil
}

func (e *Engine) recordDriftBaseline(ctx context.Context, request Request) error {
	containers, err := e.runtimeConfiguration(ctx, request.Workload)
	if err != nil {
		return err
	}
	if len(containers) == 0 && request.Action != "stop" {
		return errors.New("cannot record a drift baseline without created containers")
	}
	baseline := driftBaseline{Operation: request.ID, CapturedAt: time.Now().UTC(), Containers: containers}
	return writeJSON(filepath.Join(e.Root, "workloads", request.Workload, "drift.json"), baseline)
}

func (e *Engine) Drift(ctx context.Context, workload string) (DriftReport, error) {
	report := DriftReport{CheckedAt: time.Now().UTC(), Changes: []DriftChange{}}
	if !workloadIdentifier.MatchString(workload) {
		return report, errors.New("invalid resource")
	}
	e.mu.Lock()
	if e.active[workload] {
		e.mu.Unlock()
		report.Busy = true
		return report, nil
	}
	e.active[workload] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.active, workload)
		e.mu.Unlock()
	}()
	suspension, err := e.readSuspension(workload)
	if err != nil {
		return report, err
	}
	if suspension != nil && suspension.Phase != "awake" {
		report.Busy, report.Deferred = true, "suspended"
		return report, nil
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "drift.json"))
	if os.IsNotExist(err) {
		return report, nil
	}
	var baseline driftBaseline
	if err != nil || json.Unmarshal(data, &baseline) != nil || baseline.Containers == nil {
		return report, errors.New("drift baseline unavailable")
	}
	report.Baseline, report.Operation, report.CapturedAt = true, baseline.Operation, &baseline.CapturedAt
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	actual, err := e.runtimeConfiguration(ctx, workload)
	if err != nil {
		return report, err
	}
	for id, expected := range baseline.Containers {
		current, exists := actual[id]
		change := DriftChange{Container: id, Service: expected.Service, State: "changed", Fields: []string{}}
		if !exists {
			change.State = "missing"
		} else {
			for field, digest := range expected.Fields {
				if current.Fields[field] != digest {
					change.Fields = append(change.Fields, field)
				}
			}
			sort.Strings(change.Fields)
			if len(change.Fields) == 0 {
				continue
			}
		}
		report.Changes = append(report.Changes, change)
	}
	for id, container := range actual {
		if _, exists := baseline.Containers[id]; !exists {
			report.Changes = append(report.Changes, DriftChange{Container: id, Service: container.Service, State: "added", Fields: []string{}})
		}
	}
	sort.Slice(report.Changes, func(i, j int) bool { return report.Changes[i].Container < report.Changes[j].Container })
	return report, nil
}
