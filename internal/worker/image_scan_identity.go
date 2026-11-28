package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

func scanImageIdentity(image, platform string) string {
	digest := sha256.Sum256([]byte(image + "\n" + platform))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (e *Engine) scanPlatform(ctx context.Context, container string) string {
	var descriptor struct{ Digest string }
	if err := e.imageJSON(ctx, nil, &descriptor, "docker", "inspect", "--format", "{{json .ImageManifestDescriptor}}", container); err != nil || !imageDigest.MatchString(descriptor.Digest) {
		return ""
	}
	return descriptor.Digest
}
