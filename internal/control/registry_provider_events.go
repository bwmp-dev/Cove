package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func registryWebhookAuthenticated(provider string, secret, body []byte, r *http.Request) bool {
	if provider == "ghcr" {
		header := r.Header.Get("X-Hub-Signature-256")
		provided, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		return err == nil && strings.HasPrefix(header, "sha256=") && hmac.Equal(provided, mac.Sum(nil))
	}
	token := bearer(r)
	if provider == "docker_hub" {
		queryToken := r.URL.Query().Get("secret")
		// The native sender accepts a URL only; do not pass its credential to later logging middleware.
		r.URL.RawQuery = ""
		if token == "" {
			token = queryToken
		}
	}
	return len(token) == 43 && subtle.ConstantTimeCompare(secret, []byte(token)) == 1
}

func registryEventKey(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func registryProviderEvents(provider string, body []byte, r *http.Request, now time.Time) ([]registryPushEvent, error) {
	switch provider {
	case "distribution":
		return parseRegistryPushEvents(body)
	case "docker_hub":
		var event struct {
			Push struct {
				At  int64  `json:"pushed_at"`
				Tag string `json:"tag"`
			} `json:"push_data"`
			Repository struct {
				Name string `json:"repo_name"`
			} `json:"repository"`
		}
		if json.Unmarshal(body, &event) != nil || event.Push.At <= 0 || event.Push.At < now.Add(-7*24*time.Hour).Unix() || event.Push.At > now.Add(10*time.Minute).Unix() {
			return nil, errors.New("use a Docker Hub push event from the last seven days")
		}
		key := registryEventKey("docker_hub\x00" + event.Repository.Name + "\x00" + event.Push.Tag + "\x00" + strconv.FormatInt(event.Push.At, 10))
		return validatedProviderEvent(registryPushEvent{Key: key, SourceKey: key, Repository: event.Repository.Name, Tag: event.Push.Tag})
	case "ghcr":
		if r.Header.Get("X-GitHub-Event") == "ping" {
			return []registryPushEvent{}, nil
		}
		kind, delivery := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
		if kind != "package" && kind != "registry_package" || len(delivery) == 0 || len(delivery) > 100 || strings.ContainsAny(delivery, "\x00\r\n") {
			return nil, errors.New("use a signed package or registry_package delivery with its delivery ID")
		}
		type packageEvent struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
			Type      string `json:"package_type"`
			Owner     struct {
				Login string `json:"login"`
			} `json:"owner"`
			Version struct {
				ID        int64 `json:"id"`
				Container struct {
					Tag struct {
						Name string `json:"name"`
					} `json:"tag"`
				} `json:"container_metadata"`
			} `json:"package_version"`
		}
		var event struct {
			Action   string        `json:"action"`
			Package  *packageEvent `json:"package"`
			Registry *packageEvent `json:"registry_package"`
		}
		if json.Unmarshal(body, &event) != nil {
			return nil, errors.New("invalid GitHub package notification")
		}
		if event.Action != "published" && event.Action != "updated" {
			return []registryPushEvent{}, nil
		}
		p := event.Package
		if kind == "registry_package" {
			p = event.Registry
		}
		if p == nil {
			return nil, errors.New("GitHub package payload does not match its event header")
		}
		ecosystem := strings.ToLower(p.Ecosystem)
		if ecosystem != "docker" && ecosystem != "container" && strings.ToLower(p.Type) != "container" || p.Version.Container.Tag.Name == "" {
			return []registryPushEvent{}, nil
		}
		if p.ID <= 0 || p.Version.ID <= 0 || p.Owner.Login == "" || p.Name == "" {
			return nil, errors.New("GitHub container package identity is missing")
		}
		// Delivery headers are not signed. Bind replay protection to the signed bytes, including version updates.
		source := registryEventKey("ghcr\x00" + string(body))
		return validatedProviderEvent(registryPushEvent{Key: registryEventKey(delivery), SourceKey: source, Repository: strings.ToLower(p.Owner.Login + "/" + p.Name), Tag: p.Version.Container.Tag.Name})
	default:
		return nil, errors.New("unsupported registry webhook provider")
	}
}

func validatedProviderEvent(event registryPushEvent) ([]registryPushEvent, error) {
	if err := validateRegistryWebhookFilter(registryWebhookFilter{Repository: event.Repository, Tag: event.Tag}); err != nil || event.Tag == "*" {
		return nil, errors.New("registry notification repository or tag is invalid")
	}
	return []registryPushEvent{event}, nil
}
