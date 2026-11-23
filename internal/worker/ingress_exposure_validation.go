package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

func validIngressValidatorName(name string) bool {
	suffix, ok := strings.CutPrefix(name, "cove-ingress-check-")
	if !ok || len(suffix) != 12 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func cleanupIngressExposureValidator(ctx context.Context, client *http.Client, change *ingressProxyChange) error {
	if change.Validator == "" {
		return nil
	}
	if !validIngressValidatorName(change.Validator) || change.Previous == nil {
		return errors.New("proxy validation recovery identity is invalid")
	}
	helper, err := ingressProxyInspect(ctx, client, change.Validator)
	if err != nil {
		return err
	}
	if helper == nil {
		return nil
	}
	if helper.Config.Labels["cove.managed"] != "ingress-check" || helper.Config.Labels["cove.proxy-change"] != change.ID || helper.Image != change.Previous.Image {
		return errors.New("proxy validation helper ownership changed; it was left untouched")
	}
	return ingressDockerJSON(ctx, client, "DELETE", "/containers/"+helper.ID+"?force=1", nil, nil)
}

const exposureValidatorScript = `d=/tmp/cove-check
mkdir -p "$d"
for f in /data/dynamic/* /data/dynamic/.[!.]*; do
  [ -f "$f" ] || continue
  cp "$f" "$d/"
done
if [ "$1" = panel ]; then cp /etc/traefik/panel.yaml "$d/panel.yaml"; fi
cp /cove-check-input/sentinel.yaml "$d/cove-check-sentinel.yaml"
for f in /cove-check-input/cove-acme-*.json; do
  [ -f "$f" ] || continue
  cp "$f" "/tmp/${f##*/}"
  chmod 600 "/tmp/${f##*/}"
done
shift
exec traefik "$@"`

func exposureValidationBody(prepared preparedIngressExposure) (map[string]json.RawMessage, []byte, error) {
	encoded, err := json.Marshal(prepared.Body)
	if err != nil {
		return nil, nil, err
	}
	var body map[string]json.RawMessage
	json.Unmarshal(encoded, &body)
	var labels map[string]string
	json.Unmarshal(body["Labels"], &labels)
	labels["cove.managed"] = "ingress-check"
	labels["cove.proxy-change"] = prepared.Plan.ChangeID
	body["Labels"], _ = json.Marshal(labels)
	body["NetworkingConfig"], _ = json.Marshal(map[string]any{"EndpointsConfig": map[string]any{}})
	body["ExposedPorts"] = json.RawMessage(`{}`)
	body["User"], _ = json.Marshal("0:0")
	body["WorkingDir"], _ = json.Marshal("/tmp")
	body["Entrypoint"], _ = json.Marshal([]string{"sh"})
	var host map[string]json.RawMessage
	if json.Unmarshal(body["HostConfig"], &host) != nil {
		return nil, nil, errors.New("validation host settings are unreadable")
	}
	set := func(name string, value any) { host[name], _ = json.Marshal(value) }
	set("NetworkMode", "none")
	set("PortBindings", map[string]any{})
	set("PublishAllPorts", false)
	set("Binds", []string{})
	set("Mounts", []any{})
	set("VolumesFrom", []string{prepared.Snapshot.ID + ":ro"})
	set("RestartPolicy", map[string]any{"Name": "no"})
	set("AutoRemove", false)
	set("ReadonlyRootfs", false)
	set("CapDrop", []string{"ALL"})
	set("CapAdd", []string{"NET_BIND_SERVICE", "DAC_OVERRIDE"})
	set("SecurityOpt", []string{"no-new-privileges:true"})
	set("Devices", []any{})
	set("Links", []string{})
	set("Dns", []string{})
	set("DnsSearch", []string{})
	set("DnsOptions", []string{})
	set("ExtraHosts", []string{})
	set("DeviceRequests", []any{})
	set("Sysctls", map[string]string{})
	set("Memory", 256<<20)
	set("NanoCpus", int64(1e9))
	set("LogConfig", map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m", "max-file": "1"}})
	body["HostConfig"], _ = json.Marshal(host)
	var environment []string
	json.Unmarshal(body["Env"], &environment)
	filtered := []string{}
	for _, value := range environment {
		upper := strings.ToUpper(value)
		if strings.HasPrefix(upper, "TRAEFIK_LOG_") || strings.HasPrefix(upper, "TRAEFIK_ACCESSLOG") || strings.HasPrefix(upper, "TRAEFIK_CERTIFICATESRESOLVERS_") {
			continue
		}
		filtered = append(filtered, value)
	}
	body["Env"], _ = json.Marshal(filtered)
	mode := "worker"
	arguments := []string{}
	var source []byte
	if prepared.Snapshot.Launch.Kind == "cli" {
		var command []string
		json.Unmarshal(body["Cmd"], &command)
		resolvers := map[string]bool{}
		for _, arg := range command {
			if !strings.HasPrefix(arg, "--") {
				continue
			}
			lower := strings.ToLower(arg)
			if strings.HasPrefix(lower, "--certificatesresolvers.") {
				for _, match := range resolverFlag.FindAllStringSubmatch(arg, -1) {
					resolvers[match[1]] = true
				}
				continue
			}
			if strings.HasPrefix(lower, "--providers.file.") || strings.HasPrefix(lower, "--log.") || strings.HasPrefix(lower, "--accesslog") {
				continue
			}
			arguments = append(arguments, arg)
		}
		arguments = append(arguments, "--providers.file.directory=/tmp/cove-check", "--providers.file.watch=true", "--log.level=ERROR", "--log.format=json")
		for resolver := range resolvers {
			if !resolverName.MatchString(resolver) {
				return nil, nil, errors.New("validation resolver name is invalid")
			}
			prefix := "--certificatesresolvers." + resolver + ".acme."
			arguments = append(arguments, prefix+"storage=/tmp/cove-acme-"+resolver+".json", prefix+"tlschallenge=true", prefix+"caserver=https://127.0.0.1:9/directory")
		}
	} else {
		mode = "panel"
		var config map[string]any
		if yaml.Unmarshal(prepared.Static, &config) != nil {
			return nil, nil, errors.New("validation static file is unreadable")
		}
		for key := range config {
			switch strings.ToLower(key) {
			case "certificatesresolvers", "log", "accesslog", "providers":
				delete(config, key)
			}
		}
		var original map[string]any
		yaml.Unmarshal(prepared.Static, &original)
		resolvers, _ := asMap(lookup(original, "certificatesResolvers"))
		dummy := map[string]any{}
		for resolver := range resolvers {
			if !resolverName.MatchString(resolver) {
				return nil, nil, errors.New("validation resolver name is invalid")
			}
			dummy[resolver] = map[string]any{"acme": map[string]any{"storage": "/tmp/cove-acme-" + resolver + ".json", "tlsChallenge": map[string]any{}, "caServer": "https://127.0.0.1:9/directory"}}
		}
		config["certificatesResolvers"] = dummy
		config["log"] = map[string]any{"level": "ERROR", "format": "json"}
		providers, _ := asMap(lookup(original, "providers"))
		if providers == nil {
			providers = map[string]any{}
		}
		for key := range providers {
			if strings.EqualFold(key, "file") {
				delete(providers, key)
			}
		}
		providers["file"] = map[string]any{"directory": "/tmp/cove-check", "watch": true}
		config["providers"] = providers
		source, err = yaml.Marshal(config)
		if err != nil {
			return nil, nil, errors.New("validation static file could not be encoded")
		}
		arguments = []string{"--configFile=/cove-check-input/static.yaml"}
	}
	body["Cmd"], _ = json.Marshal(append([]string{"-ec", exposureValidatorScript, "sh", mode}, arguments...))
	return body, source, nil
}

func (e *Engine) validateIngressExposure(ctx context.Context, client *http.Client, prepared preparedIngressExposure, state *ingressExposureState) (resultErr error) {
	if state.Change == nil || state.Change.ID != prepared.Plan.ChangeID {
		return errors.New("proxy validation requires its durable change identity")
	}
	body, source, err := exposureValidationBody(prepared)
	if err != nil {
		return err
	}
	if state.Change.Staging != "" || state.Change.Validator != "" {
		return errors.New("recover the saved proxy validation before starting another")
	}
	name := ".ingress-listener-check-" + state.Change.ID
	staging := filepath.Join(e.Root, name)
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		return errors.New("private validation staging already exists or cannot be checked")
	}
	state.Change.Staging = name
	if err := e.saveIngressExposures(*state); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return errors.New("could not create private validation staging")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := cleanupIngressExposureValidator(cleanup, client, state.Change); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		if err := e.cleanupIngressExposureStaging(state.Change); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		state.Change.Validator = ""
		state.Change.Staging = ""
		if err := e.saveIngressExposures(*state); err != nil {
			resultErr = errors.Join(resultErr, errors.New("could not checkpoint private validation cleanup"))
		}
	}()
	if err := writeJSON(filepath.Join(staging, ".owner"), map[string]string{"change_id": state.Change.ID}); err != nil {
		return errors.New("could not preserve private validation ownership")
	}
	if err := os.WriteFile(filepath.Join(staging, "sentinel.yaml"), []byte(sentinelConfig), 0600); err != nil {
		return errors.New("could not stage validation input")
	}
	if len(source) > 0 {
		if err := os.WriteFile(filepath.Join(staging, "static.yaml"), source, 0600); err != nil {
			return errors.New("could not stage private validation configuration")
		}
	}
	remaining := int64(4 * maxACMEStorageBytes)
	for resolver, path := range prepared.ACMEStorage {
		file, err := os.OpenFile(filepath.Join(staging, "cove-acme-"+resolver+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("could not create private certificate cache staging")
		}
		copyErr := copyIngressProxyFile(ctx, client, prepared.Snapshot.ID, path, file, min(int64(maxACMEStorageBytes), remaining))
		info, statErr := file.Stat()
		closeErr := file.Close()
		if errors.Is(copyErr, errIngressDockerMissing) {
			if err := os.Remove(file.Name()); err != nil {
				return errors.New("could not remove unused private cache staging")
			}
			continue
		}
		if copyErr != nil || statErr != nil || closeErr != nil {
			return errors.New("persisted certificate storage could not be copied within the validation limit")
		}
		remaining -= info.Size()
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	state.Change.Validator = "cove-ingress-check-" + hex.EncodeToString(nonce[:])
	if err := e.saveIngressExposures(*state); err != nil {
		return err
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/create?name="+state.Change.Validator, body, &created); err != nil {
		return err
	}
	var isolated struct {
		HostConfig struct {
			NetworkMode  string
			PortBindings map[string]any
		}
		NetworkSettings struct{ Networks map[string]any }
		Mounts          []struct{ RW bool }
	}
	if err := ingressDockerJSON(ctx, client, "GET", "/containers/"+created.ID+"/json", nil, &isolated); err != nil {
		return err
	}
	if isolated.HostConfig.NetworkMode != "none" || len(isolated.HostConfig.PortBindings) > 0 {
		return errors.New("validation proxy isolation was not confirmed")
	}
	for name := range isolated.NetworkSettings.Networks {
		if name != "none" {
			return errors.New("validation proxy received an unexpected network")
		}
	}
	for _, mount := range isolated.Mounts {
		if mount.RW {
			return errors.New("validation proxy received a writable proxy mount")
		}
	}
	out := &limitedBuffer{Limit: 65536}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", staging, state.Change.Validator + ":/cove-check-input"}, nil, out); err != nil {
		return errors.New("could not copy private validation input")
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(validationTimeout)
	for time.Now().Before(deadline) {
		if data, err := e.proxyAPI(ctx, state.Change.Validator, "/api/rawdata"); err == nil {
			objects, err := parseRawData(data)
			if err == nil {
				if _, loaded := objects[sentinelKey]; loaded {
					for _, object := range objects {
						if object.Status == "disabled" || len(object.Errors) > 0 {
							return errors.New("proposed proxy configuration has invalid runtime objects; repair existing proxy configuration before changing listeners")
						}
					}
					return e.probeIngressExposureTLS(ctx, client, created.ID, prepared.Plan.ChangeID, prepared.Probes)
				}
			}
		}
		helper, err := ingressProxyInspect(ctx, client, created.ID)
		if err != nil {
			return err
		}
		if helper == nil || !helper.State.Running {
			return errors.New("proposed static proxy configuration did not start")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return errors.New("proposed proxy configuration did not finish loading before the validation deadline")
}
