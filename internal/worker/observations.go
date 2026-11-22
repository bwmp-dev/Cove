package worker

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"strings"
	"time"
)

type ContainerObservation struct {
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
	if len(containers) > 32 {
		return nil, errors.New("observations support at most 32 containers")
	}
	now := time.Now().UTC()
	result := &WorkloadObservations{SampledAt: now, Containers: []ContainerObservation{}, Certificates: []CertificateObservation{}}
	args := []string{"docker", "events", "--since", now.Add(-10 * time.Minute).Format(time.RFC3339Nano), "--until", now.Format(time.RFC3339Nano), "--format", "{{json .}}", "--filter", "type=container"}
	allowed := map[string]bool{}
	remainingRestarts := map[string]int{}
	pendingExit := map[string]bool{}
	for _, container := range containers {
		output, err := e.output(ctx, "docker", "inspect", "--format", `{"restart_count":{{.RestartCount}},"oom_killed":{{.State.OOMKilled}},"exit_code":{{.State.ExitCode}},"exited":{{eq .State.Status "exited"}}}`, container.ID)
		if err != nil {
			return nil, errors.New("container state unavailable")
		}
		var c ContainerObservation
		if json.Unmarshal([]byte(output), &c) != nil {
			return nil, errors.New("invalid container state")
		}
		c.ID = container.ID
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
				automaticRestart := false
				switch event.Action {
				case "start":
					if pendingExit[containerID] && remainingRestarts[containerID] > 0 {
						automaticRestart = true
						result.RestartEvents++
						remainingRestarts[containerID]--
					}
					pendingExit[containerID] = false
				case "oom":
					result.OOMEvents++
				case "die":
					result.CrashEvents++
					pendingExit[containerID] = true
				default:
					continue
				}
				result.Events = append(result.Events, ContainerEvent{Action: event.Action, AutomaticRestart: automaticRestart, Timestamp: time.Unix(0, event.TimeNano).UTC()})
			}
		}
	} else {
		result.EventsAvailable = true
	}
	owned, err := e.loadIngress()
	if err != nil {
		return nil, errors.New("route observations unavailable")
	}
	for _, route := range owned[id] {
		result.Certificates = append(result.Certificates, CertificateObservation{Domain: route.Domain})
	}
	if len(result.Certificates) > 0 {
		for _, file := range []string{"/data/traefik-acme.json", "/data/cloudflare-acme.json"} {
			out := &limitedBuffer{Limit: 4 * 1024 * 1024}
			if e.Run(ctx, e.Root, []string{"docker", "exec", "cove-ingress", "cat", file}, nil, out) != nil {
				continue
			}
			applyCertificateExpiries(out.data.Bytes(), result.Certificates)
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
				if cert.VerifyHostname(routes[i].Domain) == nil && (routes[i].ExpiresAt == nil || cert.NotAfter.After(*routes[i].ExpiresAt)) {
					stamp := cert.NotAfter.UTC()
					routes[i].ExpiresAt = &stamp
				}
			}
		}
	}
}
