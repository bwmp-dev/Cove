package worker

import (
	"context"
	"errors"
	"time"
)

func guardPITRStage(parent context.Context, limit int64) (context.Context, func() error, error) {
	initial, err := freeBytes("/restore")
	if err != nil || initial < limit+(256<<20) {
		return nil, nil, errors.New("PITR recovery stage has insufficient free space")
	}
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				free, err := freeBytes("/restore")
				if err != nil || free < 256<<20 || initial-free > limit {
					cancel(errors.New("PITR recovery stage reached its byte limit or free-space reserve"))
					return
				}
			}
		}
	}()
	return ctx, func() error {
		cause := context.Cause(ctx)
		cancel(context.Canceled)
		<-done
		if errors.Is(cause, context.Canceled) {
			return nil
		}
		return cause
	}, nil
}
