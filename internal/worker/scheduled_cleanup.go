package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const housekeepingWorkload = "__housekeeping__"

type CleanupPolicy struct {
	Enabled         bool `json:"enabled"`
	IntervalHours   int  `json:"interval_hours"`
	MinimumAgeHours int  `json:"minimum_age_hours"`
	Images          bool `json:"images"`
	BuildCache      bool `json:"build_cache"`
	Volumes         bool `json:"volumes"`
	NonCove         bool `json:"non_cove"`
}
type CleanupItem struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Bytes *int64 `json:"bytes"`
}
type CleanupPlan struct {
	Items          []CleanupItem `json:"items"`
	EstimatedBytes int64         `json:"estimated_bytes"`
	SkippedCache   int           `json:"skipped_cache"`
	UnknownSizes   bool          `json:"unknown_sizes"`
	Revision       string        `json:"revision"`
	SampledAt      time.Time     `json:"sampled_at"`
}
type CleanupControl struct {
	Action   string         `json:"action"`
	Policy   *CleanupPolicy `json:"policy,omitempty"`
	Revision string         `json:"revision,omitempty"`
}
type CleanupState struct {
	Policy         CleanupPolicy `json:"policy"`
	LastRun        *time.Time    `json:"last_run"`
	LastError      string        `json:"last_error"`
	Removed        int           `json:"removed"`
	EstimatedBytes int64         `json:"estimated_bytes"`
}

func validCleanupPolicy(p CleanupPolicy) bool {
	return p.IntervalHours >= 1 && p.IntervalHours <= 168 && p.MinimumAgeHours >= 24 && p.MinimumAgeHours <= 8760
}
func (e *Engine) cleanupState() (CleanupState, error) {
	s := CleanupState{Policy: CleanupPolicy{Enabled: true, IntervalHours: 6, MinimumAgeHours: 168, Images: true, BuildCache: true}}
	data, err := os.ReadFile(filepath.Join(e.Root, "cleanup-policy.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if len(data) > 16384 || json.Unmarshal(data, &s) != nil || !validCleanupPolicy(s.Policy) {
		return s, errors.New("invalid cleanup policy state")
	}
	return s, nil
}

func (e *Engine) CleanupPolicy(ctx context.Context, in CleanupControl) (*CleanupState, *CleanupPlan, error) {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	s, err := e.cleanupState()
	if err != nil {
		return nil, nil, err
	}
	switch in.Action {
	case "get":
		return &s, nil, nil
	case "save":
		if in.Policy == nil || !validCleanupPolicy(*in.Policy) {
			return nil, nil, errors.New("cleanup requires interval 1-168 hours and minimum age 24-8760 hours")
		}
		s.Policy = *in.Policy
		return &s, nil, writeJSON(filepath.Join(e.Root, "cleanup-policy.json"), s)
	case "preview":
		p, err := e.cleanupPlan(ctx, s.Policy)
		return &s, p, err
	case "run":
		unlock, err := e.lockNodeChange(false)
		if err != nil {
			return &s, nil, err
		}
		defer unlock()
		if err := e.requireInterlockedHostSchedule(); err != nil {
			return &s, nil, err
		}
		e.mu.Lock()
		if e.draining || e.hasMoveFence() || e.hostChanging || e.proxyRunning || e.customRunning || e.customUnsettled || len(e.active) > 0 || len(e.pending) > 0 || len(e.recovering) > 0 {
			e.mu.Unlock()
			return nil, nil, errors.New("wait for worker operations and recovery before cleanup")
		}
		e.active[housekeepingWorkload] = true
		e.mu.Unlock()
		defer func() { e.mu.Lock(); delete(e.active, housekeepingWorkload); e.mu.Unlock() }()
		plan, err := e.cleanupPlan(ctx, s.Policy)
		if err != nil {
			return &s, nil, err
		}
		if in.Revision == "" || plan.Revision != in.Revision {
			return &s, plan, errors.New("cleanup preview changed; refresh before applying")
		}
		stamp := time.Now().UTC()
		s.LastRun = &stamp
		s.LastError = ""
		s.Removed = 0
		s.EstimatedBytes = 0
		for _, item := range plan.Items {
			var args, env []string
			switch item.Kind {
			case "image":
				args = []string{"docker", "image", "rm", item.ID}
			case "volume":
				args = []string{"docker", "volume", "rm", item.ID}
			case "cache":
				owner, ownerErr := e.buildOwner()
				if ownerErr != nil {
					return &s, plan, ownerErr
				}
				builder, builderErr := e.cleanupBuilder(ctx, owner)
				if builderErr != nil || builder == "" {
					return &s, plan, errors.Join(errors.New("the owned cleanup builder changed; refresh the preview"), builderErr)
				}
				args = []string{"docker", "buildx", "prune", "--builder", builder, "--force", "--filter", "id=" + item.ID, "--filter", fmt.Sprintf("until=%dh", s.Policy.MinimumAgeHours)}
				env = []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}
			default:
				return &s, plan, errors.New("invalid cleanup item")
			}
			if err = e.Run(ctx, e.Root, args, env, &limitedBuffer{Limit: 65536}); err != nil {
				s.LastError = "Cleanup stopped after a resource changed or a Docker command failed; inspect and preview again."
				break
			}
			s.Removed++
			if item.Bytes != nil {
				s.EstimatedBytes += *item.Bytes
			}
		}
		if saveErr := writeJSON(filepath.Join(e.Root, "cleanup-policy.json"), s); saveErr != nil {
			return &s, plan, saveErr
		}
		return &s, plan, err
	}
	return nil, nil, errors.New("invalid cleanup action")
}

func (e *Engine) cleanupPlan(ctx context.Context, p CleanupPolicy) (*CleanupPlan, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	plan := &CleanupPlan{Items: []CleanupItem{}, SampledAt: time.Now().UTC()}
	owner := ""
	if _, err := os.Stat(filepath.Join(e.Root, "build-owner.json")); err == nil {
		owner, err = e.buildOwner()
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if p.Images && (owner != "" || p.NonCove) {
		args := []string{"docker", "image", "ls", "--all", "--quiet", "--no-trunc"}
		if !p.NonCove {
			args = append(args, "--filter", "label=dev.cove.build-owner="+owner)
		}
		raw, err := e.output(ctx, args...)
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(raw)
		if len(ids) > 2000 {
			return nil, errors.New("image inventory exceeds cleanup limit")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			if !imageDigest.MatchString(id) {
				return nil, errors.New("invalid cleanup image identity")
			}
			metadata, err := e.output(ctx, "docker", "image", "inspect", "--format", `{"created":{{json .Created}},"tagged":{{json .Metadata.LastTagTime}},"size":{{.Size}},"tags":{{json .RepoTags}}}`, id)
			if err != nil {
				return nil, err
			}
			var m struct {
				Created time.Time
				Tagged  time.Time
				Size    int64
				Tags    []string
			}
			if json.Unmarshal([]byte(metadata), &m) != nil || m.Size < 0 {
				return nil, errors.New("invalid cleanup image metadata")
			}
			if m.Tagged.After(m.Created) {
				m.Created = m.Tagged
			}
			protected := false
			for _, tag := range m.Tags {
				if strings.HasPrefix(tag, "cove-release/") {
					protected = true
				}
			}
			if protected || m.Created.After(plan.SampledAt.Add(-time.Duration(p.MinimumAgeHours)*time.Hour)) {
				continue
			}
			users, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "ancestor="+id)
			if err != nil {
				return nil, err
			}
			if users != "" {
				continue
			}
			plan.Items = append(plan.Items, CleanupItem{Kind: "image", ID: id, Bytes: &m.Size})
			plan.EstimatedBytes += m.Size
		}
	}
	if p.Volumes {
		raw, err := e.output(ctx, "docker", "volume", "ls", "--quiet", "--filter", "dangling=true")
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(raw)
		if len(ids) > 500 {
			return nil, errors.New("volume inventory exceeds cleanup limit")
		}
		for _, id := range ids {
			if !fileVolumeName.MatchString(id) {
				continue
			}
			metadata, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{"created":{{json .CreatedAt}},"labels":{{json .Labels}}}`, id)
			if err != nil {
				return nil, err
			}
			var m struct {
				Created time.Time
				Labels  map[string]string
			}
			if json.Unmarshal([]byte(metadata), &m) != nil {
				return nil, errors.New("invalid cleanup volume metadata")
			}
			if m.Created.IsZero() || m.Created.After(plan.SampledAt.Add(-time.Duration(p.MinimumAgeHours)*time.Hour)) {
				continue
			}
			project := m.Labels["com.docker.compose.project"]
			if m.Labels["cove.managed"] != "" || id == "cove-ingress" || strings.HasPrefix(id, "cove-link-") || strings.HasPrefix(id, "buildx_buildkit_") {
				continue
			}
			managed := strings.HasPrefix(project, "cove-") && workloadIdentifier.MatchString(strings.TrimPrefix(project, "cove-"))
			if managed {
				_, err = os.Stat(filepath.Join(e.Root, "workloads", strings.TrimPrefix(project, "cove-")))
				if err == nil {
					continue
				}
				if !os.IsNotExist(err) {
					return nil, err
				}
			} else if !p.NonCove {
				continue
			}
			users, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "volume="+id)
			if err != nil {
				return nil, err
			}
			if users != "" {
				continue
			}
			plan.Items = append(plan.Items, CleanupItem{Kind: "volume", ID: id})
			plan.UnknownSizes = true
		}
	}
	if p.BuildCache && owner != "" {
		builder, err := e.cleanupBuilder(ctx, owner)
		if err != nil {
			return nil, err
		}
		if builder != "" {
			out := &limitedBuffer{Limit: 1024 * 1024}
			args := []string{"docker", "buildx", "du", "--builder", builder, "--format=json"}
			if err = e.Run(ctx, e.Root, args, []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}, out); err != nil {
				return nil, errors.New("Cove build cache inspection unavailable")
			}
			decoder := json.NewDecoder(bytes.NewReader(out.data.Bytes()))
			for count := 0; ; count++ {
				var m struct {
					ID, Size            string
					Reclaimable, Shared bool
					CreatedAt           string
					LastUsedAt          string
				}
				err := decoder.Decode(&m)
				if err == io.EOF {
					break
				}
				if err != nil || count >= 2000 {
					return nil, errors.New("invalid or excessive build cache report")
				}
				if !m.Reclaimable {
					continue
				}
				if len(m.ID) < 1 || len(m.ID) > 128 || strings.ContainsAny(m.ID, " =\r\n\t") {
					return nil, errors.New("invalid cache identity")
				}
				last, known := cacheLastUsed(m.CreatedAt, m.LastUsedAt, plan.SampledAt)
				if !known {
					plan.SkippedCache++
					continue
				}
				if last.After(plan.SampledAt.Add(-time.Duration(p.MinimumAgeHours) * time.Hour)) {
					continue
				}

				size, parseErr := strconv.ParseInt(m.Size, 10, 64)
				var amount *int64
				if parseErr == nil && size >= 0 {
					amount = &size
				} else if parsed, valid := metricBytes(m.Size); valid && parsed >= 0 && parsed < float64(math.MaxInt64) {
					size = int64(parsed)
					amount = &size
				}
				if amount == nil {
					plan.UnknownSizes = true
				}
				if m.Shared {
					size = 0
					amount = &size
				}
				plan.Items = append(plan.Items, CleanupItem{Kind: "cache", ID: m.ID, Bytes: amount})
				if amount != nil {
					plan.EstimatedBytes += *amount
				}

			}
		}
	}
	sort.Slice(plan.Items, func(i, j int) bool {
		if plan.Items[i].Kind == plan.Items[j].Kind {
			return plan.Items[i].ID < plan.Items[j].ID
		}
		return plan.Items[i].Kind < plan.Items[j].Kind
	})
	data, _ := json.Marshal(struct {
		Policy CleanupPolicy
		Items  []CleanupItem
	}{p, plan.Items})
	sum := sha256.Sum256(data)
	plan.Revision = hex.EncodeToString(sum[:])
	return plan, nil
}

// Buildx versions also emit display-formatted dates in JSON; unknown or approximate ages stay protected.
func cacheLastUsed(created, last string, now time.Time) (time.Time, bool) {
	parse := func(value string) (time.Time, bool) {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST"} {
			if stamp, err := time.Parse(layout, value); err == nil {
				return stamp, true
			}
		}
		return time.Time{}, false
	}
	if stamp, ok := parse(last); ok {
		return stamp, true
	}
	if last == "" || last == "Never" || last == "-" {
		return parse(created)
	}
	parts := strings.Fields(last)
	if len(parts) != 3 || parts[2] != "ago" {
		return time.Time{}, false
	}
	count, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil || count < 0 || count > 10000 {
		return time.Time{}, false
	}
	unit, ok := map[string]time.Duration{"second": time.Second, "minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour, "month": 28 * 24 * time.Hour, "year": 365 * 24 * time.Hour}[strings.TrimSuffix(parts[1], "s")]
	if !ok || count > int64(math.MaxInt64)/int64(unit) {
		return time.Time{}, false
	}
	return now.Add(-time.Duration(count) * unit), true
}
