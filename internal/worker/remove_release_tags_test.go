package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeletionRetiresPulledReleaseTagsWithoutBuildOwnership(t *testing.T) {
	for _, scenario := range []string{"generated-only", "user-tagged", "shared", "shared-user-tagged", "shared-digest", "shared-own-digest", "backup"} {
		t.Run(scenario, func(t *testing.T) {
			e, dir, calls := deletionFixture(t, false, false)
			if err := os.Remove(filepath.Join(e.Root, "build-owner.json")); err != nil {
				t.Fatal(err)
			}
			image := "sha256:" + strings.Repeat("a", 64)
			tag := "cove-release/app:deployed-fixture"
			if scenario == "backup" {
				if err := os.MkdirAll(filepath.Join(dir, "backups"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := writeJSON(filepath.Join(dir, "backups", "retained.json"), map[string]any{"image": image}); err != nil {
					t.Fatal(err)
				}
			}
			original := e.Run
			e.Run = func(ctx context.Context, cwd string, args, env []string, out io.Writer) error {
				command := strings.Join(args, " ")
				if strings.HasPrefix(command, "docker image ls") {
					*calls = append(*calls, command)
					if !strings.Contains(command, "reference=cove-release/app:*") {
						t.Fatal("pulled releases depended on build-owner metadata", command)
					}
					_, err := fmt.Fprintln(out, image)
					return err
				}
				if strings.HasPrefix(command, "docker image inspect") {
					*calls = append(*calls, command)
					tags := []string{tag}
					if scenario == "user-tagged" || scenario == "shared-user-tagged" {
						tags = append(tags, "user/pulled:latest", "cove-release/other:retained")
					}
					digests := []string{}
					if scenario == "shared-digest" {
						digests = append(digests, "registry.example.test/app@"+image)
					}
					if scenario == "shared-own-digest" {
						digests = append(digests, "cove-release/app@"+image)
					}
					return json.NewEncoder(out).Encode(map[string]any{"Id": image, "RepoTags": tags, "RepoDigests": digests, "Config": map[string]any{"Labels": map[string]string{}}})
				}
				if strings.HasPrefix(scenario, "shared") && strings.HasPrefix(command, "docker ps -aq") {
					*calls = append(*calls, command)
					_, err := io.WriteString(out, "another-resource-container")
					return err
				}
				return original(ctx, cwd, args, env, out)
			}
			if _, err := e.RemoveWorkload(t.Context(), "app"); err != nil {
				t.Fatal(err)
			}
			commands := strings.Join(*calls, "\n")
			retired := strings.Contains(commands, "docker image rm "+tag)
			if retired != (scenario == "generated-only" || scenario == "user-tagged" || scenario == "shared-user-tagged" || scenario == "shared-digest") {
				t.Fatal("pulled-release retirement ignored ownership or references", scenario, commands)
			}
			for _, forbidden := range []string{"image rm --force", "image rm user/pulled:latest", "image rm cove-release/other:", "volume rm"} {
				if strings.Contains(commands, forbidden) {
					t.Fatal("foreign or protected data removed", commands)
				}
			}
		})
	}
}
