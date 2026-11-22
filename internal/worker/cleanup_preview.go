package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type CleanupImage struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created_at"`
	Size    int64     `json:"size"`
}
type CleanupPreview struct {
	Images    []CleanupImage `json:"images"`
	Bytes     int64          `json:"image_bytes"`
	SampledAt time.Time      `json:"sampled_at"`
	Managed   bool           `json:"managed"`
}

func (e *Engine) PreviewCleanup(ctx context.Context) (CleanupPreview, error) {
	report := CleanupPreview{Images: []CleanupImage{}, SampledAt: time.Now().UTC()}
	if _, err := os.Stat(filepath.Join(e.Root, "build-owner.json")); os.IsNotExist(err) {
		return report, nil
	} else if err != nil {
		return report, err
	}
	owner, err := e.buildOwner()
	if err != nil {
		return report, err
	}
	report.Managed = true
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := e.output(ctx, "docker", "image", "ls", "--quiet", "--no-trunc", "--filter", "dangling=true", "--filter", "label=dev.cove.build-owner="+owner)
	if err != nil {
		return report, err
	}
	ids := strings.Fields(raw)
	if len(ids) > 500 {
		return report, errors.New("too many images for a bounded cleanup preview")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !strings.HasPrefix(id, "sha256:") || len(id) != 71 {
			return report, errors.New("invalid image identity")
		}
		raw, err = e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}} {{.Created}} {{.Size}}", id)
		if err != nil {
			return report, err
		}
		fields := strings.Fields(raw)
		if len(fields) != 3 || fields[0] != id {
			return report, errors.New("invalid image metadata")
		}
		created, err := time.Parse(time.RFC3339Nano, fields[1])
		if err != nil {
			return report, err
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return report, errors.New("invalid image size")
		}
		if created.After(report.SampledAt.Add(-168 * time.Hour)) {
			continue
		}
		used, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "ancestor="+id)
		if err != nil {
			return report, err
		}
		if used != "" {
			continue
		}
		report.Images = append(report.Images, CleanupImage{ID: id, Created: created, Size: size})
		report.Bytes += size
	}
	return report, nil
}
