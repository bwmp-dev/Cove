package worker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

func pitrArchiveOwner(config pitrRunnerConfig) string {
	key, _ := hex.DecodeString(config.Policy.Key)
	digest := hmac.New(sha256.New, key)
	digest.Write([]byte("cove PITR archive v1\x00" + config.Workload + "\x00" + config.Policy.Archive))
	return hex.EncodeToString(digest.Sum(nil))
}

func ensurePITRArchive(ctx context.Context, config pitrRunnerConfig, root string, environment []string) error {
	archive := strings.TrimSuffix(root, "/") + "/" + config.Policy.Archive
	if _, err := pitrCommandOutput(ctx, []string{"rclone", "mkdir", archive}, environment, 65536); err != nil {
		return errors.New("PITR archive location is not writable")
	}
	data, err := pitrCommandOutput(ctx, []string{"rclone", "lsjson", "--max-depth", "1", "--no-mimetype", archive}, environment, 65536)
	if err != nil {
		return errors.New("PITR archive ownership could not be inspected")
	}
	var entries []pitrObject
	if json.Unmarshal(data, &entries) != nil || len(entries) > 128 {
		return errors.New("PITR archive root is invalid or exceeds its limit")
	}
	marker := archive + "/.cove-pitr-owner"
	found := false
	for _, entry := range entries {
		if entry.Path == ".cove-pitr-owner" {
			if found || entry.IsDir || entry.Size != 64 {
				return errors.New("PITR archive owner marker is invalid")
			}
			found = true
		}
	}
	owner := pitrArchiveOwner(config)
	if found {
		data, err := pitrCommandOutput(ctx, []string{"rclone", "cat", marker}, environment, 65)
		if err != nil || !hmac.Equal(data, []byte(owner)) {
			return errors.New("PITR archive belongs to another policy")
		}
		return nil
	}
	if config.RequireArchive {
		return errors.New("the retained PITR archive owner marker is missing; verify the storage credentials and destination")
	}
	if len(entries) != 0 {
		return errors.New("refusing an existing unowned PITR archive")
	}
	command := pitrToolCommand(ctx, []string{"rclone", "rcat", marker}, environment)
	command.Stdin = strings.NewReader(owner)
	if err := command.Run(); err != nil {
		return errors.New("could not establish PITR archive ownership")
	}
	data, err = pitrCommandOutput(ctx, []string{"rclone", "cat", marker}, environment, 65)
	if err != nil || !hmac.Equal(data, []byte(owner)) {
		return errors.New("PITR archive owner marker could not be verified")
	}
	return nil
}
