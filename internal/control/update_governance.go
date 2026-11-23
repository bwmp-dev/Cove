package control

import (
	"cove/internal/buildinfo"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type panelTarget struct {
	Root       string    `json:"source_root"`
	Generation int64     `json:"source_generation"`
	Version    string    `json:"version"`
	Revision   string    `json:"revision"`
	Schema     string    `json:"schema_hash"`
	Time       time.Time `json:"revision_time"`
	Checked    time.Time `json:"checked_at"`
	Available  bool      `json:"available"`
	Downgrade  bool      `json:"downgrade"`
	Allowed    bool      `json:"allowed"`
	Message    string    `json:"message"`
}

func schemaFingerprint() string {
	data, _ := assets.ReadFile("schema.sql")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ReplaceAll(string(data), "\r\n", "\n"))))
}

func stableVersion(value string) ([3]uint64, bool) {
	var result [3]uint64
	if !strings.HasPrefix(value, "v") {
		return result, false
	}
	parts := strings.Split(value[1:], ".")
	if len(parts) != 3 {
		return result, false
	}
	for i, p := range parts {
		v, e := strconv.ParseUint(p, 10, 64)
		if e != nil {
			return result, false
		}
		result[i] = v
	}
	return result, true
}

func panelDowngrade(current string, target panelTarget) (bool, bool) {
	if current == target.Version {
		return false, true
	}
	a, aa := stableVersion(current)
	b, bb := stableVersion(target.Version)
	if aa && bb {
		for i := range a {
			if a[i] != b[i] {
				return b[i] < a[i], true
			}
		}
		return false, true
	}
	if buildinfo.Revision != "" && buildinfo.Revision != "unknown" && buildinfo.Revision == target.Revision {
		return false, true
	}
	generation, err := strconv.ParseInt(buildinfo.SourceGeneration, 10, 64)
	if err != nil || generation < 1 || target.Generation < 1 || buildinfo.SourceRoot == "" || target.Root != buildinfo.SourceRoot || generation == target.Generation {
		return false, false
	}
	return target.Generation < generation, true
}

func assessTarget(target *panelTarget, now time.Time) {
	if target.Version == "" || now.Sub(target.Checked) > 2*time.Hour || target.Checked.After(now.Add(time.Minute)) {
		target.Message = "Run the host updater to obtain a fresh signed observation of this channel."
		return
	}
	target.Available = true
	downgrade, known := panelDowngrade(buildinfo.Version, *target)
	target.Downgrade = downgrade
	if !known {
		target.Message = "Source histories cannot be ordered safely because metadata is missing or incomparable. Use a backed-up metadata-aware forward upgrade or recovery procedure before switching channels."
		return
	}
	if downgrade && target.Schema != schemaFingerprint() {
		target.Message = "Blocked: this channel would downgrade to a different schema. Restore a compatible backup through the documented host recovery procedure instead."
		return
	}
	target.Allowed = true
	if downgrade {
		target.Message = "This channel would downgrade the panel. Its schema matches, but explicit confirmation is required; application data changes are not reversed."
	} else {
		target.Message = "The observed channel does not downgrade the installed panel."
	}
}

func (s *Server) governedUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var channel, confirmed, confirmedRevision string
	if err := s.DB.QueryRow(r.Context(), "SELECT update_channel,confirmed_panel_version,confirmed_panel_revision FROM control_settings WHERE id=1").Scan(&channel, &confirmed, &confirmedRevision); err != nil {
		http.Error(w, "update settings unavailable", 503)
		return
	}
	targets := map[string]panelTarget{"beta": {}, "release": {}}
	rows, err := s.DB.Query(r.Context(), "SELECT channel,version,revision,schema_hash,revision_time,checked_at,source_root,source_generation FROM panel_channel_observations")
	if err != nil {
		http.Error(w, "channel observations unavailable", 503)
		return
	}
	for rows.Next() {
		var name string
		var t panelTarget
		if err = rows.Scan(&name, &t.Version, &t.Revision, &t.Schema, &t.Time, &t.Checked, &t.Root, &t.Generation); err != nil {
			rows.Close()
			http.Error(w, "channel observations unavailable", 503)
			return
		}
		targets[name] = t
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		http.Error(w, "channel observations unavailable", 503)
		return
	}
	for name, target := range targets {
		assessTarget(&target, time.Now())
		targets[name] = target
	}
	if r.Method == "PUT" {
		var in struct {
			Channel  string `json:"channel"`
			Confirm  bool   `json:"confirm_downgrade"`
			Revision string `json:"target_revision"`
		}
		if !decode(w, r, &in) {
			return
		}
		target, ok := targets[in.Channel]
		if !ok {
			http.Error(w, "choose beta or release", 400)
			return
		}
		if in.Channel != channel {
			if !target.Available || !target.Allowed {
				http.Error(w, target.Message, 409)
				return
			}
			if in.Revision != "" && in.Revision != target.Revision {
				http.Error(w, "channel observation changed; review again", 409)
				return
			}
			if target.Downgrade && (!in.Confirm || in.Revision != target.Revision) {
				http.Error(w, "confirm this downgrade using the reviewed target revision", 409)
				return
			}
			confirmed, confirmedRevision = "", ""
			if target.Downgrade {
				confirmed, confirmedRevision = target.Version, target.Revision
			}
			if _, err = s.DB.Exec(r.Context(), "UPDATE control_settings SET update_channel=$1,confirmed_panel_version=$2,confirmed_panel_revision=$3 WHERE id=1", in.Channel, confirmed, confirmedRevision); err != nil {
				http.Error(w, "could not save update channel", 503)
				return
			}
			channel = in.Channel
		}
	}
	respond(w, 200, map[string]any{"channel": channel, "version": buildinfo.Version, "revision": buildinfo.Revision, "revision_time": buildinfo.RevisionTime, "build_channel": buildinfo.Channel, "schema_hash": schemaFingerprint(), "targets": targets, "confirmed_downgrade_version": confirmed, "confirmed_downgrade_revision": confirmedRevision})
}
