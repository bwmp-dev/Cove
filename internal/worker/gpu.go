package worker

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type GPU struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	MemoryMB   *int64 `json:"memory_mb"`
	Assignable bool   `json:"assignable"`
	Notice     string `json:"notice"`
}

type GPUInventory struct {
	CheckedAt time.Time `json:"checked_at"`
	Devices   []GPU     `json:"devices"`
	Limited   bool      `json:"limited"`
	Notices   []string  `json:"notices"`
}

var nvidiaGPUIdentifier = regexp.MustCompile(`^GPU-[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$`)
var cdiGPUIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}/gpu=[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

func validateGPUSelection(selection *[]string) error {
	if selection == nil {
		return nil
	}
	if len(*selection) > 16 {
		return errors.New("at most 16 GPU devices may be assigned per service")
	}
	seen := map[string]bool{}
	for _, id := range *selection {
		if (!nvidiaGPUIdentifier.MatchString(id) && !cdiGPUIdentifier.MatchString(id)) || seen[id] {
			return errors.New("GPU selection requires distinct NVIDIA UUIDs or qualified CDI GPU names")
		}
		seen[id] = true
	}
	return nil
}

func parseNVIDIAGPUs(raw string) ([]GPU, error) {
	if len(raw) > 65536 {
		return nil, errors.New("GPU report exceeds its size limit")
	}
	rows, err := csv.NewReader(strings.NewReader(raw)).ReadAll()
	if err != nil || len(rows) > 64 {
		return nil, errors.New("NVIDIA inventory is invalid or exceeds 64 devices")
	}
	devices := []GPU{}
	seen := map[string]bool{}
	for _, fields := range rows {
		if len(fields) != 3 {
			return nil, errors.New("invalid NVIDIA inventory columns")
		}
		id, name := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		memory, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if !nvidiaGPUIdentifier.MatchString(id) || seen[id] || len(name) == 0 || len(name) > 256 || strings.ContainsAny(name, "\r\n\x00") || err != nil || memory <= 0 || memory > 16777216 {
			return nil, errors.New("invalid NVIDIA device observation")
		}
		seen[id] = true
		devices = append(devices, GPU{ID: id, Name: name, Driver: "nvidia", MemoryMB: &memory})
	}
	return devices, nil
}

func inspectHostGPUs(ctx context.Context, root string, run snapshotCommand) GPUInventory {
	report := GPUInventory{CheckedAt: time.Now().UTC(), Devices: []GPU{}, Notices: []string{}}
	if raw, err := run(ctx, "/usr/bin/nvidia-smi", "--query-gpu=uuid,name,memory.total", "--format=csv,noheader,nounits"); err == nil {
		devices, err := parseNVIDIAGPUs(raw)
		if err != nil {
			report.Notices = append(report.Notices, err.Error())
		} else {
			report.Devices = append(report.Devices, devices...)
		}
	} else {
		report.Notices = append(report.Notices, "Native NVIDIA observations unavailable; install and configure the host driver separately.")
	}
	seen := map[string]bool{}
	for _, device := range report.Devices {
		seen[device.ID] = true
	}
	files := 0
	for _, directory := range []string{"etc/cdi", "var/run/cdi"} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			report.Notices = append(report.Notices, "A CDI inventory directory could not be read.")
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") && !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			files++
			if files > 32 {
				report.Limited = true
				break
			}
			filename := filepath.Join(root, directory, entry.Name())
			info, err := os.Lstat(filename)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 || info.Mode().Perm()&0022 != 0 {
				report.Notices = append(report.Notices, "An unsafe or oversized CDI specification was skipped.")
				continue
			}
			uid, _ := fileOwner(info)
			if uid != 0 {
				report.Notices = append(report.Notices, "A CDI specification not owned by root was skipped.")
				continue
			}
			raw, err := os.ReadFile(filename)
			var spec struct {
				Kind    string `yaml:"kind"`
				Devices []struct {
					Name string `yaml:"name"`
				} `yaml:"devices"`
			}
			if err != nil || yaml.Unmarshal(raw, &spec) != nil || len(spec.Devices) > 128 {
				report.Notices = append(report.Notices, "A CDI specification could not be read within its limits.")
				continue
			}
			for _, device := range spec.Devices {
				id := spec.Kind + "=" + device.Name
				if !cdiGPUIdentifier.MatchString(id) || seen[id] {
					continue
				}
				if len(report.Devices) >= 128 {
					report.Limited = true
					break
				}
				seen[id] = true
				report.Devices = append(report.Devices, GPU{ID: id, Name: id, Driver: "cdi"})
			}
		}
	}
	sort.Slice(report.Devices, func(i, j int) bool { return report.Devices[i].ID < report.Devices[j].ID })
	return report
}

func RunHostGPUs(ctx context.Context, input io.Reader, output io.Writer) error {
	var request HostMaintenanceRequest
	decoder := json.NewDecoder(io.LimitReader(input, 8193))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return errors.New("invalid GPU inspection request")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || request.Action != "inspect" || request.Enabled {
		return errors.New("unexpected GPU inspection data")
	}
	if err := validateHostMaintenance(request); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(inspectHostGPUs(ctx, "/", snapshotHostCommand))
}

func (e *Engine) GPUs(ctx context.Context) (inventory *GPUInventory, result error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if !e.backupMu.TryLock() {
		return nil, errors.New("wait for the current host inspection or backup")
	}
	defer e.backupMu.Unlock()
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return nil, err
	}
	binary, err := e.prepareSnapshotHostBinary()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(HostMaintenanceRequest{Action: "inspect", Root: e.Root, Owner: owner})
	if err != nil {
		return nil, err
	}
	name := "cove-gpu-" + owner
	args := []string{"docker", "run", "--rm", "--interactive", "--name", name, "--privileged", "--pid", "host", "--network", "none", "--read-only", "--log-driver", "none", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "--label", "dev.cove.backup-owner=" + owner, "--label", "dev.cove.operation=gpu", "--entrypoint", "nsenter", "busybox:1.37", "-t", "1", "-m", "-r", "-w", "--", binary, "host-gpus"}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		result = errors.Join(result, e.removeBackupHelper(cleanup, owner, name, "gpu"))
	}()
	out := &limitedBuffer{Limit: 256 << 10}
	if err = runInput(ctx, e.Root, args, nil, bytes.NewReader(data), out); err != nil {
		return nil, errors.New("host GPU inspection unavailable; verify native host tools and worker privileges")
	}
	report := GPUInventory{}
	if json.Unmarshal(out.data.Bytes(), &report) != nil || len(report.Devices) > 128 || report.CheckedAt.IsZero() {
		return nil, errors.New("invalid GPU inventory")
	}
	raw, err := e.output(ctx, "docker", "info", "--format", `{"runtimes":{{json .Runtimes}},"cdi":{{json .CDISpecDirs}}}`)
	var daemon struct {
		Runtimes map[string]json.RawMessage `json:"runtimes"`
		CDI      []string                   `json:"cdi"`
	}
	if err != nil || len(raw) > 65536 || json.Unmarshal([]byte(raw), &daemon) != nil {
		report.Notices = append(report.Notices, "Docker GPU driver availability is unknown.")
	} else {
		for i := range report.Devices {
			device := &report.Devices[i]
			device.Assignable = device.Driver == "nvidia" && daemon.Runtimes["nvidia"] != nil || device.Driver == "cdi" && (slices.Contains(daemon.CDI, "/etc/cdi") || slices.Contains(daemon.CDI, "/var/run/cdi") || slices.Contains(daemon.CDI, "/run/cdi"))
			if !device.Assignable {
				device.Notice = "Configure Docker's matching NVIDIA runtime or CDI directory before assigning this device."
			}
		}
	}
	return &report, nil
}

func (e *Engine) validateGPUs(ctx context.Context, limits map[string]ServiceLimits) error {
	requested := map[string]bool{}
	for _, limit := range limits {
		if limit.GPUs != nil {
			for _, id := range *limit.GPUs {
				requested[id] = true
			}
		}
	}
	if len(requested) == 0 {
		return nil
	}
	report, err := e.GPUs(ctx)
	if err != nil {
		return err
	}
	for _, device := range report.Devices {
		if device.Assignable {
			delete(requested, device.ID)
		}
	}
	if len(requested) > 0 {
		return errors.New("selected GPUs are absent or their Docker driver is unavailable; inspect the assigned node")
	}
	return nil
}

func applyServiceGPUs(service, resources map[string]any, selection *[]string) error {
	if selection == nil {
		return nil
	}
	if err := validateGPUSelection(selection); err != nil {
		return err
	}
	delete(service, "gpus")
	devices, _ := service["devices"].([]any)
	kept := []any{}
	for _, raw := range devices {
		device, ok := raw.(map[string]any)
		if !ok {
			return errors.New("GPU assignments require normalized Compose device mappings")
		}
		source, _ := device["source"].(string)
		if !cdiGPUIdentifier.MatchString(source) {
			kept = append(kept, device)
		}
	}
	reservations, _ := resources["reservations"].(map[string]any)
	if reservations == nil {
		reservations = map[string]any{}
		resources["reservations"] = reservations
	}
	rawReservations, _ := reservations["devices"].([]any)
	other := []any{}
	for _, raw := range rawReservations {
		device, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid Compose device reservation")
		}
		caps, _ := device["capabilities"].([]any)
		gpu := false
		for _, cap := range caps {
			gpu = gpu || cap == "gpu"
		}
		if !gpu {
			other = append(other, device)
		}
	}
	nvidia := []string{}
	for _, id := range *selection {
		if nvidiaGPUIdentifier.MatchString(id) {
			nvidia = append(nvidia, id)
		} else {
			kept = append(kept, map[string]any{"source": id, "target": id, "permissions": "rwm"})
		}
	}
	if len(nvidia) > 0 {
		other = append(other, map[string]any{"driver": "nvidia", "capabilities": []any{"gpu"}, "device_ids": nvidia})
	}
	if len(kept) > 0 {
		service["devices"] = kept
	} else {
		delete(service, "devices")
	}
	if len(other) > 0 {
		reservations["devices"] = other
	} else {
		delete(reservations, "devices")
	}
	return nil
}
