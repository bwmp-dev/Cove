package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

func configurationChanges(before, after map[string]json.RawMessage) []string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	changed := []string{}
	for key := range keys {
		if bytes.Equal(before[key], after[key]) {
			continue
		}
		if key == "environment" {
			var a, b map[string]json.RawMessage
			if json.Unmarshal(before[key], &a) == nil && json.Unmarshal(after[key], &b) == nil {
				for _, name := range configurationChanges(a, b) {
					changed = append(changed, "environment."+name)
				}
				continue
			}
		}
		changed = append(changed, key)
	}
	sort.Strings(changed)
	return changed
}

type configurationDiff struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func safeConfigurationDiffs(before, after map[string]json.RawMessage) []configurationDiff {
	diffs := []configurationDiff{}
	for _, key := range []string{"name", "kind", "job_service", "routes", "limits"} {
		if !bytes.Equal(before[key], after[key]) {
			diffs = append(diffs, configurationDiff{key, string(before[key]), string(after[key])})
		}
	}
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(before["source"], &a)
	_ = json.Unmarshal(after["source"], &b)
	for _, key := range []string{"repository", "branch", "compose_path", "watch_paths"} {
		if !bytes.Equal(a[key], b[key]) {
			diffs = append(diffs, configurationDiff{"source." + key, string(a[key]), string(b[key])})
		}
	}
	return diffs
}

func (s *Server) configurationHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT id,configuration,created_at FROM configuration_history WHERE workload_id=$1 ORDER BY id ASC`, r.PathValue("id"))
	if err != nil {
		http.Error(w, "history unavailable", 503)
		return
	}
	defer rows.Close()
	type revision struct {
		Diffs     []configurationDiff `json:"diffs"`
		ID        string              `json:"id"`
		CreatedAt time.Time           `json:"created_at"`
		Changes   []string            `json:"changes"`
		Baseline  bool                `json:"baseline"`
	}
	items := []revision{}
	var previous map[string]json.RawMessage
	for rows.Next() {
		var id int64
		var encrypted []byte
		var item revision
		if err = rows.Scan(&id, &encrypted, &item.CreatedAt); err != nil {
			http.Error(w, "history unavailable", 503)
			return
		}
		raw, e := unseal(s.Key, encrypted)
		var current map[string]json.RawMessage
		if e != nil || json.Unmarshal(raw, &current) != nil {
			http.Error(w, "history unavailable", 503)
			return
		}
		item.Diffs = safeConfigurationDiffs(previous, current)
		item.ID = strconv.FormatInt(id, 10)
		item.Baseline = previous == nil
		item.Changes = configurationChanges(previous, current)
		items = append(items, item)
		previous = current
	}
	if rows.Err() != nil {
		http.Error(w, "history unavailable", 503)
		return
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	respond(w, 200, items)
}
