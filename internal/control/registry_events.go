package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type registryPushEvent struct {
	Key        string
	SourceKey  string
	Repository string
	Tag        string
}

func parseRegistryPushEvents(body []byte) ([]registryPushEvent, error) {
	if len(body) > 256*1024 {
		return nil, errors.New("registry notification exceeds 256 KiB")
	}
	var envelope struct {
		Events []struct {
			ID, Action string
			Target     struct{ MediaType, Repository, Tag string }
		}
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Events == nil || len(envelope.Events) > 64 {
		return nil, errors.New("use a registry notification with at most 64 events")
	}
	parsed := []registryPushEvent{}
	seen := map[string]registryPushEvent{}
	for _, event := range envelope.Events {
		if event.Action != "push" || event.Target.Tag == "" {
			continue
		}
		switch event.Target.MediaType {
		case "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json":
		default:
			continue
		}
		if event.ID == "" || len(event.ID) > 128 || event.Target.Repository == "" || len(event.Target.Repository) > 200 || len(event.Target.Tag) > 128 || strings.ContainsAny(event.ID+event.Target.Repository+event.Target.Tag, "\x00\r\n") {
			return nil, errors.New("registry notification identity or image name is invalid")
		}
		if _, err := validatedProviderEvent(registryPushEvent{Repository: event.Target.Repository, Tag: event.Target.Tag}); err != nil {
			return nil, err
		}
		digest := sha256.Sum256([]byte(event.ID))
		item := registryPushEvent{Key: hex.EncodeToString(digest[:]), SourceKey: hex.EncodeToString(digest[:]), Repository: event.Target.Repository, Tag: event.Target.Tag}
		if previous, ok := seen[item.Key]; ok {
			if previous != item {
				return nil, errors.New("registry event identity names conflicting images")
			}
			continue
		}
		seen[item.Key] = item
		parsed = append(parsed, item)
	}
	return parsed, nil
}
