package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostSecurityDisableRemovesOnlyInactiveOwnedUnits(t *testing.T) {
	for _, scenario := range []string{"complete", "service-only", "absent", "foreign", "timer-active", "service-active", "disable-error", "reload-error"} {
		t.Run(scenario, func(t *testing.T) {
			host, root, owner := t.TempDir(), t.TempDir(), strings.Repeat("f1", 16)
			unit, directory := hostPolicyUnit(owner), filepath.Join(host, "etc", "systemd", "system")
			if err := os.MkdirAll(directory, 0755); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{".service", ".timer"} {
				if scenario == "absent" || scenario == "service-only" && suffix == ".timer" {
					continue
				}
				content := hostPolicyMarker(owner) + "[Unit]\n"
				if scenario == "foreign" && suffix == ".service" {
					content = "operator-owned unit\n"
				}
				if err := os.WriteFile(filepath.Join(directory, unit+suffix), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			disables, reloads, calls := 0, 0, 0
			run := func(_ context.Context, args ...string) (string, error) {
				calls++
				if len(args) == 5 && args[1] == "show" {
					if args[4] == unit+".timer" && scenario == "timer-active" || args[4] == unit+".service" && scenario == "service-active" {
						return "active", nil
					}
					return "inactive", nil
				}
				if len(args) == 4 && args[1] == "disable" && args[2] == "--now" && args[3] == unit+".timer" {
					disables++
					if scenario == "disable-error" {
						return "", errors.New("injected systemd failure")
					}
					return "", nil
				}
				if len(args) == 2 && args[1] == "daemon-reload" {
					reloads++
					if scenario == "reload-error" && reloads == 1 {
						return "", errors.New("injected reload failure")
					}
					return "", nil
				}
				t.Fatal("unexpected schedule command", args)
				return "", nil
			}
			err := configureHostPatchPolicy(t.Context(), host, root, owner, false, run)
			refused := scenario == "foreign" || scenario == "timer-active" || scenario == "service-active" || scenario == "disable-error"
			if refused {
				if err == nil || reloads != 0 || scenario == "foreign" && calls != 0 {
					t.Fatal("unsafe or unconfirmed schedule cleanup was accepted", err, calls, reloads)
				}
				for _, suffix := range []string{".service", ".timer"} {
					if _, statErr := os.Lstat(filepath.Join(directory, unit+suffix)); statErr != nil {
						t.Fatal("refusal removed a unit", statErr)
					}
				}
				return
			}
			if scenario == "reload-error" {
				if err == nil || !strings.Contains(err.Error(), "retry disable") {
					t.Fatal("unconfirmed cleanup reload was hidden", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{".service", ".timer"} {
				if _, statErr := os.Lstat(filepath.Join(directory, unit+suffix)); !os.IsNotExist(statErr) {
					t.Fatal("disabled schedule left a unit file", statErr)
				}
			}
			before := disables
			if err = configureHostPatchPolicy(t.Context(), host, root, owner, false, run); err != nil || disables != before || reloads != 2 {
				t.Fatal("disable/cleanup could not be retried with absent unit files", err, disables, reloads)
			}
		})
	}
}
