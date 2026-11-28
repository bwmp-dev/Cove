package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type IngressConfigRequest struct {
	Action   string  `json:"action"`
	ChangeID string  `json:"change_id,omitempty"`
	Name     string  `json:"name"`
	Content  *string `json:"content,omitempty"`
	Revision string  `json:"revision,omitempty"`
}

type IngressConfigChange struct {
	ID         string     `json:"id,omitempty"`
	Action     string     `json:"action"`
	Name       string     `json:"name"`
	State      string     `json:"state"`
	Message    string     `json:"message,omitempty"`
	Problems   []string   `json:"problems,omitempty"`
	Warnings   []string   `json:"warnings,omitempty"`
	Revision   string     `json:"revision,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type IngressConfigResult struct {
	// Code classifies a refused request: conflict, blocked, invalid, not_found or unavailable.
	Code   string               `json:"code,omitempty"`
	Error  string               `json:"error,omitempty"`
	Change *IngressConfigChange `json:"change,omitempty"`
}

type IngressConfigObject struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Errors []string `json:"errors,omitempty"`
}

type IngressConfigFile struct {
	Name     string                `json:"name"`
	Revision string                `json:"revision,omitempty"`
	Size     int64                 `json:"size"`
	Status   string                `json:"status"`
	Modified bool                  `json:"modified,omitempty"`
	Problem  string                `json:"problem,omitempty"`
	Content  *string               `json:"content,omitempty"`
	Objects  []IngressConfigObject `json:"objects,omitempty"`
	Warnings []string              `json:"warnings,omitempty"`
}

type IngressConfigState struct {
	Available   bool                 `json:"available"`
	Unavailable string               `json:"unavailable,omitempty"`
	Running     bool                 `json:"running"`
	Image       string               `json:"image,omitempty"`
	Ports       []string             `json:"ports"`
	EntryPoints []IngressEntrypoint  `json:"entrypoints"`
	Resolvers   []string             `json:"resolvers"`
	Notice      string               `json:"notice,omitempty"`
	Blocked     string               `json:"blocked,omitempty"`
	Files       []IngressConfigFile  `json:"files"`
	Change      *IngressConfigChange `json:"change,omitempty"`
}

const IngressFileMissing = "custom configuration file not found"

type customIngressRecord struct {
	Revision     string    `json:"revision"`
	Hosts        []string  `json:"hosts,omitempty"`
	HostPatterns []string  `json:"host_patterns,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// customIngressJournal is written before the proxy's file changes. Until Committed, the outcome is
// the previous file, which the backup in the proxy volume holds; once Committed it is the new one.
type customIngressJournal struct {
	IngressConfigChange
	Digest    string               `json:"digest"`
	Previous  string               `json:"previous,omitempty"`
	Desired   string               `json:"desired,omitempty"`
	Backup    string               `json:"backup,omitempty"`
	Record    *customIngressRecord `json:"record,omitempty"`
	Committed bool                 `json:"committed,omitempty"`
}

type customIngressState struct {
	Managed map[string]customIngressRecord `json:"managed,omitempty"`
	Change  *customIngressJournal          `json:"change,omitempty"`
}

func unsettledCustom(j *customIngressJournal) bool {
	return j != nil && !j.Committed && (j.State == "applying" || j.State == "restoring")
}

func (e *Engine) loadCustomIngress() (customIngressState, error) {
	state := customIngressState{Managed: map[string]customIngressRecord{}}
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress-custom.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Managed == nil {
		state.Managed = map[string]customIngressRecord{}
	}
	return state, nil
}

func (e *Engine) saveCustomIngress(state customIngressState) error {
	return writeJSON(filepath.Join(e.Root, "ingress-custom.json"), state)
}

func (e *Engine) ingressBlocked(state customIngressState) string {
	if err := e.ingressExposureReady(); err != nil {
		return err.Error()
	}
	e.mu.Lock()
	draining := e.draining
	e.mu.Unlock()
	if draining {
		return "The worker is draining for maintenance. Resume it before changing proxy configuration."
	}
	mode, err := e.ingressMode()
	if err != nil {
		return "The proxy migration state is unreadable."
	}
	if mode.Staged {
		return "A same-host migration has staged this proxy. Complete the handover before changing custom proxy configuration."
	}
	if unsettledCustom(state.Change) {
		return "A previous change is being restored. Cove retries until the proxy holds the previous configuration."
	}
	return ""
}

func customFileStatus(name string, size int64, state customIngressState) (string, string) {
	switch {
	case protectedIngressFile(name):
		return "protected", ""
	case !customIngressName.MatchString(name):
		return "unsupported", "Only .yaml and .yml files with lowercase names can be managed."
	case size > MaxCustomIngressBytes:
		return "unsupported", errCustomFileTooLarge.Error()
	}
	if _, ok := state.Managed[name]; ok {
		return "managed", ""
	}
	return "unmanaged", ""
}

func (e *Engine) IngressConfig(ctx context.Context) (IngressConfigState, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	state, err := e.loadCustomIngress()
	if err != nil {
		return IngressConfigState{}, errors.New("custom proxy configuration state is unreadable")
	}
	result := IngressConfigState{Ports: []string{}, EntryPoints: []IngressEntrypoint{}, Resolvers: []string{}, Files: []IngressConfigFile{}, Blocked: e.ingressBlocked(state)}
	if state.Change != nil {
		change := state.Change.IngressConfigChange
		result.Change = &change
	}
	proxy, err := e.inspectProxy(ctx)
	if err != nil {
		result.Unavailable = err.Error()
		return result, nil
	}
	result.Available, result.Running, result.Image = true, proxy.Running, proxy.Image
	if proxy.Ports != nil {
		result.Ports = proxy.Ports
	}
	files, err := e.listProxyFiles(ctx, proxy)
	if err != nil {
		return result, err
	}
	listed := map[string]bool{}
	for _, file := range files {
		listed[file.Name] = true
		status, problem := customFileStatus(file.Name, file.Size, state)
		entry := IngressConfigFile{Name: file.Name, Revision: file.Revision, Size: file.Size, Status: status, Problem: problem}
		if record, ok := state.Managed[file.Name]; ok && status == "managed" {
			entry.Modified = record.Revision != file.Revision
		}
		result.Files = append(result.Files, entry)
	}
	for name := range state.Managed {
		if !listed[name] {
			result.Files = append(result.Files, IngressConfigFile{Name: name, Status: "missing", Problem: "The file was removed outside Cove."})
		}
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Name < result.Files[j].Name })
	if !proxy.Running {
		result.Notice = "The proxy is stopped. Entry points are read from the running proxy, and changes require it to be running."
		return result, nil
	}
	if result.EntryPoints, err = e.proxyEntrypoints(ctx); err != nil {
		result.EntryPoints = []IngressEntrypoint{}
		result.Notice = "The proxy API did not respond, so entry points could not be read."
	}
	if resolvers := e.proxyResolvers(ctx, proxy); resolvers != nil {
		result.Resolvers = sortedKeys(resolvers)
	} else {
		result.Notice = "The proxy's certificate resolvers could not be read from its static configuration."
	}
	return result, nil
}

func (e *Engine) IngressConfigFile(ctx context.Context, name string) (IngressConfigFile, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !customIngressName.MatchString(name) {
		return IngressConfigFile{}, errors.New("invalid custom configuration file name")
	}
	state, err := e.loadCustomIngress()
	if err != nil {
		return IngressConfigFile{}, errors.New("custom proxy configuration state is unreadable")
	}
	proxy, err := e.inspectProxy(ctx)
	if err != nil {
		return IngressConfigFile{}, err
	}
	content, exists, err := e.readProxyFile(ctx, proxy, name)
	if errors.Is(err, errCustomFileTooLarge) {
		return IngressConfigFile{Name: name, Status: "unsupported", Problem: err.Error()}, nil
	}
	if err != nil {
		return IngressConfigFile{}, err
	}
	if !exists {
		return IngressConfigFile{}, errors.New(IngressFileMissing)
	}
	status, problem := customFileStatus(name, int64(len(content)), state)
	file := IngressConfigFile{Name: name, Revision: contentRevision(content), Size: int64(len(content)), Status: status, Problem: problem, Content: &content}
	if record, ok := state.Managed[name]; ok && status == "managed" {
		file.Modified = record.Revision != file.Revision
	}
	config, err := decodeCustomIngress(content)
	if err != nil {
		if file.Problem == "" {
			file.Problem = err.Error()
		}
		return file, nil
	}
	var live map[string]rawObject
	if proxy.Running {
		if live, err = e.liveRawData(ctx); err != nil {
			file.Warnings = append(file.Warnings, "Live object status is unavailable because the proxy API did not respond.")
		}
	}
	for _, object := range config.Objects {
		entry := IngressConfigObject{Kind: object.Section + "." + object.Group, Name: object.Name, Status: "not_reported"}
		if _, observable := rawSections[entry.Kind]; observable && live != nil {
			entry.Status = "not_loaded"
			if got, ok := live[object.key()]; ok {
				entry.Status, entry.Errors = got.Status, got.Errors
			}
		}
		file.Objects = append(file.Objects, entry)
	}
	if status == "protected" || !proxy.Running {
		return file, nil
	}
	files, err := e.listProxyFiles(ctx, proxy)
	if err != nil {
		return file, err
	}
	checks, _, warnings, err := e.customContext(ctx, proxy, files, name)
	if err != nil {
		file.Warnings = append(file.Warnings, err.Error())
		return file, nil
	}
	issues, notes := customIngressIssues(config, checks)
	for _, issue := range issues {
		file.Warnings = append(file.Warnings, issue.Message)
	}
	file.Warnings = append(append(file.Warnings, notes...), warnings...)
	return file, nil
}

// customContext gathers what custom configuration must not conflict with: the proxy's static
// entry points and resolvers, Cove-managed hostnames and names defined by other files.
func (e *Engine) customContext(ctx context.Context, proxy proxyInfo, files []proxyFile, exclude string) (customIngressContext, map[string]customIngress, []string, error) {
	c := customIngressContext{EntryPoints: map[string]bool{}, Others: map[string]string{}}
	others := map[string]customIngress{}
	var warnings []string
	entryPoints, err := e.proxyEntrypoints(ctx)
	if err != nil {
		return c, nil, nil, err
	}
	for _, entryPoint := range entryPoints {
		c.EntryPoints[entryPoint.Name] = true
	}
	c.Resolvers = e.proxyResolvers(ctx, proxy)
	if c.Resolvers == nil {
		warnings = append(warnings, "The proxy's certificate resolvers could not be determined, so resolver names were not checked.")
	}
	routes, err := e.loadIngress()
	if err != nil {
		return c, nil, nil, err
	}
	for _, list := range routes {
		for _, route := range list {
			if !route.IsTransportRoute() {
				c.Domains = append(c.Domains, route.Domain)
			}
		}
	}
	if proxy.PanelDomain != "" && proxy.PanelDomain != "<no value>" {
		c.Domains = append(c.Domains, strings.ToLower(proxy.PanelDomain))
	}
	for _, file := range files {
		if file.Name == exclude || !(strings.HasSuffix(file.Name, ".yaml") || strings.HasSuffix(file.Name, ".yml")) {
			continue
		}
		content, _, err := e.readProxyFile(ctx, proxy, file.Name)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Names in %s were not checked: %v.", file.Name, err))
			continue
		}
		config, err := decodeCustomIngress(content)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Names in %s were not checked: %v.", file.Name, err))
			continue
		}
		for _, object := range config.Objects {
			c.Others[object.key()] = file.Name
		}
		if !protectedIngressFile(file.Name) {
			others[file.Name] = config
		}
	}
	return c, others, warnings, nil
}

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validateIngressConfigRequest(r IngressConfigRequest) error {
	switch r.Action {
	case "put", "validate":
		if r.Content == nil {
			return errors.New("content is required")
		}
	case "delete", "import":
		if r.Content != nil {
			return errors.New("content is not accepted for this action")
		}
	default:
		return errors.New("unsupported custom configuration action")
	}
	if r.Action != "validate" && !identifier.MatchString(r.ChangeID) {
		return errors.New("a change ID is required")
	}
	if !customIngressName.MatchString(r.Name) {
		return errors.New("file names must be lowercase .yaml or .yml names using letters, digits, dots, dashes and underscores")
	}
	if protectedIngressFile(r.Name) {
		return errors.New("routes.yaml, panel.yaml and cove- files are generated by Cove and cannot be changed here")
	}
	if r.Revision != "" && !revisionPattern.MatchString(r.Revision) {
		return errors.New("invalid revision")
	}
	return nil
}

func ingressDigest(r IngressConfigRequest) string {
	content := ""
	if r.Content != nil {
		content = contentRevision(*r.Content)
	}
	data, _ := json.Marshal([]string{r.Action, r.Name, r.Revision, content})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func refused(code, message string) IngressConfigResult {
	return IngressConfigResult{Code: code, Error: message}
}

// ChangeIngressConfig validates, imports, writes or removes one custom dynamic configuration file.
// Changes are serialized with Cove's generated routes and confirmed against the running proxy.
func (e *Engine) ChangeIngressConfig(ctx context.Context, r IngressConfigRequest) IngressConfigResult {
	if err := validateIngressConfigRequest(r); err != nil {
		return refused("invalid", err.Error())
	}
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	state, err := e.loadCustomIngress()
	if err != nil {
		return refused("", "custom proxy configuration state is unreadable")
	}
	routes, err := e.loadIngress()
	if err != nil {
		return refused("unavailable", "managed route inventory is unreadable")
	}
	if r.Content != nil {
		if err := validateTransportCustomConfig(*r.Content, transportRouteReferences(routes)); err != nil {
			return refused("invalid", err.Error())
		}
		if err := validatePrivateCustomConfig(*r.Content, len(privateRouteGuards(routes)) > 0); err != nil {
			return refused("invalid", err.Error())
		}
	}
	digest := ingressDigest(r)
	if r.Action != "validate" {
		if j := state.Change; j != nil && j.ID == r.ChangeID {
			if j.Digest != digest {
				return refused("invalid", "this change ID was already used for different input")
			}
			change := j.IngressConfigChange
			return IngressConfigResult{Change: &change}
		}
		if reason := e.ingressBlocked(state); reason != "" {
			return refused("blocked", reason)
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	proxy, err := e.inspectProxy(checkCtx)
	if err != nil {
		return refused("unavailable", err.Error())
	}
	if !proxy.Running {
		return refused("unavailable", "The managed proxy is not running. Cove does not start or restart the proxy from here; start it, then retry.")
	}
	files, err := e.listProxyFiles(checkCtx, proxy)
	if err != nil {
		return refused("unavailable", err.Error())
	}
	current, exists, err := e.readProxyFile(checkCtx, proxy, r.Name)
	if err != nil {
		return refused("invalid", err.Error())
	}
	revision := ""
	if exists {
		revision = contentRevision(current)
	}
	if r.Action != "validate" && revision != r.Revision {
		return refused("conflict", "The file changed after it was loaded. Reload it and reapply your edits.")
	}
	_, managed := state.Managed[r.Name]
	switch r.Action {
	case "import":
		if !exists {
			return refused("not_found", IngressFileMissing)
		}
		if managed {
			return refused("invalid", "This file is already managed by Cove.")
		}
		return e.importCustomIngress(checkCtx, r, state, proxy, files, current, digest)
	case "put":
		if exists && !managed {
			return refused("invalid", "Import this file before editing it, so its current contents are reviewed first.")
		}
		if !exists {
			count := 0
			for _, file := range files {
				if !protectedIngressFile(file.Name) {
					count++
				}
			}
			if count >= maxCustomIngressFiles {
				return refused("invalid", fmt.Sprintf("A proxy can have at most %d custom configuration files.", maxCustomIngressFiles))
			}
		}
	case "delete":
		if !exists {
			return refused("not_found", IngressFileMissing)
		}
		if !managed {
			return refused("invalid", "Only files managed by Cove can be removed. Import it first.")
		}
	}
	return e.applyCustomIngress(ctx, checkCtx, r, state, proxy, files, current, exists, digest)
}

func (e *Engine) importCustomIngress(ctx context.Context, r IngressConfigRequest, state customIngressState, proxy proxyInfo, files []proxyFile, current, digest string) IngressConfigResult {
	config, err := parseCustomIngress(current)
	if err != nil {
		return refused("invalid", "Cove cannot manage this file: "+err.Error())
	}
	c, _, warnings, err := e.customContext(ctx, proxy, files, r.Name)
	if err != nil {
		return refused("unavailable", err.Error())
	}
	issues, notes := customIngressIssues(config, c)
	// Importing changes nothing on the proxy, so existing conflicts are reported rather than refused.
	for _, issue := range issues {
		warnings = append(warnings, "Existing issue: "+issue.Message)
	}
	hosts, patterns := hostRecord(config)
	now := time.Now().UTC()
	revision := contentRevision(current)
	state.Managed[r.Name] = customIngressRecord{Revision: revision, Hosts: hosts, HostPatterns: patterns, UpdatedAt: now}
	state.Change = &customIngressJournal{IngressConfigChange: IngressConfigChange{ID: r.ChangeID, Action: r.Action, Name: r.Name, State: "succeeded", Message: "Cove now manages this file. Its contents and the proxy were not changed.", Warnings: append(warnings, notes...), Revision: revision, StartedAt: now, FinishedAt: &now}, Digest: digest, Committed: true}
	if err = e.saveCustomIngress(state); err != nil {
		return refused("", "could not record the import")
	}
	change := state.Change.IngressConfigChange
	return IngressConfigResult{Change: &change}
}

func (e *Engine) applyCustomIngress(ctx, checkCtx context.Context, r IngressConfigRequest, state customIngressState, proxy proxyInfo, files []proxyFile, current string, exists bool, digest string) IngressConfigResult {
	change := IngressConfigChange{ID: r.ChangeID, Action: r.Action, Name: r.Name, StartedAt: time.Now().UTC()}
	reject := func(problems, warnings []string) IngressConfigResult {
		now := time.Now().UTC()
		change.State, change.Problems, change.Warnings, change.FinishedAt = "rejected", problems, warnings, &now
		change.Message = "Nothing was changed on the proxy."
		if r.Action == "validate" {
			change.State, change.Message = "invalid", ""
			return IngressConfigResult{Change: &change}
		}
		state.Change = &customIngressJournal{IngressConfigChange: change, Digest: digest}
		if err := e.saveCustomIngress(state); err != nil {
			return refused("", "could not record the rejected change")
		}
		return IngressConfigResult{Change: &change}
	}
	var candidate customIngress
	desired := ""
	if r.Content != nil {
		parsed, err := parseCustomIngress(*r.Content)
		if err != nil {
			return reject([]string{err.Error()}, nil)
		}
		candidate, desired = parsed, contentRevision(*r.Content)
	}
	c, others, warnings, err := e.customContext(checkCtx, proxy, files, r.Name)
	if err != nil {
		return refused("unavailable", err.Error())
	}
	var previous customIngress
	previousParsed := false
	known := map[string]bool{}
	if exists {
		if previous, err = decodeCustomIngress(current); err == nil {
			previousParsed = true
			issues, _ := customIngressIssues(previous, c)
			for _, issue := range issues {
				known[issue.Key] = true
			}
		}
	}
	var problems []string
	if r.Content != nil {
		issues, notes := customIngressIssues(candidate, c)
		for _, issue := range issues {
			if known[issue.Key] {
				warnings = append(warnings, "Existing issue: "+issue.Message)
			} else {
				problems = append(problems, issue.Message)
			}
		}
		warnings = append(warnings, notes...)
	}
	if len(problems) > 0 {
		return reject(problems, warnings)
	}
	live, err := e.liveRawData(checkCtx)
	if err != nil {
		return refused("unavailable", err.Error())
	}
	entryPoints, err := e.proxyEntrypoints(checkCtx)
	if err != nil {
		return refused("unavailable", err.Error())
	}
	resolvers := sortedKeys(c.Resolvers)
	if c.Resolvers == nil {
		// Unknown resolvers are declared for the referenced names so only the rest is validated.
		referenced := map[string]bool{}
		for _, config := range append([]customIngress{candidate}, mapValues(others)...) {
			for _, router := range config.Routers {
				if router.CertResolver != "" {
					referenced[router.CertResolver] = true
				}
			}
		}
		resolvers = sortedKeys(referenced)
	}
	// Only volume paths are probed; any other path was refused above or already existed in this file.
	readable := slices.DeleteFunc(slices.Clone(candidate.CertFiles), func(file string) bool { return !certificatePath(file) })
	v, err := e.validateWithTraefik(checkCtx, proxy, r.Name, r.Content, entryPoints, resolvers, readable)
	if err != nil {
		return refused("unavailable", "Traefik validation could not run: "+err.Error())
	}
	secrets := append(slices.Clone(candidate.Secrets), previous.Secrets...)
	for _, config := range others {
		secrets = append(secrets, config.Secrets...)
	}
	if problems = validationProblems(v, r.Name, candidate, others, live, secrets); len(problems) > 0 {
		return reject(problems, warnings)
	}
	if r.Action == "validate" {
		now := time.Now().UTC()
		change.State, change.Warnings, change.FinishedAt = "valid", warnings, &now
		change.Message = "Traefik accepted the configuration. Nothing was changed on the proxy."
		return IngressConfigResult{Change: &change}
	}
	expected := map[string]rawObject{}
	for _, object := range candidate.Objects {
		if got, ok := v.Objects[object.key()]; ok {
			expected[object.key()] = got
		}
	}
	var absent []string
	if previousParsed {
		for _, object := range previous.Objects {
			_, observable := rawSections[object.Section+"."+object.Group]
			if _, kept := expected[object.key()]; observable && !kept && c.Others[object.key()] == "" {
				absent = append(absent, object.key())
			}
		}
	}
	change.Warnings = warnings
	j := &customIngressJournal{IngressConfigChange: change, Digest: digest, Desired: desired}
	if exists {
		j.Previous = contentRevision(current)
	}
	if r.Action == "put" {
		hosts, patterns := hostRecord(candidate)
		j.Record = &customIngressRecord{Revision: desired, Hosts: hosts, HostPatterns: patterns}
	}
	return e.activateCustomIngress(ctx, state, j, proxy, r.Content, expected, absent)
}

func mapValues(values map[string]customIngress) []customIngress {
	list := make([]customIngress, 0, len(values))
	for _, value := range values {
		list = append(list, value)
	}
	return list
}

const customRestoring = "The change did not complete. Restoring the previous configuration; the worker retries until the proxy holds it."

var customConfirmTimeout = 30 * time.Second

func (e *Engine) activateCustomIngress(ctx context.Context, state customIngressState, j *customIngressJournal, proxy proxyInfo, content *string, expected map[string]rawObject, absent []string) IngressConfigResult {
	// The proxy is changed even if the caller disconnects, so the journal always settles.
	live, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	j.State = "applying"
	if j.Action == "delete" {
		j.Backup = j.ID + ".deleted"
	}
	out := &limitedBuffer{Limit: 64 * 1024}
	prepare := `h="/data/cove-custom-history/$1"
mkdir -p "$h" /data/dynamic
if [ -n "$3" ]; then cp "/data/dynamic/$1" "$h/$3"; sync; sha256sum < "$h/$3" | cut -d' ' -f1; fi`
	backup := ""
	if j.Action == "put" && j.Previous != "" {
		j.Backup = j.ID + ".previous"
		backup = j.Backup
	}
	if err := e.proxyShell(live, proxy, out, prepare, j.Name, j.ID, backup); err != nil {
		return refused("unavailable", "could not preserve the current file in the proxy volume")
	}
	if backup != "" && strings.TrimSpace(out.data.String()) != j.Previous {
		return refused("conflict", "The file changed while the change was validated. Reload it and reapply your edits.")
	}
	state.Change = j
	if err := e.saveCustomIngress(state); err != nil {
		return refused("", "could not journal the change; the proxy was not changed")
	}
	err := e.replaceProxyFile(live, proxy, j, content)
	if err == nil {
		err = e.confirmCustomIngress(live, proxy, j, expected, absent)
	}
	if err != nil {
		return e.failCustomIngress(live, state, j, err)
	}
	if err = e.pruneCustomHistory(live, proxy, j.Name); err != nil {
		j.Warnings = append(j.Warnings, "Older versions in the proxy volume could not be pruned.")
	}
	now := time.Now().UTC()
	j.State, j.Committed, j.FinishedAt, j.Revision = "succeeded", true, &now, j.Desired
	j.Message = "Traefik loaded the change without restarting."
	if j.Action == "delete" {
		delete(state.Managed, j.Name)
		j.Message = "Traefik unloaded the file without restarting. A copy is retained in the proxy volume under " + customHistoryDir + "/" + j.Name + "."
	} else {
		record := *j.Record
		record.UpdatedAt = now
		state.Managed[j.Name] = record
	}
	if err = e.saveCustomIngress(state); err != nil {
		// The commit may have reached the disk, so only recovery reading the journal can decide.
		e.scheduleCustomRecovery()
		return refused("", "The proxy accepted the change, but the result could not be recorded. The worker settles it automatically; reload to see the outcome.")
	}
	change := j.IngressConfigChange
	return IngressConfigResult{Change: &change}
}

func (e *Engine) replaceProxyFile(ctx context.Context, proxy proxyInfo, j *customIngressJournal, content *string) error {
	out := &limitedBuffer{Limit: 64 * 1024}
	if j.Action == "delete" {
		script := `mv "/data/dynamic/$1" "/data/cove-custom-history/$1/$2"; sync`
		if err := e.proxyShell(ctx, proxy, out, script, j.Name, j.Backup); err != nil {
			return errors.New("could not remove the file from the dynamic directory")
		}
		return nil
	}
	staged, err := os.CreateTemp(e.Root, ".ingress-custom-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	if err = staged.Chmod(0600); err == nil {
		_, err = staged.WriteString(*content)
	}
	if err == nil {
		err = staged.Sync()
	}
	if closeErr := staged.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("could not stage the file on the worker")
	}
	next := customHistoryDir + "/" + j.Name + "/" + j.ID + ".next"
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", staged.Name(), "cove-ingress:" + next}, nil, out); err != nil {
		return errors.New("could not copy the file into the proxy volume")
	}
	// Renaming within the volume replaces the file atomically for Traefik's watcher.
	script := `h="/data/cove-custom-history/$1"; [ -f "$h/$2.next" ]; sync; mv "$h/$2.next" "/data/dynamic/$1"; sync`
	if err = e.proxyShell(ctx, proxy, out, script, j.Name, j.ID); err != nil {
		return errors.New("could not activate the file in the dynamic directory")
	}
	return nil
}

func (e *Engine) confirmCustomIngress(ctx context.Context, proxy proxyInfo, j *customIngressJournal, expected map[string]rawObject, absent []string) error {
	revision, err := e.proxyFileRevision(ctx, proxy, j.Name)
	if err != nil {
		return err
	}
	if revision != j.Desired {
		return errors.New("the dynamic directory does not hold the new file")
	}
	deadline := time.Now().Add(customConfirmTimeout)
	for {
		if live, err := e.liveRawData(ctx); err == nil && liveMatches(live, expected, absent) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Traefik did not load the change within %s", customConfirmTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func liveMatches(live, expected map[string]rawObject, absent []string) bool {
	for key, want := range expected {
		if got, ok := live[key]; !ok || got.Config != want.Config {
			return false
		}
	}
	for _, key := range absent {
		if _, ok := live[key]; ok {
			return false
		}
	}
	return true
}

func (e *Engine) failCustomIngress(ctx context.Context, state customIngressState, j *customIngressJournal, cause error) IngressConfigResult {
	restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := e.restoreCustomFile(restore, j); err != nil {
		j.State, j.Message = "restoring", customRestoring
		if saveErr := e.saveCustomIngress(state); saveErr != nil {
			fmt.Fprintln(os.Stderr, "Could not record the custom proxy configuration restore:", saveErr)
		}
		e.scheduleCustomRecovery()
		change := j.IngressConfigChange
		return IngressConfigResult{Change: &change}
	}
	now := time.Now().UTC()
	j.State, j.FinishedAt = "failed", &now
	j.Message = fmt.Sprintf("The change was not applied: %v. The previous configuration is back in place.", cause)
	if err := e.saveCustomIngress(state); err != nil {
		// The journal still reads applying, so recovery restores again and records the outcome.
		e.scheduleCustomRecovery()
	}
	change := j.IngressConfigChange
	return IngressConfigResult{Change: &change}
}

// restoreCustomFile puts the previous file back, or removes a new one, and confirms the result.
// Repeating it is harmless.
func (e *Engine) restoreCustomFile(ctx context.Context, j *customIngressJournal) error {
	proxy, err := e.inspectProxy(ctx)
	if err != nil {
		return err
	}
	script := `h="/data/cove-custom-history/$1"; f="/data/dynamic/$1"
mkdir -p "$h"
rm -f "$h/$2.next"
if [ -n "$3" ]; then
  if [ -f "$h/$3" ]; then cp "$h/$3" "$h/$2.restore"; sync; mv "$h/$2.restore" "$f"; sync; fi
elif [ -f "$f" ]; then
  mv "$f" "$h/$2.rejected"; sync
fi`
	backup := ""
	if j.Previous != "" {
		backup = j.Backup
	}
	if err = e.proxyShell(ctx, proxy, &limitedBuffer{Limit: 64 * 1024}, script, j.Name, j.ID, backup); err != nil {
		return errors.New("could not restore the previous file in the proxy volume")
	}
	revision, err := e.proxyFileRevision(ctx, proxy, j.Name)
	if err != nil {
		return err
	}
	if revision != j.Previous {
		return errors.New("the dynamic directory does not hold the previous file")
	}
	return nil
}

func (e *Engine) pruneCustomHistory(ctx context.Context, proxy proxyInfo, name string) error {
	script := `cd "/data/cove-custom-history/$1" 2>/dev/null || exit 0
ls -1t | tail -n +11 | while IFS= read -r f; do rm -f -- "$f"; done`
	return e.proxyShell(ctx, proxy, &limitedBuffer{Limit: 64 * 1024}, script, name)
}

func (e *Engine) scheduleCustomRecovery() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.customUnsettled = true
	e.customRetry = time.Now().Add(routeRecoveryRetry)
}

// recoverCustomIngress settles a change that did not commit: the previous file is restored, and
// the journal records the outcome only after the proxy volume holds it again.
func (e *Engine) recoverCustomIngress(ctx context.Context) error {
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	state, err := e.loadCustomIngress()
	if err != nil {
		return err
	}
	j := state.Change
	if !unsettledCustom(j) {
		return nil
	}
	interrupted := j.State == "applying"
	if err = e.restoreCustomFile(ctx, j); err != nil {
		if j.State != "restoring" {
			j.State, j.Message = "restoring", customRestoring
			if saveErr := e.saveCustomIngress(state); saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
		return err
	}
	now := time.Now().UTC()
	j.State, j.FinishedAt = "failed", &now
	j.Message = "The change did not complete. The previous configuration is back in place."
	if interrupted {
		j.State = "interrupted"
		j.Message = "The worker stopped before the change completed. The previous configuration is back in place."
	}
	return e.saveCustomIngress(state)
}

func (e *Engine) runCustomRecovery(ctx context.Context) {
	recovery, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := e.recoverCustomIngress(recovery)
	cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.customRunning = false
	if err == nil {
		e.customUnsettled = false
		return
	}
	fmt.Fprintln(os.Stderr, "Custom proxy configuration recovery failed; retrying:", err)
	e.customRetry = time.Now().Add(routeRecoveryRetry)
}

// customHostConflict refuses resource hostnames that a managed custom file already routes.
// Hostnames the workload already serves are not rechecked, so restoring routes is never blocked.
func (e *Engine) customHostConflict(previous, desired []Route) error {
	state, err := e.loadCustomIngress()
	if err != nil {
		return errors.New("custom proxy configuration state is unreadable")
	}
	names := make([]string, 0, len(state.Managed))
	for name := range state.Managed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, route := range desired {
		if route.IsTransportRoute() {
			continue
		}
		if slices.ContainsFunc(previous, func(owned Route) bool { return owned.Domain == route.Domain }) {
			continue
		}
		for _, name := range names {
			record := state.Managed[name]
			claimed := slices.ContainsFunc(record.Hosts, func(host string) bool { return RouteDomainsOverlap(host, route.Domain) })
			for _, pattern := range record.HostPatterns {
				if expression, err := regexp.Compile(pattern); err == nil && (strings.HasPrefix(route.Domain, "*.") || expression.MatchString(route.Domain)) {
					claimed = true
				}
			}
			if claimed {
				return fmt.Errorf("hostname %s is routed by custom proxy configuration %s; remove it there before adding it to a resource", route.Domain, name)
			}
		}
	}
	return nil
}
