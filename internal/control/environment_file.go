package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"cove/internal/worker"
)

func parseEnvironmentFile(text string) (map[string]string, error) {
	if len(text) > 1024*1024 || strings.ContainsRune(text, 0) {
		return nil, errors.New("environment file must be at most 1 MiB without NUL bytes")
	}
	text = strings.TrimPrefix(strings.ReplaceAll(text, "\r\n", "\n"), "\ufeff")
	lines := strings.Split(text, "\n")
	values := map[string]string{}
	for line := 0; line < len(lines); line++ {
		input := strings.TrimSpace(lines[line])
		if input == "" || strings.HasPrefix(input, "#") {
			continue
		}
		if strings.HasPrefix(input, "export ") || strings.HasPrefix(input, "export\t") {
			input = strings.TrimSpace(input[len("export"):])
		}
		name, raw, ok := strings.Cut(input, "=")
		name = strings.TrimSpace(name)
		if !ok || !variableName.MatchString(name) || len(name) > 128 {
			return nil, fmt.Errorf("invalid variable assignment on line %d", line+1)
		}
		raw = strings.TrimLeft(raw, " \t")
		value := ""
		if len(raw) > 0 && (raw[0] == '\'' || raw[0] == '"') {
			quote := raw[0]

			var body strings.Builder
			segment := raw[1:]
			for {
				end := -1
				escaped := false
				for i := 0; i < len(segment); i++ {
					if quote == '"' && segment[i] == '\\' && !escaped {
						escaped = true
						continue
					}
					if segment[i] == quote && !escaped {
						end = i
						break
					}
					escaped = false
				}
				if end >= 0 {
					body.WriteString(segment[:end])
					tail := strings.TrimSpace(segment[end+1:])
					if tail != "" && !strings.HasPrefix(tail, "#") {
						return nil, fmt.Errorf("unexpected text after quoted value on line %d", line+1)
					}
					break
				}
				body.WriteString(segment)
				body.WriteByte('\n')
				line++
				if line >= len(lines) {
					return nil, errors.New("unterminated quoted environment value")
				}
				segment = lines[line]
			}
			value = body.String()
			if quote == '"' {
				var normalized strings.Builder
				normalized.WriteByte('"')
				for i := 0; i < len(value); i++ {
					b := value[i]
					if b == '\\' && i+1 < len(value) {
						i++
						if value[i] == '$' {
							normalized.WriteByte('$')
						} else {
							normalized.WriteByte('\\')
							normalized.WriteByte(value[i])
						}
					} else if b < 32 {
						fmt.Fprintf(&normalized, "\\u%04x", b)
					} else {
						normalized.WriteByte(b)
					}
				}
				normalized.WriteByte('"')
				if json.Unmarshal([]byte(normalized.String()), &value) != nil {
					return nil, fmt.Errorf("invalid quoted escape on line %d", line+1)
				}
			}
		} else {
			for i, b := range raw {
				if b == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
					raw = raw[:i]
					break
				}
			}
			value = strings.TrimRight(raw, " \t")
		}
		values[name] = value
		if len(values) > 256 {
			return nil, errors.New("at most 256 variables per import")
		}
	}
	if len(values) == 0 {
		return nil, errors.New("environment file contains no variables")
	}
	if err := worker.ValidateRequest(worker.Request{ID: "validation", Workload: "validation", Action: "deploy", Compose: "services: {}", Environment: values}); err != nil {
		return nil, errors.New("environment file contains a reserved name, invalid value or a value exceeding 64 KiB")
	}
	return values, nil
}

func encodeEnvironmentFile(values map[string]string) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var output strings.Builder
	for _, name := range names {
		value, _ := json.Marshal(values[name])
		output.WriteString(name)
		output.WriteByte('=')
		output.Write(value)
		output.WriteByte('\n')
	}
	return output.String()
}

func (s *Server) environmentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAccess(w, r, "workload", id, 3) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	importing := strings.HasSuffix(r.URL.Path, "/import")
	var values map[string]string
	if importing {
		if !s.requirePersistentResource(w, r, id) {
			return
		}
		var in struct {
			Dotenv string `json:"dotenv"`
		}
		if !decode(w, r, &in) {
			return
		}
		var err error
		values, err = parseEnvironmentFile(in.Dotenv)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	if err = tx.QueryRow(r.Context(), `SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&encrypted); err != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	if importing {
		var active bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
			http.Error(w, "variables unavailable", 503)
			return
		}
		if active {
			http.Error(w, "wait for the active operation before editing", 409)
			return
		}
	}
	var config workloadInput
	if err = s.unsealJSON(encrypted, &config); err != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	if !importing {
		if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'workload variables exported',$2,200,$3)`, "user "+actor(r).ID, id, actor(r).Team); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not audit variable export", 503)
			return
		}
		respond(w, 200, map[string]string{"dotenv": encodeEnvironmentFile(config.Environment)})
		return
	}
	if config.Environment == nil {
		config.Environment = map[string]string{}
	}
	for name, value := range values {
		config.Environment[name] = value
	}
	if err = validateWorkload(config); err != nil {
		http.Error(w, "import would create invalid workload variables", 400)
		return
	}
	saved, err := s.sealPreview(config)
	if err != nil {
		http.Error(w, "could not protect variables", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE workloads SET configuration=$2 WHERE id=$1`, id, saved); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not import variables", 503)
		return
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	respond(w, 200, map[string]any{"keys": names})
}
