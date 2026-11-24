package worker

import (
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const ManagedRegistryImage = "docker.io/library/registry:3.1.2@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c"

var managedRegistryHash = regexp.MustCompile(`^\$2[aby]\$1[0-4]\$[./A-Za-z0-9]{53}$`)

type ManagedRegistryConfig struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Hash     string `json:"hash"`
}

// The generated workload uses ordinary deployment recovery, file secrets and route claims.
// The native registry auth file stays out of Compose and route history.
func (c ManagedRegistryConfig) Deployment(operation string) (Request, error) {
	if _, err := netip.ParseAddr(c.Hostname); err == nil {
		return Request{}, errors.New("managed registry requires a DNS hostname, not an IP address")
	}
	if !certificateIdentifier.MatchString(c.ID) || !identifier.MatchString(operation) || !environmentIdentifier.MatchString(c.Username) || len(c.Username) > 32 || strings.Contains(c.Hostname, "*") {
		return Request{}, errors.New("invalid managed registry identity")
	}
	cost, err := bcrypt.Cost([]byte(c.Hash))
	if err != nil || cost < 10 || cost > 14 || !managedRegistryHash.MatchString(c.Hash) {
		return Request{}, errors.New("managed registry requires a bcrypt credential with cost 10 to 14")
	}
	routes := []Route{{Domain: c.Hostname, Service: "registry", Port: 5000}}
	if err = ValidateRoutes(routes, "service"); err != nil {
		return Request{}, err
	}
	config := map[string]any{
		"services": map[string]any{"registry": map[string]any{
			"image":        ManagedRegistryImage,
			"restart":      "unless-stopped",
			"read_only":    true,
			"cap_drop":     []string{"ALL"},
			"security_opt": []string{"no-new-privileges:true"},
			"pids_limit":   128,
			"mem_limit":    "512m",
			"cpus":         1,
			"tmpfs":        []string{"/tmp:rw,noexec,nosuid,size=16777216"},
			"volumes":      []string{"data:/var/lib/registry"},
			"environment": map[string]string{
				"REGISTRY_AUTH":                   "htpasswd",
				"REGISTRY_LOG_LEVEL":              "warn",
				"REGISTRY_LOG_ACCESSLOG_DISABLED": "true",
				"REGISTRY_AUTH_HTPASSWD_REALM":    "Cove registry",
				"REGISTRY_AUTH_HTPASSWD_PATH":     "/run/secrets/registry_auth",
				"REGISTRY_STORAGE_DELETE_ENABLED": "false",
				"OTEL_TRACES_EXPORTER":            "none",
				"OTEL_METRICS_EXPORTER":           "none",
				"OTEL_LOGS_EXPORTER":              "none",
			},
			"labels":      map[string]string{"dev.cove.managed-registry": c.ID},
			"logging":     map[string]any{"driver": "local", "options": map[string]string{"max-size": "10m", "max-file": "3"}},
			"healthcheck": map[string]any{"test": []string{"CMD-SHELL", "wget -S --spider http://127.0.0.1:5000/v2/ 2>&1 | grep -q 'HTTP/1.1 401'"}, "interval": "10s", "timeout": "2s", "retries": 3, "start_period": "2s"},
		}},
		"volumes": map[string]any{"data": map[string]any{"labels": map[string]string{"dev.cove.managed-registry": c.ID}}},
	}
	compose, err := json.Marshal(config)
	if err != nil {
		return Request{}, err
	}
	r := Request{ID: operation, Workload: c.ID, Action: "deploy", Kind: "service", Compose: string(compose), Routes: routes,
		Environment: map[string]string{"COVE_REGISTRY_AUTH": c.Username + ":" + c.Hash + "\n"},
		FileSecrets: []FileSecret{{Name: "registry_auth", Variable: "COVE_REGISTRY_AUTH", Services: []string{"registry"}}},
	}
	return r, ValidateRequest(r)
}

type ManagedRegistryStorage struct {
	HardCapVerified bool   `json:"hard_cap_verified"`
	Reason          string `json:"reason"`
}

func ManagedRegistryStorageSupport() ManagedRegistryStorage {
	return ManagedRegistryStorage{Reason: "No hard cap on this storage: the managed Docker volume has no verified filesystem quota. Retention and garbage collection do not impose a hard storage cap."}
}
