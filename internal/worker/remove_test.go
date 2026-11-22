package worker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestRemovalPreservesRunningWorkloadsAndStorage(t *testing.T) {
	for _, state := range []string{"running", "paused", "restarting", "exited"} {
		t.Run(state, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var mutations []string
			e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				command := strings.Join(args, " ")
				switch {
				case args[1] == "ps":
					_, err := fmt.Fprintf(out, `{"ID":"owned-container","State":%q}`, state)
					return err
				case strings.HasPrefix(command, "docker network ls"):
					_, err := io.WriteString(out, "owned-network")
					return err
				default:
					mutations = append(mutations, command)
					return nil
				}
			}
			err = e.RemoveWorkload(context.Background(), "app")
			if state != "exited" {
				if err == nil || len(mutations) != 0 {
					t.Fatalf("running workload changed: %v %v", err, mutations)
				}
			} else if err != nil || strings.Join(mutations, ";") != "docker rm owned-container;docker network rm owned-network" {
				t.Fatalf("unexpected removal or storage mutation: %v %v", err, mutations)
			}
		})
	}
}
