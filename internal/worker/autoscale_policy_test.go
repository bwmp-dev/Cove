package worker

import (
	"math"
	"testing"
	"time"
)

func TestCPUScalingUsesAllHealthyReplicasAndExplicitLimits(t *testing.T) {
	containers := []Container{{Service: "app", State: "running", Health: "healthy", Metrics: &ContainerMetrics{CPU: "25.00%"}}, {Service: "app", State: "running", Health: "healthy", Metrics: &ContainerMetrics{CPU: "50.00%"}}, {Service: "peer", State: "exited"}}
	value, err := ServiceCPUPercent(containers, "app", 2, 0.5)
	if err != nil || value != 75 {
		t.Fatal(value, err)
	}
	for _, test := range []struct {
		cpu   string
		limit float64
	}{{"25%", 0}, {"-1%", 1}, {"NaN%", 1}, {"+Inf%", 1}, {"unavailable", 1}, {"25%", math.Inf(1)}} {
		copy := append([]Container{}, containers...)
		copy[0].Metrics = &ContainerMetrics{CPU: test.cpu}
		if _, err := ServiceCPUPercent(copy, "app", 2, test.limit); err == nil {
			t.Fatal("invalid CPU sample accepted", test)
		}
	}
	if _, err := ServiceCPUPercent(containers, "app", 3, 1); err == nil {
		t.Fatal("partial replica set accepted")
	}
	for index := range 2 {
		containers[index].Metrics = &ContainerMetrics{CPU: "1e308%"}
	}
	if _, err := ServiceCPUPercent(containers, "app", 2, 1); err == nil {
		t.Fatal("overflowed CPU aggregate accepted")
	}
	containers[0].Health = "starting"
	if _, err := ServiceCPUPercent(containers, "app", 2, 1); err == nil {
		t.Fatal("starting replica accepted")
	}
}

func TestCPUScalingNeedsThreeConsistentFreshSamplesAndCooldown(t *testing.T) {
	now := time.Now().UTC()
	policy := AutoscalePolicy{Enabled: true, Services: map[string]AutoscaleService{"app": {MinReplicas: 1, MaxReplicas: 4, TargetCPUPercent: 70}}}
	samples := func(cpu ...float64) []CPUScaleObservation {
		out := []CPUScaleObservation{}
		for index, value := range cpu {
			out = append(out, CPUScaleObservation{SampledAt: now.Add(time.Duration(index-2) * time.Minute), Generation: "release", Replicas: map[string]int{"app": 2}, CPUPercent: map[string]float64{"app": value}})
		}
		return out
	}
	if next, reason := CPUScaleDecision(policy, samples(80, 90, 85), now, nil); reason != "scale" || next["app"] != 3 {
		t.Fatal(next, reason)
	}
	if next, reason := CPUScaleDecision(policy, samples(20, 10, 30), now, nil); reason != "scale" || next["app"] != 1 {
		t.Fatal(next, reason)
	}
	if next, reason := CPUScaleDecision(policy, samples(90, 65, 90), now, nil); reason != "within_target" || next != nil {
		t.Fatal("one lower sample was ignored", next, reason)
	}
	if next, reason := CPUScaleDecision(policy, samples(20, 40, 20), now, nil); reason != "within_target" || next != nil {
		t.Fatal("hysteresis was ignored", next, reason)
	}
	last := now.Add(-4 * time.Minute)
	if _, reason := CPUScaleDecision(policy, samples(90, 90, 90), now, &last); reason != "cooldown" {
		t.Fatal(reason)
	}
	for _, mutate := range []func([]CPUScaleObservation){
		func(s []CPUScaleObservation) { s[0].Generation = "previous" },
		func(s []CPUScaleObservation) { s[1].Replicas["app"] = 1 },
		func(s []CPUScaleObservation) { delete(s[1].CPUPercent, "app") },
		func(s []CPUScaleObservation) { s[2].CPUPercent["app"] = math.NaN() },
		func(s []CPUScaleObservation) { s[2].SampledAt = now.Add(time.Second) },
		func(s []CPUScaleObservation) { s[1].SampledAt = s[0].SampledAt },
	} {
		observations := samples(90, 90, 90)
		mutate(observations)
		if next, _ := CPUScaleDecision(policy, observations, now, nil); next != nil {
			t.Fatal("inconsistent observations changed replicas", next)
		}
	}
	if next, _ := CPUScaleDecision(policy, samples(90, 90), now, nil); next != nil {
		t.Fatal("two samples were sufficient")
	}
	if next, _ := CPUScaleDecision(policy, samples(90, 90, 90), now.Add(2*time.Minute), nil); next != nil {
		t.Fatal("stale CPU data changed replicas")
	}
}

func TestCPUScalingPolicyPreservesExistingReplicaBounds(t *testing.T) {
	policy := AutoscalePolicy{Enabled: true, Services: map[string]AutoscaleService{"app": {}}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	if policy.Services["app"].MaxReplicas != 0 {
		t.Fatal("validation mutated saved input")
	}
	for _, service := range []AutoscaleService{{MinReplicas: 4, MaxReplicas: 2, TargetCPUPercent: 70}, {MinReplicas: 1, MaxReplicas: 17, TargetCPUPercent: 70}, {MinReplicas: 1, MaxReplicas: 4, TargetCPUPercent: 91}, {MinReplicas: 1, MaxReplicas: 4, TargetCPUPercent: 9}} {
		policy.Services["app"] = service
		if err := policy.Validate(); err == nil {
			t.Fatal("invalid policy accepted", service)
		}
	}
}
