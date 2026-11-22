package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestBackupStartNamespaceDependencies(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []bool{false, true} {
		started := []string{}
		calls := 0
		e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
			if args[1] == "inspect" {
				_, err := io.WriteString(out, "{}")
				return err
			}
			calls++
			id := args[2]
			if args[1] != "start" || !slices.Contains([]string{"proxy", "app", "network"}, id) {
				t.Fatalf("unexpected command: %v", args)
			}
			dependency := map[string]string{"proxy": "app", "app": "network"}[id]
			if blocked || dependency != "" && !slices.Contains(started, dependency) {
				_, _ = io.WriteString(out, "namespace owner stopped\n")
				return errors.New("start failed")
			}
			started = append(started, id)
			return nil
		}
		var output strings.Builder
		err := e.startBackupContainers(context.Background(), []string{"proxy", "app", "network"}, &output)
		if blocked {
			if err == nil || calls != 3 || !strings.Contains(output.String(), "namespace owner stopped") {
				t.Fatalf("failure did not stop and report errors: %v %d %q", err, calls, output.String())
			}
		} else if err != nil || !slices.Equal(started, []string{"network", "app", "proxy"}) || output.Len() != 0 {
			t.Fatalf("dependency recovery failed: %v %v %q", err, started, output.String())
		}
	}
}

func TestBackupStartHonorsHealthDependencies(t *testing.T) {
	for _, running := range []bool{true, false} {
		e, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		started := []string{}
		checked := false
		e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
			id := args[len(args)-1]
			if args[1] == "inspect" && args[3] == "{{json .Config.Labels}}" {
				labels := map[string]string{"com.docker.compose.project": "fixture", "com.docker.compose.service": id}
				if id == "app" {
					labels["com.docker.compose.depends_on"] = "db:service_healthy:false,previously-stopped:service_started:false"
				}
				return json.NewEncoder(out).Encode(labels)
			}
			if args[1] == "inspect" {
				if id != "db" || !slices.Contains(started, "db") {
					t.Fatal("health checked before database started")
				}
				checked = true
				return json.NewEncoder(out).Encode(map[string]any{"Running": running, "Health": map[string]string{"Status": "healthy"}})
			}
			if args[1] != "start" || id != "db" && id != "app" {
				t.Fatalf("unexpected start: %v", args)
			}
			if id == "app" && !checked {
				t.Fatal("application started before dependency health check")
			}
			started = append(started, id)
			return nil
		}
		err = e.startBackupContainers(context.Background(), []string{"app", "db"}, io.Discard)
		if running {
			if err != nil || !slices.Equal(started, []string{"db", "app"}) {
				t.Fatalf("healthy recovery failed: %v %v", err, started)
			}
		} else if err == nil || !slices.Equal(started, []string{"db"}) {
			t.Fatalf("started app with failed dependency: %v %v", err, started)
		}
	}
}
