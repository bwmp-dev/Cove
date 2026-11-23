package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type observationEvent struct {
	container, action string
	timestamp         time.Time
	manual            bool
}

func classifyObservationEvents(events []observationEvent, restarts map[string]int) ([]ContainerEvent, map[string]bool) {
	lastDie := map[string]int{}
	lastStart := map[string]int{}
	killed := map[string]time.Time{}
	for i, event := range events {
		switch event.action {
		case "kill":
			killed[event.container] = event.timestamp
		case "die":
			if stamp, ok := killed[event.container]; ok {
				events[i].manual = !event.timestamp.Before(stamp) && event.timestamp.Sub(stamp) <= 2*time.Minute
			}
			delete(killed, event.container)
			lastDie[event.container] = i
		case "start":
			lastStart[event.container] = i
		case "stop", "restart":
			if index, ok := lastDie[event.container]; ok {
				events[index].manual = true
			}
			if event.action == "restart" {
				if index, ok := lastStart[event.container]; ok {
					events[index].manual = true
				}
			}
		}
	}
	out := []ContainerEvent{}
	manualExit := map[string]bool{}
	pending := map[string]bool{}
	for _, event := range events {
		automatic := false
		switch event.action {
		case "die":
			manualExit[event.container] = event.manual
			pending[event.container] = !event.manual
		case "start":
			delete(manualExit, event.container)
			if pending[event.container] && !event.manual && restarts[event.container] > 0 {
				automatic = true
				restarts[event.container]--
			}
			pending[event.container] = false
		case "oom":
		default:
			continue
		}
		if event.manual {
			continue
		}
		out = append(out, ContainerEvent{Action: event.action, Timestamp: event.timestamp, AutomaticRestart: automatic})
	}
	return out, manualExit
}

type manualExitState struct {
	SampledAt  time.Time         `json:"sampled_at"`
	Exits      map[string]string `json:"exits"`
	Unexpected map[string]string `json:"unexpected"`
}

func (e *Engine) rememberManualExits(workload string, sample *WorkloadObservations, finished map[string]string, manual map[string]bool) error {
	e.observationMu.Lock()
	defer e.observationMu.Unlock()
	path := filepath.Join(e.Root, "workloads", workload, "observed-manual-exits.json")
	var previous manualExitState
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && (len(data) > 16384 || json.Unmarshal(data, &previous) != nil || len(previous.Exits)+len(previous.Unexpected) > 32) {
		return errors.New("invalid intentional-exit observations")
	}
	next := manualExitState{SampledAt: sample.SampledAt, Exits: map[string]string{}, Unexpected: map[string]string{}}
	for i := range sample.Containers {
		c := &sample.Containers[i]
		stamp := finished[c.ID]
		_, parseErr := time.Parse(time.RFC3339Nano, stamp)
		if !c.Exited || stamp == "" || parseErr != nil {
			continue
		}
		c.ManualStop = manual[c.ID] || previous.Exits[c.ID] == stamp
		_, observed := manual[c.ID]
		c.ExitUnknown = !observed && !c.ManualStop && previous.Unexpected[c.ID] != stamp
		if c.ManualStop {
			next.Exits[c.ID] = stamp
		} else if !c.ExitUnknown {
			next.Unexpected[c.ID] = stamp
		}
	}
	if previous.SampledAt.After(sample.SampledAt) || len(next.Exits)+len(next.Unexpected) == 0 && os.IsNotExist(err) {
		return nil
	}
	return writeJSON(path, next)
}
