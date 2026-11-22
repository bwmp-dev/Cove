package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

type DeploymentAssertions struct {
	Volumes  []VolumeAssertion  `json:"volumes,omitempty"`
	Networks []NetworkAssertion `json:"networks,omitempty"`
	Services []ServiceAssertion `json:"services,omitempty"`
}
type VolumeAssertion struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}
type NetworkAssertion struct {
	Name     string            `json:"name"`
	Internal bool              `json:"internal"`
	Labels   map[string]string `json:"labels,omitempty"`
}
type ServiceAssertion struct {
	Service  string           `json:"service"`
	Image    string           `json:"image"`
	Networks []string         `json:"networks"`
	Mounts   []MountAssertion `json:"mounts"`
}
type MountAssertion struct {
	Volume   string `json:"volume"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

var assertionObjectName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var assertionImageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var assertionContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validateDeploymentAssertions(a *DeploymentAssertions) error {
	if a == nil {
		return nil
	}
	bad := errors.New("deployment assertions require bounded Docker names, image IDs, absolute mount targets and labels")
	if len(a.Volumes) > 32 || len(a.Networks) > 16 || len(a.Services) > 16 {
		return bad
	}
	labels := func(m map[string]string) bool {
		if len(m) > 16 {
			return false
		}
		for k, v := range m {
			if len(k) == 0 || len(k) > 256 || len(v) > 1024 {
				return false
			}
		}
		return true
	}
	for _, v := range a.Volumes {
		if !assertionObjectName.MatchString(v.Name) || !labels(v.Labels) {
			return bad
		}
	}
	for _, n := range a.Networks {
		if !assertionObjectName.MatchString(n.Name) || !labels(n.Labels) {
			return bad
		}
	}
	for _, s := range a.Services {
		if !serviceIdentifier.MatchString(s.Service) || !assertionImageID.MatchString(s.Image) || len(s.Networks) == 0 || len(s.Networks) > 16 || len(s.Mounts) > 32 {
			return bad
		}
		for _, n := range s.Networks {
			if !assertionObjectName.MatchString(n) {
				return bad
			}
		}
		for _, m := range s.Mounts {
			if !assertionObjectName.MatchString(m.Volume) || len(m.Target) == 0 || m.Target[0] != '/' || len(m.Target) > 1024 {
				return bad
			}
		}
	}
	return nil
}

func (e *Engine) checkDeploymentAssertions(ctx context.Context, dir string, args, env []string, a *DeploymentAssertions) error {
	if a == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	fail := func(kind string, i int) error {
		return fmt.Errorf("pre-deployment %s assertion %d failed; application replacement was not started", kind, i+1)
	}
	inspect := func(command []string, target any) bool {
		b := &limitedBuffer{Limit: 2 * 1024 * 1024}
		return e.Run(ctx, dir, command, env, b) == nil && json.Unmarshal(b.data.Bytes(), target) == nil
	}
	matchLabels := func(actual, expected map[string]string) bool {
		for k, v := range expected {
			if got, ok := actual[k]; !ok || got != v {
				return false
			}
		}
		return true
	}
	var config struct {
		Services map[string]struct {
			Image    string                     `json:"image"`
			Ports    []json.RawMessage          `json:"ports"`
			Networks map[string]json.RawMessage `json:"networks"`
			Volumes  []struct {
				Type, Source, Target string
				ReadOnly             bool `json:"read_only"`
			} `json:"volumes"`
		} `json:"services"`
		Networks map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"networks"`
		Volumes map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"volumes"`
	}
	if len(a.Services) > 0 && !inspect(append(slices.Clone(args), "config", "--format", "json"), &config) {
		return errors.New("could not resolve deployment assertion configuration")
	}
	for i, v := range a.Volumes {
		var got []struct {
			Name, Driver    string
			Options, Labels map[string]string
		}
		if !inspect([]string{"docker", "volume", "inspect", v.Name}, &got) || len(got) != 1 || got[0].Name != v.Name || got[0].Driver != "local" || len(got[0].Options) != 0 || !matchLabels(got[0].Labels, v.Labels) {
			return fail("volume", i)
		}
	}
	for i, n := range a.Networks {
		var got []struct {
			Name, Driver string
			Internal     bool
			Labels       map[string]string
		}
		if !inspect([]string{"docker", "network", "inspect", n.Name}, &got) || len(got) != 1 || got[0].Name != n.Name || got[0].Driver != "bridge" || got[0].Internal != n.Internal || !matchLabels(got[0].Labels, n.Labels) {
			return fail("network", i)
		}
	}
	for i, s := range a.Services {
		planned, ok := config.Services[s.Service]
		if !ok || planned.Image != s.Image || len(planned.Ports) != 0 || len(planned.Networks) != len(s.Networks) {
			return fail("service configuration", i)
		}
		for key := range planned.Networks {
			n, ok := config.Networks[key]
			if !ok || !n.External || !slices.Contains(s.Networks, n.Name) {
				return fail("service configuration", i)
			}
		}
		persistent := 0
		for _, m := range planned.Volumes {
			if m.Type == "tmpfs" {
				continue
			}
			persistent++
			v, ok := config.Volumes[m.Source]
			if m.Type != "volume" || !ok || !v.External || !slices.Contains(s.Mounts, MountAssertion{Volume: v.Name, Target: m.Target, ReadOnly: m.ReadOnly}) {
				return fail("service configuration", i)
			}
		}
		if persistent != len(s.Mounts) {
			return fail("service configuration", i)
		}
		b := &limitedBuffer{Limit: 1024}
		command := append(slices.Clone(args), "ps", "--all", "--quiet", s.Service)
		if e.Run(ctx, dir, command, env, b) != nil {
			return fail("service", i)
		}
		ids := strings.Fields(b.data.String())
		if len(ids) != 1 || !assertionContainerID.MatchString(ids[0]) {
			return fail("service", i)
		}
		var got []struct {
			Image string
			State struct {
				Running bool
				Health  struct{ Status string }
			}
			HostConfig      struct{ PortBindings map[string]any }
			NetworkSettings struct{ Networks map[string]any }
			Mounts          []struct {
				Type, Name, Destination string
				RW                      bool
			}
		}
		if !inspect([]string{"docker", "inspect", ids[0]}, &got) || len(got) != 1 {
			return fail("service", i)
		}
		c := got[0]
		if c.Image != s.Image || !c.State.Running || c.State.Health.Status != "healthy" || len(c.HostConfig.PortBindings) != 0 || len(c.NetworkSettings.Networks) != len(s.Networks) {
			return fail("service", i)
		}
		for _, n := range s.Networks {
			if _, ok := c.NetworkSettings.Networks[n]; !ok {
				return fail("service", i)
			}
		}
		for _, m := range s.Mounts {
			found := false
			for _, actual := range c.Mounts {
				if actual.Type == "volume" && actual.Name == m.Volume && actual.Destination == m.Target && actual.RW != m.ReadOnly {
					found = true
				}
			}
			if !found {
				return fail("service", i)
			}
		}
	}
	return nil
}
