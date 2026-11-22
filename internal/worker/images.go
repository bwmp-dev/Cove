package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ImageCheck struct {
	CheckedAt time.Time     `json:"checked_at"`
	Images    []ImageStatus `json:"images"`
}

type ImageStatus struct {
	Service        string `json:"service"`
	Reference      string `json:"reference"`
	ImageID        string `json:"image_id"`
	RegistryDigest string `json:"registry_digest,omitempty"`
	Status         string `json:"status"`
	Message        string `json:"message,omitempty"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
}

type imageMetadata struct {
	ID           string            `json:"id"`
	Digests      []string          `json:"digests"`
	OS           string            `json:"os"`
	Architecture string            `json:"architecture"`
	Labels       map[string]string `json:"labels"`
}

type registryManifest struct {
	Digest string `json:"digest"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string                            `json:"digest"`
		Platform struct{ OS, Architecture string } `json:"platform"`
	} `json:"manifests"`
}

func imageRepository(reference string) string {
	reference, _, _ = strings.Cut(reference, "@")
	if colon := strings.LastIndex(reference, ":"); colon > strings.LastIndex(reference, "/") {
		reference = reference[:colon]
	}
	parts := strings.Split(reference, "/")
	if len(parts) == 1 {
		return "docker.io/library/" + reference
	}
	parts[0] = strings.ToLower(parts[0])
	if parts[0] == "index.docker.io" {
		parts[0] = "docker.io"
	}
	if !strings.ContainsAny(parts[0], ".:") && parts[0] != "localhost" {
		return "docker.io/" + strings.Join(parts, "/")
	}
	if parts[0] == "docker.io" && len(parts) == 2 {
		return "docker.io/library/" + parts[1]
	}
	return strings.Join(parts, "/")
}

var releaseVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)

func imageReleaseNotes(labels map[string]string) string {
	source, err := url.Parse(labels["org.opencontainers.image.source"])
	version := labels["org.opencontainers.image.version"]
	if err != nil || source.Scheme != "https" || source.Host != "github.com" || source.User != nil || source.RawQuery != "" || source.Fragment != "" || len(source.String()) > 2048 || len(version) > 128 || !releaseVersion.MatchString(version) {
		return ""
	}
	path := strings.TrimSuffix(strings.Trim(source.Path, "/"), ".git")
	if len(strings.Split(path, "/")) != 2 || strings.ContainsAny(path, "\\\x00\r\n") {
		return ""
	}
	return "https://github.com/" + path + "/releases/tag/" + url.PathEscape(version)
}

func (e *Engine) imageJSON(ctx context.Context, env []string, target any, args ...string) error {
	out := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, e.Root, args, env, out); err != nil {
		return err
	}
	return json.Unmarshal(out.data.Bytes(), target)
}

func (e *Engine) checkImage(ctx context.Context, status ImageStatus, env []string) ImageStatus {
	unknown := func(message string) ImageStatus { status.Status = "unknown"; status.Message = message; return status }
	if !imageDigest.MatchString(status.ImageID) || status.Reference == "" || len(status.Reference) > 512 || strings.HasPrefix(status.Reference, "-") || strings.ContainsAny(status.Reference, " \t\r\n\x00") {
		return unknown("The deployed image reference is unavailable.")
	}
	var local imageMetadata
	if err := e.imageJSON(ctx, env, &local, "docker", "image", "inspect", "--format", `{"id":{{json .Id}},"digests":{{json (index . "RepoDigests")}},"os":{{json .Os}},"architecture":{{json .Architecture}},"labels":{{json (index .Config "Labels")}}}`, status.ImageID); err != nil {
		return unknown("The local image metadata could not be read.")
	}
	status.ReleaseNotes = imageReleaseNotes(local.Labels)
	if imageDigest.MatchString(status.Reference) || strings.Contains(status.Reference, "@sha256:") {
		status.Status = "pinned"
		return status
	}
	repository := imageRepository(status.Reference)
	known := map[string]bool{}
	for _, ref := range local.Digests {
		repo, digest, ok := strings.Cut(ref, "@")
		if ok && imageRepository(repo) == repository && imageDigest.MatchString(digest) {
			known[digest] = true
		}
	}
	if len(known) == 0 {
		status.Status = "unpublished"
		status.Message = "No registry digest is recorded for this image reference."
		return status
	}
	var remote registryManifest
	if err := e.imageJSON(ctx, env, &remote, "docker", "buildx", "imagetools", "inspect", "--format", "{{json .Manifest}}", status.Reference); err != nil {
		return unknown("Registry check failed; verify connectivity, registry credentials and Buildx availability.")
	}
	if !imageDigest.MatchString(remote.Digest) {
		return unknown("The registry did not return a valid image digest.")
	}
	status.RegistryDigest = remote.Digest
	if known[remote.Digest] {
		status.Status = "current"
		return status
	}
	manifestRef := repository + "@" + remote.Digest
	if len(remote.Manifests) > 0 {
		matches := 0
		for _, manifest := range remote.Manifests {
			if manifest.Platform.OS == local.OS && manifest.Platform.Architecture == local.Architecture {
				manifestRef = repository + "@" + manifest.Digest
				matches++
			}
		}
		if matches != 1 {
			return unknown("The registry does not identify one matching image platform.")
		}
		_, digest, _ := strings.Cut(manifestRef, "@")
		if !imageDigest.MatchString(digest) {
			return unknown("The registry returned an invalid platform digest.")
		}
	}
	var platform registryManifest
	if err := e.imageJSON(ctx, env, &platform, "docker", "buildx", "imagetools", "inspect", "--raw", manifestRef); err != nil || !imageDigest.MatchString(platform.Config.Digest) {
		return unknown("The platform image manifest could not be verified.")
	}
	status.Status = "update_available"
	_, platformDigest, _ := strings.Cut(manifestRef, "@")
	if platform.Config.Digest == status.ImageID || platformDigest == status.ImageID {
		status.Status = "current"
	}
	return status
}

func (e *Engine) performImageCheck(ctx context.Context, r Request, env []string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil {
		return err
	}
	if len(containers) == 0 || len(containers) > 64 {
		return errors.New("image checks require between one and 64 deployed service containers")
	}
	result := ImageCheck{Images: []ImageStatus{}}
	seen := map[string]bool{}
	for _, container := range containers {
		var deployed struct{ Reference, Image, Service string }
		if err = e.imageJSON(ctx, env, &deployed, "docker", "inspect", "--format", `{"reference":{{json (or (index .Config.Labels "dev.cove.image-reference") .Config.Image)}},"image":{{json .Image}},"service":{{json (or (index .Config.Labels "dev.cove.service") (index .Config.Labels "com.docker.compose.service"))}}}`, container.ID); err != nil {
			return err
		}
		key := deployed.Service + "/" + deployed.Image + "/" + deployed.Reference
		if seen[key] {
			continue
		}
		seen[key] = true
		itemCtx, done := context.WithTimeout(ctx, 30*time.Second)
		status := e.checkImage(itemCtx, ImageStatus{Service: deployed.Service, Reference: deployed.Reference, ImageID: deployed.Image}, env)
		done()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		result.Images = append(result.Images, status)
		fmt.Fprintf(out, "Image check for service %s: %s\n", status.Service, status.Status)
	}
	sort.Slice(result.Images, func(i, j int) bool {
		return result.Images[i].Service+result.Images[i].ImageID < result.Images[j].Service+result.Images[j].ImageID
	})
	result.CheckedAt = time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(r.ID)
	if err != nil {
		return err
	}
	j.Operation.ImageCheck = &result
	return writeJSON(e.path(r.ID), j)
}
