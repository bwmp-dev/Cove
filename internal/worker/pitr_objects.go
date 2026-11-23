package worker

import (
	"context"
	"errors"
	"strings"
)

func (e *Engine) pitrContainerID(ctx context.Context, name string) (string, error) {
	value, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$")
	if err != nil {
		return "", errors.New("Docker container inventory is unavailable")
	}
	ids := strings.Fields(value)
	if len(ids) > 1 {
		return "", errors.New("ambiguous PITR helper identity")
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

func (e *Engine) pitrVolumeExists(ctx context.Context, name string) (bool, error) {
	value, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "name=^"+name+"$")
	if err != nil {
		return false, errors.New("Docker volume inventory is unavailable")
	}
	names := strings.Fields(value)
	if len(names) > 1 || len(names) == 1 && names[0] != name {
		return false, errors.New("ambiguous PITR volume identity")
	}
	return len(names) == 1, nil
}
