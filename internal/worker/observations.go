package worker

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"slices"
	"strings"
	"time"
)

type ContainerObservation struct {
	ExitUnknown       bool   `json:"exit_unknown,omitempty"`
	ManualStop        bool   `json:"manual_stop,omitempty"`
	ID                string `json:"id"`
	RestartCount      int    `json:"restart_count"`
	OOMKilled         bool   `json:"oom_killed"`
	ExitCode          int    `json:"exit_code"`
	Exited            bool   `json:"exited"`
	ExpectedCompleted bool   `json:"expected_completed"`
}
type CertificateObservation struct {
	Domain    string     `json:"domain"`
	ExpiresAt *time.Time `json:"expires_at"`
}
type WorkloadObservations struct {
	Events          []ContainerEvent         `json:"events"`
	SampledAt       time.Time                `json:"sampled_at"`
	Containers      []ContainerObservation   `json:"containers"`
	Certificates    []CertificateObservation `json:"certificates"`
	EventsAvailable bool                     `json:"events_available"`
	RestartEvents   int                      `json:"restart_events"`
	OOMEvents       int                      `json:"oom_events"`
	CrashEvents     int                      `json:"crash_events"`
}

type ContainerEvent struct {
	AutomaticRestart bool      `json:"automatic_restart"`
	Action           string    `json:"action"`
	Timestamp        time.Time `json:"timestamp"`
}

func (e *Engine) ObserveWorkload(ctx context.Context, id string) (*WorkloadObservations, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	containers, err := e.HealthContainers(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(containers) > MaxResourceContainers {
		return nil, errors.New("observations support at most 128 containers")
	}
	now := time.Now().UTC()
	result := &WorkloadObservations{SampledAt: now, Containers: []ContainerObservation{}, Certificates: []CertificateObservation{}}
	args := []string{"docker", "events", "--since", now.Add(-10 * time.Minute).Format(time.RFC3339Nano), "--until", now.Format(time.RFC3339Nano), "--format", "{{json .}}", "--filter", "type=container"}
	allowed := map[string]bool{}
	remainingRestarts := map[string]int{}
	finished := map[string]string{}
	var events []observationEvent
	for _, container := range containers {
		output, err := e.output(ctx, "docker", "inspect", "--format", `{"restart_count":{{.RestartCount}},"oom_killed":{{.State.OOMKilled}},"exit_code":{{.State.ExitCode}},"exited":{{eq .State.Status "exited"}},"finished_at":{{json .State.FinishedAt}}}`, container.ID)
		if err != nil {
			return nil, errors.New("container state unavailable")
		}
		var inspected struct {
			ContainerObservation
			FinishedAt string `json:"finished_at"`
		}
		if json.Unmarshal([]byte(output), &inspected) != nil {
			return nil, errors.New("invalid container state")
		}
		c := inspected.ContainerObservation
		c.ID = container.ID
		finished[c.ID] = inspected.FinishedAt
		c.ExpectedCompleted = container.ExpectedCompleted
		result.Containers = append(result.Containers, c)
		if !c.ExpectedCompleted {
			allowed[c.ID] = true
			remainingRestarts[c.ID] = c.RestartCount
			args = append(args, "--filter", "container="+c.ID)
		}
	}
	if len(allowed) > 0 {
		out := &limitedBuffer{Limit: 512 * 1024}
		if e.Run(ctx, e.Root, args, nil, out) == nil {
			result.EventsAvailable = true
			decoder := json.NewDecoder(&out.data)
			for count := 0; ; count++ {
				var event struct {
					Action   string
					TimeNano int64 `json:"timeNano"`
					Actor    struct{ ID string }
				}
				err := decoder.Decode(&event)
				if err == io.EOF {
					break
				}
				if err != nil || count >= 4096 {
					result.EventsAvailable = false
					break
				}
				owned := false
				containerID := ""
				for short := range allowed {
					if strings.HasPrefix(event.Actor.ID, short) {
						owned = true
						containerID = short
						break
					}
				}
				if !owned {
					continue
				}
				events = append(events, observationEvent{container: containerID, action: event.Action, timestamp: time.Unix(0, event.TimeNano).UTC()})
			}
		}
	} else {
		result.EventsAvailable = true
	}
	var manual map[string]bool
	result.Events, manual = classifyObservationEvents(events, remainingRestarts)
	for _, event := range result.Events {
		switch event.Action {
		case "start":
			if event.AutomaticRestart {
				result.RestartEvents++
			}
		case "oom":
			result.OOMEvents++
		case "die":
			result.CrashEvents++
		}
	}
	if err = e.rememberManualExits(id, result, finished, manual); err != nil {
		return nil, err
	}
	owned, err := e.loadIngress()
	if err != nil {
		return nil, errors.New("route observations unavailable")
	}
	var acme []CertificateObservation
	var acmeIndexes []int
	for _, route := range owned[id] {
		observation := CertificateObservation{Domain: route.Domain}
		if route.Policy != nil && route.Policy.CertificateID != "" {
			if material, err := e.readRouteCertificate(id, route.Policy.CertificateID); err == nil {
				if block, _ := pem.Decode([]byte(material.CertificatePEM)); block != nil {
					if certificate, err := x509.ParseCertificate(block.Bytes); err == nil {
						stamp := certificate.NotAfter.UTC()
						observation.ExpiresAt = &stamp
					}
				}
			}
		} else if route.Policy == nil || route.Policy.CertificateSource != "default" {
			acmeIndexes = append(acmeIndexes, len(result.Certificates))
			acme = append(acme, observation)
		}
		result.Certificates = append(result.Certificates, observation)
	}
	if len(result.Certificates) > 0 {
		label, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "cove.managed"}}`, "cove-ingress")
		if err != nil || label != "ingress" {
			return result, nil
		}
		for _, file := range []string{"/data/traefik-acme.json", "/data/cloudflare-acme.json"} {
			out := &limitedBuffer{Limit: 4 * 1024 * 1024}
			if e.Run(ctx, e.Root, []string{"docker", "exec", "cove-ingress", "cat", file}, nil, out) != nil {
				continue
			}
			applyCertificateExpiries(out.data.Bytes(), acme)
		}
		for i, index := range acmeIndexes {
			result.Certificates[index] = acme[i]
		}
	}
	return result, nil
}

func applyCertificateExpiries(data []byte, routes []CertificateObservation) {
	var resolvers map[string]struct {
		Certificates []struct {
			Certificate string `json:"certificate"`
		}
	}
	if json.Unmarshal(data, &resolvers) != nil {
		return
	}
	for _, resolver := range resolvers {
		for _, item := range resolver.Certificates {
			decoded, err := base64.StdEncoding.DecodeString(item.Certificate)
			if err != nil {
				continue
			}
			block, _ := pem.Decode(decoded)
			if block == nil || block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			for i := range routes {
				covered := cert.VerifyHostname(routes[i].Domain) == nil
				if strings.HasPrefix(routes[i].Domain, "*.") {
					covered = slices.Contains(cert.DNSNames, routes[i].Domain)
				}
				if covered && (routes[i].ExpiresAt == nil || cert.NotAfter.After(*routes[i].ExpiresAt)) {
					stamp := cert.NotAfter.UTC()
					routes[i].ExpiresAt = &stamp
				}
			}
		}
	}
}
