package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGPUSelectionAndComposePreservation(t *testing.T) {
	uuid := "GPU-12345678-1234-1234-1234-123456789abc"
	for _, ids := range [][]string{{"all"}, {"/dev/mem"}, {uuid, uuid}, {"vendor/gpu=$(id)"}, make([]string, 17)} {
		if validateGPUSelection(&ids) == nil {
			t.Fatal("unsafe or unbounded GPU selection accepted", ids)
		}
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(`{"services":{"app":{"devices":[{"source":"/dev/ttyUSB0","target":"/dev/ttyUSB0"},{"source":"old.example/gpu=old","target":"old.example/gpu=old"}],"gpus":"all","deploy":{"resources":{"reservations":{"memory":"32m","devices":[{"driver":"nvidia","count":"all","capabilities":["gpu"]},{"driver":"example","capabilities":["other"]}]}}}}}}`), &config); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(config)
	if err := applyLimits(config, map[string]ServiceLimits{"app": {CPUs: 1}}); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(config)
	if !strings.Contains(string(after), `"gpus":"all"`) || !strings.Contains(string(before), `old.example/gpu=old`) {
		t.Fatal("CPU limit erased Compose GPU configuration")
	}
	selected := []string{uuid, "cove.test/gpu=card0"}
	if err := applyLimits(config, map[string]ServiceLimits{"app": {GPUs: &selected}}); err != nil {
		t.Fatal(err)
	}
	after, _ = json.Marshal(config)
	text := string(after)
	for _, expected := range []string{uuid, "cove.test/gpu=card0", "/dev/ttyUSB0", `"memory":"32m"`, `"capabilities":["other"]`} {
		if !strings.Contains(text, expected) {
			t.Fatal("GPU selection erased unrelated settings", text)
		}
	}
	if strings.Contains(text, `old.example/gpu=old`) || strings.Contains(text, `"gpus"`) || strings.Contains(text, `"count"`) {
		t.Fatal("GPU selection retained conflicting all-device request", text)
	}
	empty := []string{}
	if err := applyLimits(config, map[string]ServiceLimits{"app": {GPUs: &empty}}); err != nil {
		t.Fatal(err)
	}
	after, _ = json.Marshal(config)
	if strings.Contains(string(after), uuid) || strings.Contains(string(after), "cove.test/gpu=") || !strings.Contains(string(after), "/dev/ttyUSB0") {
		t.Fatal("explicit empty GPU selection did not clear only GPU requests", string(after))
	}
}

func TestGPUReportRejectsMalformedAndDuplicateDevices(t *testing.T) {
	uuid := "GPU-12345678-1234-1234-1234-123456789abc"
	row := uuid + ", NVIDIA sample, 16384\n"
	devices, err := parseNVIDIAGPUs(row)
	if err != nil || len(devices) != 1 || devices[0].MemoryMB == nil || *devices[0].MemoryMB != 16384 {
		t.Fatal("valid bounded native report rejected", devices, err)
	}
	for _, raw := range []string{row + row, "0, Card, 1024\n", uuid + ", Card, -1\n", uuid + ", Card, unknown\n", strings.Repeat("x", 65537)} {
		if _, err := parseNVIDIAGPUs(raw); err == nil {
			t.Fatal("invalid native report accepted")
		}
	}
}
