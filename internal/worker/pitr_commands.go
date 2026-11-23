package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

const pitrWALGFile = "/tmp/pitr/config.json"

func pitrToolCommand(ctx context.Context, arguments []string, environment []string) *exec.Cmd {
	command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	configureCommand(command)
	command.Env = append(os.Environ(), environment...)
	command.Dir = "/"
	command.Stderr = io.Discard
	return command
}

func pitrCommandOutput(ctx context.Context, arguments []string, environment []string, limit int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := pitrToolCommand(ctx, arguments, environment)
	output := &limitedBuffer{Limit: limit}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return nil, errors.New("PITR storage command failed")
	}
	return output.data.Bytes(), nil
}

func pitrWALG(ctx context.Context, arguments ...string) ([]byte, error) {
	if len(arguments) > 0 && (arguments[0] == "backup-push" || arguments[0] == "backup-fetch") {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		command := pitrToolCommand(ctx, append([]string{"wal-g", "--config", pitrWALGFile}, arguments...), nil)
		output := &limitedBuffer{Limit: pitrMetadataLimit}
		command.Stdout = output
		if err := command.Run(); err != nil {
			return nil, errors.New("PITR base transfer failed")
		}
		return output.data.Bytes(), nil
	}
	return pitrCommandOutput(ctx, append([]string{"wal-g", "--config", pitrWALGFile}, arguments...), nil, pitrMetadataLimit)
}

func pitrBaseInventory(ctx context.Context) ([]pitrStoredBase, error) {
	data, err := pitrWALG(ctx, "backup-list", "--json")
	if err != nil {
		return nil, err
	}
	var bases []pitrStoredBase
	if json.Unmarshal(data, &bases) != nil || len(bases) > 128 {
		return nil, errors.New("PITR base backup inventory is invalid or exceeds its limit")
	}
	return bases, nil
}

func pitrArchiveInventory(ctx context.Context, root string, config pitrRunnerConfig, environment []string) (pitrInventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	remote := strings.TrimSuffix(root, "/") + "/" + config.Policy.Archive
	command := pitrToolCommand(ctx, []string{"rclone", "lsjson", "--recursive", "--files-only", "--no-mimetype", remote}, environment)
	reader, err := command.StdoutPipe()
	if err != nil {
		return pitrInventory{}, err
	}
	if err := command.Start(); err != nil {
		return pitrInventory{}, errors.New("PITR archive inventory is unavailable")
	}
	inventory, readErr := readPITRInventory(reader)
	if readErr != nil {
		cancel()
	}
	waitErr := command.Wait()
	if readErr != nil {
		return inventory, readErr
	}
	if waitErr != nil {
		return inventory, errors.New("PITR archive inventory is unavailable")
	}
	return inventory, nil
}

func maintainPITRArchive(ctx context.Context, config pitrRunnerConfig, root string, environment []string) ([]pitrStoredBase, pitrInventory, error) {
	bases, err := pitrBaseInventory(ctx)
	if err != nil {
		return nil, pitrInventory{}, err
	}
	kept, err := retainedPITRBases(bases, config.Policy.Settings, time.Now().UTC())
	if err != nil {
		return nil, pitrInventory{}, err
	}
	if len(kept) > 0 && len(kept) < len(bases) {
		if _, err := pitrWALG(ctx, "delete", "before", "FIND_FULL", kept[len(kept)-1].Name, "--use-sentinel-time", "--confirm"); err != nil {
			return nil, pitrInventory{}, errors.New("PITR archive retention could not complete")
		}
	}
	inventory, err := pitrArchiveInventory(ctx, root, config, environment)
	if err != nil {
		return kept, inventory, err
	}
	for inventory.Bytes > config.Policy.Settings.MaxBytes && len(kept) > 1 {
		kept = kept[:len(kept)-1]
		if _, err := pitrWALG(ctx, "delete", "before", "FIND_FULL", kept[len(kept)-1].Name, "--use-sentinel-time", "--confirm"); err != nil {
			return kept, inventory, errors.New("PITR archive pressure retention could not complete")
		}
		inventory, err = pitrArchiveInventory(ctx, root, config, environment)
		if err != nil {
			return kept, inventory, err
		}
	}
	if inventory.Bytes > config.Policy.Settings.MaxBytes || inventory.Objects >= pitrObjectLimit {
		return kept, inventory, errors.New("PITR archive reached its byte or object limit while preserving the newest base backup")
	}
	return kept, inventory, nil
}

func pitrRelativeObject(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 1024 && !path.IsAbs(value) && path.Clean(value) == value && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\\x00\r\n")
}
