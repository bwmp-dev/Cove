package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type removalImage struct {
	id   string
	tags []string
}

func (e *Engine) removalImages(ctx context.Context, workload string, state *workloadRemovalState) ([]removalImage, error) {
	if state.result.CleanupWarning != "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(e.Root, "build-owner.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	owner := ""
	if err == nil {
		owner, _, err = decodeBuildOwner(raw)
		if err != nil {
			return nil, err
		}
	}
	products := map[string]bool{}
	if owner != "" {
		entries, err := os.ReadDir(filepath.Join(e.Root, "operations"))
		if err != nil {
			return nil, err
		}
		if len(entries) > 400000 {
			return nil, errors.New("build ownership inventory exceeds its limit")
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			j, err := e.read(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return nil, err
			}
			if j.Request.Workload == workload && j.Request.Action == "build" && j.Operation.Build != nil {
				for _, image := range j.Operation.Build.Images {
					products[image.ImageID] = true
				}
			}
		}
	}
	list := ""
	if owner != "" {
		list, err = e.output(ctx, "docker", "image", "ls", "--all", "--quiet", "--no-trunc", "--filter", "label=dev.cove.build-owner="+owner)
		if err != nil {
			return nil, err
		}
	}
	releases, err := e.output(ctx, "docker", "image", "ls", "--all", "--quiet", "--no-trunc", "--filter", "reference=cove-release/"+workload+":*")
	if err != nil {
		return nil, err
	}
	list += "\n" + releases
	if len(strings.Fields(list)) > 2000 {
		return nil, errors.New("build image inventory exceeds its limit")
	}
	seen := map[string]bool{}
	result := []removalImage{}
	project := e.composeProject(workload)
	for _, id := range strings.Fields(list) {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !imageDigest.MatchString(id) {
			return nil, errors.New("invalid build image identity")
		}
		var image struct {
			ID          string `json:"Id"`
			RepoTags    []string
			RepoDigests []string
			Config      struct{ Labels map[string]string }
		}
		if err := e.imageJSON(ctx, nil, &image, "docker", "image", "inspect", "--format", "{{json .}}", id); err != nil {
			return nil, err
		}
		if image.ID != id {
			continue
		}
		protected := state.images[id]
		for _, reference := range append(append([]string{}, image.RepoTags...), image.RepoDigests...) {
			protected = protected || state.images[reference]
		}
		if protected {
			continue
		}
		built := owner != "" && image.Config.Labels["dev.cove.build-owner"] == owner && (image.Config.Labels["com.docker.compose.project"] == project || products[id])
		generated := []string{}
		for _, tag := range image.RepoTags {
			if strings.HasPrefix(tag, "cove-release/"+workload+":") {
				generated = append(generated, tag)
				continue
			}
			service := image.Config.Labels["com.docker.compose.service"]
			if built && serviceIdentifier.MatchString(service) && (tag == project+"-"+service+":latest" || tag == project+"_"+service+":latest") {
				generated = append(generated, tag)
			}
		}
		if len(generated) > 0 {
			// Retire release pins before a shared image's last generated project tag.
			sort.Slice(generated, func(i, j int) bool {
				a, b := strings.HasPrefix(generated[i], "cove-release/"), strings.HasPrefix(generated[j], "cove-release/")
				if a != b {
					return a
				}
				return generated[i] < generated[j]
			})
			result = append(result, removalImage{id: id, tags: generated})
		}
	}
	return result, nil
}

func (e *Engine) removeGeneratedImageTags(ctx context.Context, images []removalImage) error {
	for _, image := range images {
		users, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "ancestor="+image.id)
		if err != nil {
			return err
		}
		for _, tag := range image.tags {
			// Retags outside Cove are never removed, even if they share this image ID.
			var inspected struct {
				ID          string `json:"Id"`
				RepoTags    []string
				RepoDigests []string
			}
			if err := e.imageJSON(ctx, nil, &inspected, "docker", "image", "inspect", "--format", "{{json .}}", tag); err != nil {
				return err
			}
			if inspected.ID != image.id {
				return errors.New("generated image tag changed; retry resource deletion")
			}
			// A repository's final tag can also retire its digest alias. That alias is not another name.
			otherName := len(inspected.RepoTags) > 1
			repository, _, _ := strings.Cut(tag, ":")
			for _, reference := range inspected.RepoDigests {
				named, _, digest := strings.Cut(reference, "@")
				named = strings.TrimPrefix(strings.TrimPrefix(named, "docker.io/"), "index.docker.io/")
				named = strings.TrimPrefix(named, "library/")
				otherName = otherName || digest && named != "" && named != repository
			}
			if users != "" && !otherName {
				continue
			}
			if _, err := e.output(ctx, "docker", "image", "rm", tag); err != nil {
				return err
			}
		}
	}
	return nil
}
