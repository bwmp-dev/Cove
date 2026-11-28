package control

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func logPeriodicError(ctx context.Context, task string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	code := "unavailable"
	var database *pgconn.PgError
	if errors.As(err, &database) {
		code = database.Code
	}
	// Database details and remote errors can contain configuration or credentials.
	log.Printf("periodic task %s failed (%s); retrying on its next tick", task, code)
}

func retryBackgroundLoop(ctx context.Context, task string, run func(context.Context) error) error {
	for ctx.Err() == nil {
		err := run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("background loop stopped")
		}
		logPeriodicError(ctx, task, err)
		timer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
