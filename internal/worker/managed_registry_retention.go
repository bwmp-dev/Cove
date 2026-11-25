package worker

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ManagedRegistryManifest struct {
	Repository string    `json:"repository"`
	Digest     string    `json:"digest"`
	Tags       []string  `json:"tags"`
	Children   []string  `json:"children"`
	CreatedAt  time.Time `json:"created_at"`
}

type ManagedRegistryRetention struct {
	KeepPerRepository int `json:"keep_per_repository"`
	MinimumAgeDays    int `json:"minimum_age_days"`
}

const MaxManagedRegistryReferences = 4096

var managedBuildTag = regexp.MustCompile(`^cove-[a-f0-9]{32}-[a-f0-9]{16}$`)
var managedManifestDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func (p ManagedRegistryRetention) Validate() error {
	if p.KeepPerRepository < 1 || p.KeepPerRepository > 100 || p.MinimumAgeDays < 1 || p.MinimumAgeDays > 3650 {
		return errors.New("registry retention requires 1 to 100 retained manifests per repository and a minimum age from 1 to 3650 days")
	}
	return nil
}

// Protection is by manifest, not tag: a retained tag or image index keeps every
// referenced child even when another expired tag points at that same content.
func PlanManagedRegistryRetention(p ManagedRegistryRetention, inventory []ManagedRegistryManifest, protected []string, now time.Time) ([]ManagedRegistryManifest, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if len(inventory) > 8192 || len(protected) > MaxManagedRegistryReferences || now.IsZero() {
		return nil, errors.New("registry retention inventory exceeds its bound or has no observation time")
	}
	byKey := map[string]ManagedRegistryManifest{}
	byRepo := map[string][]ManagedRegistryManifest{}
	keep := map[string]bool{}
	key := func(repo, digest string) string { return repo + "@" + digest }
	for _, m := range inventory {
		if ValidateRegistryRepository(m.Repository) != nil || !managedManifestDigest.MatchString(m.Digest) || len(m.Tags) > 256 || len(m.Children) > 256 {
			return nil, errors.New("invalid registry retention inventory")
		}
		k := key(m.Repository, m.Digest)
		if _, exists := byKey[k]; exists {
			return nil, errors.New("duplicate registry manifest inventory")
		}
		byKey[k] = m
		byRepo[m.Repository] = append(byRepo[m.Repository], m)
		if len(m.Tags) == 0 || m.CreatedAt.IsZero() || m.CreatedAt.After(now) || now.Sub(m.CreatedAt) < time.Duration(p.MinimumAgeDays)*24*time.Hour {
			keep[k] = true
		}
		for _, tag := range m.Tags {
			if !managedBuildTag.MatchString(tag) {
				keep[k] = true
			}
		}
		for _, digest := range m.Children {
			if !managedManifestDigest.MatchString(digest) {
				return nil, errors.New("invalid registry manifest child")
			}
		}
	}
	for _, ref := range protected {
		repo, digest, ok := strings.Cut(ref, "@")
		if !ok || ValidateRegistryRepository(repo) != nil || !managedManifestDigest.MatchString(digest) {
			return nil, errors.New("invalid protected registry reference")
		}
		keep[ref] = true
	}
	for _, manifests := range byRepo {
		sort.Slice(manifests, func(i, j int) bool {
			if manifests[i].CreatedAt.Equal(manifests[j].CreatedAt) {
				return manifests[i].Digest < manifests[j].Digest
			}
			return manifests[i].CreatedAt.After(manifests[j].CreatedAt)
		})
		for i := 0; i < len(manifests) && i < p.KeepPerRepository; i++ {
			m := manifests[i]
			keep[key(m.Repository, m.Digest)] = true
		}
	}
	queue := make([]string, 0, len(keep))
	for k := range keep {
		queue = append(queue, k)
	}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		m, exists := byKey[k]
		if !exists {
			continue
		}
		for _, child := range m.Children {
			k := key(m.Repository, child)
			if _, exists := byKey[k]; !exists {
				return nil, errors.New("incomplete retained image index inventory; registry deletion refused")
			}
			if !keep[k] {
				keep[k] = true
				queue = append(queue, k)
			}
		}
	}
	remove := []ManagedRegistryManifest{}
	for k, m := range byKey {
		if !keep[k] {
			remove = append(remove, m)
		}
	}
	sort.Slice(remove, func(i, j int) bool {
		return key(remove[i].Repository, remove[i].Digest) < key(remove[j].Repository, remove[j].Digest)
	})
	return remove, nil
}
