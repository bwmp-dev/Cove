package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const customHistoryDir = "/data/cove-custom-history"

type proxyInfo struct {
	Running     bool
	Image       string
	Args        []string
	Ports       []string
	PanelDomain string
}

var errProxyMissing = errors.New("the managed proxy has not been created on this node yet; Cove creates it with the first resource route")

func (e *Engine) inspectProxy(ctx context.Context) (proxyInfo, error) {
	var info proxyInfo
	out := &limitedBuffer{Limit: 1024 * 1024}
	format := `{"labels":{{json .Config.Labels}},"running":{{json .State.Running}},"image":{{json .Config.Image}},"args":{{json .Args}},"ports":{{json .HostConfig.PortBindings}}}`
	if err := e.Run(ctx, e.Root, []string{"docker", "inspect", "--type", "container", "--format", format, "cove-ingress"}, nil, out); err != nil {
		if strings.Contains(out.data.String(), "No such") {
			return info, errProxyMissing
		}
		return info, errors.New("could not inspect the managed proxy")
	}
	var raw struct {
		Labels  map[string]string                              `json:"labels"`
		Running bool                                           `json:"running"`
		Image   string                                         `json:"image"`
		Args    []string                                       `json:"args"`
		Ports   map[string][]struct{ HostIP, HostPort string } `json:"ports"`
	}
	if err := json.Unmarshal(out.data.Bytes(), &raw); err != nil {
		return info, errors.New("could not inspect the managed proxy")
	}
	if raw.Labels["cove.managed"] != "ingress" {
		return info, errors.New("existing cove-ingress container is not managed by Cove")
	}
	if !strings.HasPrefix(raw.Image, "traefik:") {
		return info, errors.New("the managed proxy needs migration to Traefik before custom configuration can be managed")
	}
	info = proxyInfo{Running: raw.Running, Image: raw.Image, Args: raw.Args, PanelDomain: raw.Labels["cove.panel.domain"]}
	for port, bindings := range raw.Ports {
		for _, binding := range bindings {
			host := binding.HostPort
			if binding.HostIP != "" {
				host = binding.HostIP + ":" + host
			}
			info.Ports = append(info.Ports, host+" → "+port)
		}
	}
	sort.Strings(info.Ports)
	return info, nil
}

// proxyShell runs a script against the proxy's /data volume: inside the running proxy, or in a
// short-lived helper using the same image when it is stopped, so recovery never needs to start it.
func (e *Engine) proxyShell(ctx context.Context, p proxyInfo, out io.Writer, script string, args ...string) error {
	command := []string{"docker", "exec", "cove-ingress", "sh", "-ec", script, "sh"}
	if !p.Running {
		command = []string{"docker", "run", "--rm", "--pull", "never", "--network", "none", "--security-opt", "no-new-privileges:true", "--label", "cove.managed=ingress-helper", "-v", "cove-ingress:/data", "--entrypoint", "sh", p.Image, "-ec", script, "sh"}
	}
	return e.Run(ctx, e.Root, append(command, args...), nil, out)
}

type proxyFile struct {
	Name     string
	Revision string
	Size     int64
}

func (e *Engine) listProxyFiles(ctx context.Context, p proxyInfo) ([]proxyFile, error) {
	out := &limitedBuffer{Limit: 1024 * 1024}
	script := `cd /data/dynamic 2>/dev/null || exit 0
for f in * .[!.]*; do
  [ -f "$f" ] || continue
  printf '%s %s %s\n' "$(sha256sum < "$f" | cut -d' ' -f1)" "$(wc -c < "$f" | tr -d ' ')" "$f"
done`
	if err := e.proxyShell(ctx, p, out, script); err != nil {
		return nil, errors.New("could not list the proxy's dynamic configuration files")
	}
	var files []proxyFile
	for _, line := range strings.Split(out.data.String(), "\n") {
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 || len(fields[0]) != 64 {
			continue
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		files = append(files, proxyFile{Name: fields[2], Revision: fields[0], Size: size})
		if len(files) == 256 {
			break
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

var errCustomFileTooLarge = fmt.Errorf("the file exceeds %d KiB and cannot be managed by Cove", MaxCustomIngressBytes/1024)

func (e *Engine) readProxyFile(ctx context.Context, p proxyInfo, name string) (string, bool, error) {
	out := &limitedBuffer{Limit: MaxCustomIngressBytes + 64}
	script := `f="/data/dynamic/$1"
if [ ! -f "$f" ]; then printf 'absent\n'; exit 0; fi
if [ "$(wc -c < "$f")" -gt "$2" ]; then printf 'large\n'; exit 0; fi
printf 'present\n'
exec cat "$f"`
	if err := e.proxyShell(ctx, p, out, script, name, strconv.Itoa(MaxCustomIngressBytes)); err != nil {
		return "", false, errors.New("could not read the proxy configuration file")
	}
	header, content, _ := strings.Cut(out.data.String(), "\n")
	switch header {
	case "present":
		return content, true, nil
	case "absent":
		return "", false, nil
	case "large":
		return "", true, errCustomFileTooLarge
	}
	return "", false, errors.New("could not read the proxy configuration file")
}

func (e *Engine) proxyFileRevision(ctx context.Context, p proxyInfo, name string) (string, error) {
	out := &limitedBuffer{Limit: 4096}
	script := `f="/data/dynamic/$1"; if [ -f "$f" ]; then sha256sum < "$f" | cut -d' ' -f1; else echo absent; fi`
	if err := e.proxyShell(ctx, p, out, script, name); err != nil {
		return "", errors.New("could not read the proxy configuration file")
	}
	revision := strings.TrimSpace(out.data.String())
	if revision == "absent" {
		return "", nil
	}
	return revision, nil
}

func contentRevision(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) proxyAPI(ctx context.Context, container, path string) ([]byte, error) {
	out := &limitedBuffer{Limit: 16 * 1024 * 1024}
	err := e.Run(ctx, e.Root, []string{"docker", "exec", container, "wget", "-qO-", "http://127.0.0.1:8080" + path}, nil, out)
	return out.data.Bytes(), err
}

// rawObject is a Traefik runtime object reduced to what validation and confirmation compare.
type rawObject struct {
	Status string
	Errors []string
	// Config is Traefik's normalized definition without runtime fields, so two proxies running
	// the same image report identical values for the same input.
	Config string
}

var rawSections = map[string]string{
	"http.routers":     "routers",
	"http.services":    "services",
	"http.middlewares": "middlewares",
	"tcp.routers":      "tcpRouters",
	"tcp.services":     "tcpServices",
	"tcp.middlewares":  "tcpMiddlewares",
	"udp.routers":      "udpRouters",
	"udp.services":     "udpServices",
}

// parseRawData returns file provider objects keyed like customObject.key(). Traefik's raw data
// includes secrets such as basic authentication users, so it is only held in memory.
func parseRawData(data []byte) (map[string]rawObject, error) {
	var raw map[string]map[string]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("the proxy API returned an unreadable response")
	}
	objects := map[string]rawObject{}
	for kind, section := range rawSections {
		for name, definition := range raw[section] {
			base, ok := strings.CutSuffix(name, "@file")
			if !ok {
				continue
			}
			object := rawObject{}
			object.Status, _ = definition["status"].(string)
			if list, ok := definition["error"].([]any); ok {
				for _, item := range list {
					if message, ok := item.(string); ok {
						object.Errors = append(object.Errors, message)
					}
				}
			}
			for _, field := range []string{"status", "error", "using", "usedBy", "serverStatus", "observability"} {
				delete(definition, field)
			}
			config, err := json.Marshal(definition)
			if err != nil {
				return nil, err
			}
			object.Config = string(config)
			objects[kind+"/"+base] = object
		}
	}
	return objects, nil
}

func (e *Engine) liveRawData(ctx context.Context) (map[string]rawObject, error) {
	data, err := e.proxyAPI(ctx, "cove-ingress", "/api/rawdata")
	if err != nil {
		return nil, errors.New("the proxy API did not respond")
	}
	return parseRawData(data)
}

type IngressEntrypoint struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

func (e *Engine) proxyEntrypoints(ctx context.Context) ([]IngressEntrypoint, error) {
	data, err := e.proxyAPI(ctx, "cove-ingress", "/api/entrypoints")
	if err != nil {
		return nil, errors.New("the proxy API did not respond")
	}
	var entryPoints []IngressEntrypoint
	if err = json.Unmarshal(data, &entryPoints); err != nil {
		return nil, errors.New("the proxy API returned an unreadable response")
	}
	sort.Slice(entryPoints, func(i, j int) bool { return entryPoints[i].Name < entryPoints[j].Name })
	return entryPoints, nil
}

var (
	resolverFlag   = regexp.MustCompile(`(?i)--certificatesresolvers\.([a-z0-9_-]+)\.`)
	configFileFlag = regexp.MustCompile(`(?i)--configfile[= ]([^\s"']+)`)
	staticFilePath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+\.ya?ml$`)
	resolverName   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// proxyResolvers returns the proxy's certificate resolver names, or nil when its static
// configuration cannot be read.
func (e *Engine) proxyResolvers(ctx context.Context, p proxyInfo) map[string]bool {
	joined := strings.Join(p.Args, " ")
	resolvers := map[string]bool{}
	for _, match := range resolverFlag.FindAllStringSubmatch(joined, -1) {
		resolvers[match[1]] = true
	}
	match := configFileFlag.FindStringSubmatch(joined)
	if match == nil {
		if strings.Contains(joined, "--entrypoints.") {
			return resolvers
		}
		return nil
	}
	if !p.Running || !staticFilePath.MatchString(match[1]) {
		return nil
	}
	out := &limitedBuffer{Limit: 1024 * 1024}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", "cove-ingress", "cat", match[1]}, nil, out); err != nil {
		return nil
	}
	var static map[string]any
	if err := yaml.Unmarshal(out.data.Bytes(), &static); err != nil {
		return nil
	}
	configured, _ := asMap(lookup(static, "certificatesResolvers"))
	for name := range configured {
		resolvers[name] = true
	}
	return resolvers
}

type validation struct {
	Loaded     bool
	Exited     bool
	Objects    map[string]rawObject
	Logs       []map[string]any
	Unreadable []string
}

var (
	entryPointName    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	entryPointAddress = regexp.MustCompile(`^[0-9A-Za-z.:\[\]%-]*:[0-9]{1,5}(?:/(?:tcp|udp))?$`)
)

const validatorScript = `d=/tmp/cove-check
mkdir -p "$d"
for f in /data/dynamic/* /data/dynamic/.[!.]*; do
  [ -f "$f" ] || continue
  [ "${f##*/}" = "$1" ] && continue
  cp "$f" "$d/"
done
if [ -f /cove-check-input/candidate ]; then cp /cove-check-input/candidate "$d/$1"; fi
cp /cove-check-input/sentinel.yaml "$d/cove-check-sentinel.yaml"
shift
exec traefik "$@"`

const sentinelKey = "http.services/cove-check-sentinel"

const sentinelConfig = "http:\n  services:\n    cove-check-sentinel:\n      loadBalancer:\n        servers:\n          - url: http://127.0.0.1:9\n"

var (
	validationTimeout = 25 * time.Second
	// validationSettle lets router handlers and resolver checks finish after the configuration is published.
	validationSettle = time.Second
)

// validateWithTraefik loads the proxy's dynamic directory, with name replaced by candidate or
// removed when candidate is nil, into an isolated Traefik of the same image. The instance has no
// network, published ports or writable proxy volumes, and is removed afterwards.
func (e *Engine) validateWithTraefik(ctx context.Context, p proxyInfo, name string, candidate *string, entryPoints []IngressEntrypoint, resolvers []string, certFiles []string) (validation, error) {
	result := validation{}
	e.removeValidators(ctx)
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return result, err
	}
	container := "cove-ingress-check-" + hex.EncodeToString(suffix)
	staging, err := os.MkdirTemp(e.Root, ".ingress-check-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(staging)
	if candidate != nil {
		if err = os.WriteFile(filepath.Join(staging, "candidate"), []byte(*candidate), 0600); err != nil {
			return result, err
		}
	}
	if err = os.WriteFile(filepath.Join(staging, "sentinel.yaml"), []byte(sentinelConfig), 0600); err != nil {
		return result, err
	}
	args := []string{"--api.insecure=true", "--api.dashboard=false", "--entrypoints.traefik.address=127.0.0.1:8080", "--providers.file.directory=/tmp/cove-check", "--log.level=ERROR", "--log.format=json"}
	for _, entryPoint := range entryPoints {
		if entryPoint.Name != "traefik" && entryPointName.MatchString(entryPoint.Name) && entryPointAddress.MatchString(entryPoint.Address) {
			args = append(args, "--entrypoints."+entryPoint.Name+".address="+entryPoint.Address)
		}
	}
	for _, resolver := range resolvers {
		if resolverName.MatchString(resolver) {
			prefix := "--certificatesresolvers." + resolver + ".acme."
			// Network isolation keeps the CA unreachable; only the resolver's existence matters here.
			args = append(args, prefix+"storage=/tmp/cove-acme-"+resolver+".json", prefix+"tlschallenge=true", prefix+"caserver=https://127.0.0.1:9/directory")
		}
	}
	create := []string{"docker", "create", "--name", container, "--label", "cove.managed=ingress-check", "--network", "none", "--pull", "never", "--security-opt", "no-new-privileges:true", "--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE", "--cap-add", "DAC_OVERRIDE", "--log-driver", "json-file", "--log-opt", "max-size=1m", "--volumes-from", "cove-ingress:ro", "-e", "COVE_DOMAIN=cove-panel.invalid", "--entrypoint", "sh", p.Image, "-ec", validatorScript, "sh", name}
	if err = e.Run(ctx, e.Root, append(create, args...), nil, &limitedBuffer{Limit: 64 * 1024}); err != nil {
		return result, errors.New("could not create the validation proxy")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", container}, nil, &limitedBuffer{Limit: 64 * 1024}); err != nil {
			fmt.Fprintln(os.Stderr, "Could not remove proxy validation container", container, "; it is removed before the next validation:", err)
		}
	}()
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", staging, container + ":/cove-check-input"}, nil, &limitedBuffer{Limit: 64 * 1024}); err != nil {
		return result, errors.New("could not stage the validation input")
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "start", container}, nil, &limitedBuffer{Limit: 64 * 1024}); err != nil {
		return result, errors.New("could not start the validation proxy")
	}
	deadline := time.Now().Add(validationTimeout)
	for {
		if data, apiErr := e.proxyAPI(ctx, container, "/api/rawdata"); apiErr == nil {
			if objects, parseErr := parseRawData(data); parseErr == nil {
				if _, ok := objects[sentinelKey]; ok {
					result.Loaded = true
					break
				}
			}
		}
		if running, _ := e.output(ctx, "docker", "inspect", "--format", "{{.State.Running}}", container); running != "true" {
			result.Exited = true
			break
		}
		if logs := e.validatorLogs(ctx, container); slices.ContainsFunc(logs, func(entry map[string]any) bool { return entry["providerName"] == "file" }) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if result.Loaded {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(validationSettle):
		}
		data, err := e.proxyAPI(ctx, container, "/api/rawdata")
		if err != nil {
			return result, errors.New("the validation proxy API did not respond")
		}
		if result.Objects, err = parseRawData(data); err != nil {
			return result, err
		}
		if len(certFiles) > 0 {
			out := &limitedBuffer{Limit: 64 * 1024}
			check := append([]string{"docker", "exec", container, "sh", "-ec", `for f do [ -r "$f" ] || printf '%s\n' "$f"; done`, "sh"}, certFiles...)
			if err = e.Run(ctx, e.Root, check, nil, out); err != nil {
				return result, errors.New("could not check certificate files in the validation proxy")
			}
			result.Unreadable = strings.Fields(out.data.String())
		}
	}
	result.Logs = e.validatorLogs(ctx, container)
	return result, nil
}

func (e *Engine) validatorLogs(ctx context.Context, container string) []map[string]any {
	out := &limitedBuffer{Limit: 1024 * 1024}
	if err := e.Run(ctx, e.Root, []string{"docker", "logs", container}, nil, out); err != nil {
		return nil
	}
	var entries []map[string]any
	for _, line := range strings.Split(out.data.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["level"] == "error" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// removeValidators removes validation containers a crashed worker left behind.
func (e *Engine) removeValidators(ctx context.Context) {
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=cove.managed=ingress-check")
	if err != nil {
		return
	}
	for _, id := range strings.Fields(ids) {
		if err := e.Run(ctx, e.Root, []string{"docker", "rm", "-f", id}, nil, &limitedBuffer{Limit: 64 * 1024}); err != nil {
			fmt.Fprintln(os.Stderr, "Could not remove stale proxy validation container", id+":", err)
		}
	}
}

func acmeLog(entry map[string]any) bool {
	if _, ok := entry["resolver"]; ok {
		return true
	}
	provider, _ := entry["providerName"].(string)
	message, _ := entry["message"].(string)
	detail, _ := entry["error"].(string)
	return strings.Contains(provider, "acme") || strings.Contains(strings.ToLower(message+detail), "acme")
}

func logMessage(entry map[string]any) string {
	message, _ := entry["message"].(string)
	if detail, _ := entry["error"].(string); detail != "" {
		if message != "" {
			message += ": "
		}
		message += detail
	}
	// The validation copy's directory is an implementation detail; file names identify the source.
	return strings.ReplaceAll(message, "/tmp/cove-check/", "")
}

func logObject(entry map[string]any) bool {
	for _, field := range []string{"routerName", "serviceName", "middlewareName", "entryPointName"} {
		if _, ok := entry[field]; ok {
			return true
		}
	}
	return false
}

// validationProblems reports what the candidate would break. Errors that the live proxy already
// shows for an object are pre-existing and do not block an unrelated change.
func validationProblems(v validation, name string, candidate customIngress, others map[string]customIngress, live map[string]rawObject, secrets []string) []string {
	var problems []string
	add := func(message string) {
		message = redact(message, secrets)
		if !slices.Contains(problems, message) {
			problems = append(problems, message)
		}
	}
	if !v.Loaded {
		for _, entry := range v.Logs {
			if !acmeLog(entry) && !logObject(entry) {
				add("Traefik rejected the configuration: " + logMessage(entry))
			}
		}
		if len(problems) == 0 {
			add("Traefik did not load the configuration within the validation period.")
		}
		return problems
	}
	check := func(object customObject, file string) {
		if _, observable := rawSections[object.Section+"."+object.Group]; !observable {
			return
		}
		got, ok := v.Objects[object.key()]
		if !ok {
			if file == name {
				add(object.label() + " was not loaded by Traefik.")
			}
			return
		}
		var introduced []string
		for _, message := range got.Errors {
			if !slices.Contains(live[object.key()].Errors, message) {
				introduced = append(introduced, message)
			}
		}
		if len(introduced) > 0 {
			where := ""
			if file != name {
				where = " in " + file
			}
			add(fmt.Sprintf("%s%s: %s", object.label(), where, strings.Join(introduced, "; ")))
		}
	}
	for _, object := range candidate.Objects {
		check(object, name)
	}
	files := make([]string, 0, len(others))
	for file := range others {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		for _, object := range others[file].Objects {
			check(object, file)
		}
	}
	if candidate.TLS {
		for _, entry := range v.Logs {
			if !acmeLog(entry) && !logObject(entry) {
				add("Traefik reported a TLS problem: " + logMessage(entry))
			}
		}
	}
	for _, path := range v.Unreadable {
		add(fmt.Sprintf("Certificate file %s is not readable inside the proxy.", path))
	}
	return problems
}
