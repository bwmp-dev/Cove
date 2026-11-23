package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestUnhealthyRestartLeavesHealthyStartingAndStoppedContainersAlone(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	states := []struct{ state, health string }{{"running", "unhealthy"}, {"running", "healthy"}, {"running", "starting"}, {"exited", "unhealthy"}, {"running", ""}}
	ids := []string{}
	for i := range states {
		ids = append(ids, strings.Repeat(fmt.Sprint(i+1), 64))
	}
	restarted := []string{}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		switch args[1] {
		case "ps":
			for i, v := range states {
				if err := json.NewEncoder(out).Encode(map[string]string{"ID": ids[i], "Names": "app", "State": v.state}); err != nil {
					return err
				}
			}
		case "inspect":
			for i, v := range states {
				if err := json.NewEncoder(out).Encode(map[string]any{"id": ids[i], "service": "app", "state": map[string]any{"Status": v.state, "Health": map[string]string{"Status": v.health}}}); err != nil {
					return err
				}
			}
		case "restart":
			restarted = append(restarted, args[4:]...)
		default:
			t.Fatal("unexpected command", args)
		}
		return nil
	}
	if err := e.restartUnhealthy(context.Background(), "app", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(restarted) != 1 || restarted[0] != ids[0] {
		t.Fatal("restarted a container without unhealthy running state", restarted)
	}
	t.Run("bound does not partially restart a resource", func(t *testing.T) {
		states = nil
		ids = nil
		restarted = nil
		for i := 0; i < 65; i++ {
			states = append(states, struct{ state, health string }{"running", "unhealthy"})
			ids = append(ids, fmt.Sprintf("%064x", i+1))
		}
		if err := e.restartUnhealthy(context.Background(), "app", io.Discard); err == nil || len(restarted) != 0 {
			t.Fatal("oversized restart changed containers", restarted, err)
		}
	})
}
