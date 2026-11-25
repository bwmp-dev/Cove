package worker

import (
	"context"
	"cove/internal/outbound"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type ImageUpdateOptions struct {
	Generation    string            `json:"generation"`
	Configuration string            `json:"configuration"`
	Policy        ImageUpdatePolicy `json:"policy"`
	Images        map[string]string `json:"images"`
}

type ImageUpdateCandidate struct {
	Service string `json:"service"`
	Current string `json:"current"`
	Target  string `json:"target"`
}

type ImageUpdateResult struct {
	State      string                 `json:"state"`
	Message    string                 `json:"message,omitempty"`
	Candidates []ImageUpdateCandidate `json:"candidates"`
	Pins       map[string]string      `json:"pins,omitempty"`
	Key        string                 `json:"key,omitempty"`
}

func (o ImageUpdateOptions) Validate() error {
	if err := o.Policy.Validate(); err != nil {
		return err
	}
	if err := ValidateImageUpdateImages(o.Images); err != nil {
		return err
	}
	for _, rule := range o.Policy.Rules {
		if o.Images[rule.Service] == "" || strings.Contains(o.Images[rule.Service], "@") {
			return errors.New("selected image update services require mutable references")
		}
	}
	return nil
}

func ValidateImageUpdateImages(images map[string]string) error {
	if len(images) == 0 || len(images) > 64 {
		return errors.New("image updates require complete service image inputs")
	}
	for service, image := range images {
		if !serviceIdentifier.MatchString(service) || image == "" || len(image) > 512 || strings.ContainsAny(image, " \t\r\n\x00$") {
			return errors.New("image updates require explicit service image references")
		}
	}
	return nil
}

func imageReferenceTag(reference string) string {
	reference, _, _ = strings.Cut(reference, "@")
	if i := strings.LastIndex(reference, ":"); i > strings.LastIndex(reference, "/") {
		return reference[i+1:]
	}
	return "latest"
}

func (e *Engine) checkImageUpdates(ctx context.Context, options ImageUpdateOptions, images []ImageStatus, registry *Registry, env []string, selected ...*outbound.Config) ImageUpdateResult {
	unknown := func(message string) ImageUpdateResult {
		return ImageUpdateResult{State: "unknown", Message: message, Candidates: []ImageUpdateCandidate{}}
	}
	if err := options.Validate(); err != nil {
		return unknown(err.Error())
	}
	deployed := map[string]ImageStatus{}
	pins := map[string]string{}
	for _, image := range images {
		if previous, ok := deployed[image.Service]; ok && (previous.ImageID != image.ImageID || previous.Reference != image.Reference) {
			return unknown("A service has mixed deployed images; finish its deployment before checking updates.")
		}
		deployed[image.Service] = image
		pins[image.Service] = image.ImageID
	}
	if len(deployed) != len(options.Images) {
		return unknown("Not every configured service has a retained deployed image.")
	}
	for service := range options.Images {
		if !imageDigest.MatchString(pins[service]) {
			return unknown("A configured service has no verified deployed image identity.")
		}
	}
	result := ImageUpdateResult{State: "current", Candidates: []ImageUpdateCandidate{}}
	proxy := outbound.Environment()
	if len(selected) > 0 && selected[0] != nil {
		proxy = *selected[0]
	}
	client := newRegistryTagClient(proxy)
	for _, rule := range options.Policy.Rules {
		current := deployed[rule.Service]
		reference := options.Images[rule.Service]
		if imageRepository(current.Reference) != imageRepository(reference) {
			return unknown("The deployed repository differs from the saved configuration; deploy it explicitly first.")
		}
		if rule.Range != "" {
			tags, err := client.tags(ctx, imageRepository(reference), registry)
			if err != nil {
				return unknown(err.Error())
			}
			tag, err := selectImageUpdateTag(imageReferenceTag(current.Reference), rule.Range, tags)
			if err != nil {
				return unknown(err.Error())
			}
			if tag == "" {
				continue
			}
			reference = imageRepository(reference) + ":" + tag
		}
		status := e.checkImage(ctx, ImageStatus{Service: rule.Service, Reference: reference, ImageID: current.ImageID}, env)
		if status.Status != "current" && status.Status != "update_available" {
			return unknown("A candidate image could not be verified for the deployed platform.")
		}
		if status.Status == "current" {
			continue
		}
		if !imageDigest.MatchString(status.RegistryDigest) {
			return unknown("A candidate image has no immutable registry digest.")
		}
		target := reference + "@" + status.RegistryDigest
		pins[rule.Service] = target
		result.Candidates = append(result.Candidates, ImageUpdateCandidate{Service: rule.Service, Current: current.Reference, Target: target})
	}
	if len(result.Candidates) > 0 {
		result.State = "available"
		result.Pins = pins
		targets := map[string]string{}
		for _, candidate := range result.Candidates {
			targets[candidate.Service] = candidate.Target
		}
		data, _ := json.Marshal(targets)
		sum := sha256.Sum256(data)
		result.Key = hex.EncodeToString(sum[:])
	}
	return result
}
