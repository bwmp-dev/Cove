package worker

import (
	"context"
	"errors"
	"syscall"
	"time"
)

func lockPITRArchive(ctx context.Context, directory string) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		unlock, err := lockWorker(directory)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("PITR archive is busy; retry after its current retention or recovery pass")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
