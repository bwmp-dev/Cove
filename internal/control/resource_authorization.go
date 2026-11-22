package control

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

var errRequestBusy = errors.New("another change is being checked; reload and try again")
var errGuardCapacity = errors.New("safe resource changes require at least three database pool connections")

var requestGuardInit sync.Mutex

func (s *Server) requestGuard(ctx context.Context, key string, shared bool) (func(), error) {
	if s.DB.Config().MaxConns < 3 {
		return nil, errGuardCapacity
	}
	requestGuardInit.Lock()
	if s.requestGuardSlots == nil {
		s.requestGuardSlots = make(chan struct{}, max(1, int(s.DB.Config().MaxConns)/3))
	}
	slots := s.requestGuardSlots
	requestGuardInit.Unlock()
	// Leave connections for the handler transaction and its access checks.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errRequestBusy
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		<-slots
		return nil, err
	}
	query := `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`
	if shared {
		query = `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,0))`
	}
	var locked bool
	err = tx.QueryRow(ctx, query, key).Scan(&locked)
	release := func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.Error("request guard could not be released")
		}
		<-slots
	}
	if err != nil {
		release()
		return nil, err
	}
	if !locked {
		release()
		return nil, errRequestBusy
	}
	return release, nil
}

func (s *Server) serveAuthorized(next http.Handler, w http.ResponseWriter, r *http.Request, name string) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != "GET" && strings.HasPrefix(r.URL.Path, "/api/workloads/") {
		release, err := s.requestGuard(r.Context(), "cove-resource-access:"+r.PathValue("id"), !strings.HasSuffix(r.URL.Path, "/move"))
		if err != nil {
			status, message := 503, "resource authorization checks unavailable"
			if errors.Is(err, errRequestBusy) {
				status, message = 409, errRequestBusy.Error()
			} else if errors.Is(err, errGuardCapacity) {
				message = errGuardCapacity.Error()
			}
			http.Error(w, message, status)
			s.recordAudit(r, name, status)
			return
		}
		defer release()
		// A move can commit between the first access check and acquiring the guard.
		if !s.authorize(w, r) {
			return
		}
	}
	s.authenticated(next, w, r, name)
}
