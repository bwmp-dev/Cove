package worker

import (
	"errors"
	"strings"
)

func ValidatePreviewRegistryServer(server string) error {
	if ValidateRegistry(&Registry{Server: server, Username: "validation", Password: "validation"}) != nil {
		return errors.New("CI images require a valid connected registry")
	}
	// Docker treats lowercase single-label prefixes as Docker Hub namespaces.
	if server != "localhost" && !strings.ContainsAny(server, ".:") && strings.ToLower(server) == server {
		return errors.New("CI images require a registry hostname with a dot or port, or localhost; a lowercase single-label prefix would select Docker Hub")
	}
	return nil
}

func ValidatePreviewRegistryImages(pins map[string]string, server string) error {
	if err := ValidatePreviewRegistryServer(server); err != nil {
		return err
	}
	if len(pins) == 0 {
		return errors.New("CI preview images require a connected registry and complete service digests")
	}
	if err := validateImagePins(pins); err != nil {
		return errors.New("CI preview images require valid service names and immutable image digests")
	}
	for _, image := range pins {
		repository, digest, ok := strings.Cut(strings.TrimPrefix(image, server+"/"), "@")
		if !strings.HasPrefix(image, server+"/") || !ok || !imageDigest.MatchString(digest) || ValidateRegistryRepository(repository) != nil {
			return errors.New("CI preview images must be registry/repository@sha256 digests on the resource's connected registry, without tags")
		}
	}
	return nil
}
