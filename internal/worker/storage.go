package worker

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

func validateReleaseStorage(data []byte, releases string) error {
	var config struct {
		Services map[string]struct {
			Volumes []struct {
				Type     string `json:"type"`
				Source   string `json:"source"`
				ReadOnly bool   `json:"read_only"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return errors.New("could not inspect resolved Compose storage")
	}
	for _, service := range config.Services {
		for _, volume := range service.Volumes {
			if volume.Type != "bind" || volume.ReadOnly {
				continue
			}
			if !filepath.IsAbs(volume.Source) {
				return errors.New("writable bind mounts must use stable absolute paths or named volumes")
			}
			source := filepath.Clean(volume.Source)
			// Resolve existing ancestors too: Docker can create the final bind directory.
			ancestor, suffix := source, ""
			for {
				if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
					source = filepath.Join(resolved, suffix)
					break
				}
				parent := filepath.Dir(ancestor)
				if parent == ancestor {
					return errors.New("could not resolve writable bind mount")
				}
				suffix = filepath.Join(filepath.Base(ancestor), suffix)
				ancestor = parent
			}
			relative, err := filepath.Rel(releases, source)
			if err != nil {
				return errors.New("could not validate writable bind mount")
			}
			if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return errors.New("writable bind mounts inside Git releases would lose data on redeploy; use a named volume or a stable absolute host path")
			}
		}
	}
	return nil
}
