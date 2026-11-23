package worker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func (s workloadSuspension) idleEpisode() string {
	if s.IdleEpisode != "" {
		return s.IdleEpisode
	}
	return s.Operation
}

func (e *Engine) startIdleWake(ctx context.Context) (func(), error) {
	if runtime.GOOS != "linux" {
		return func() {}, nil
	}
	dir := filepath.Join(e.Root, "idle-wake")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "worker.sock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("idle wake socket path is not a socket")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	address, closeAddress, err := idleSocketAddress(path)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", address)
	if err != nil {
		closeAddress()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		closeAddress()
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/health" {
			w.WriteHeader(204)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/wake" {
			http.NotFound(w, r)
			return
		}
		var input struct {
			Workload string `json:"workload"`
			Episode  string `json:"episode"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || !workloadIdentifier.MatchString(input.Workload) || !identifier.MatchString(input.Episode) {
			http.Error(w, "Invalid wake request", 400)
			return
		}
		state, err := e.readIdle(input.Workload)
		if err != nil || state == nil || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+state.Config.Token)) != 1 {
			http.Error(w, "Wake unavailable", 403)
			return
		}
		wake, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		if err = e.wakeIdleEpisode(wake, input.Workload, input.Episode); err != nil {
			http.Error(w, "Wake readiness was not confirmed", 503)
			return
		}
		w.WriteHeader(204)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 3*time.Minute + 5*time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			select {
			case e.fatal <- err:
			default:
			}
		}
	}()
	return func() { server.Close(); listener.Close(); closeAddress() }, nil
}

func (e *Engine) wakeIdleEpisode(ctx context.Context, workload, episode string) error {
	if !strings.HasPrefix(episode, "idle-") {
		return errors.New("not an idle episode")
	}
	// A request can arrive after quiesce but before the sleep snapshot has reached disk.
	ready := time.Now().Add(10 * time.Second)
	for {
		suspension, err := e.readSuspension(workload)
		if err != nil {
			return err
		}
		if suspension != nil {
			if suspension.Options.Reason != "idle" || suspension.ManualStop || suspension.idleEpisode() != episode {
				return errors.New("resource is held by another operation")
			}
			break
		}
		if time.Now().After(ready) {
			op, err := e.Status(episode)
			if err != nil || op.Workload != workload || op.Action != "idle_sleep" || op.State != "queued" && op.State != "running" {
				return errors.New("sleep snapshot was not confirmed")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	op, err := e.Submit(Request{ID: "wake-" + episode, Workload: workload, Action: "idle_wake"})
	if err != nil {
		return err
	}
	for {
		if op.State == "succeeded" {
			status, err := e.IdleStatus(ctx, workload)
			if err != nil || status == nil || status.Phase != "awake" {
				return errors.New("wake operation did not confirm request readiness")
			}
			return nil
		}
		if op.State != "queued" && op.State != "running" {
			return errors.New("this episode's wake failed; start the resource explicitly after repair")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		op, err = e.Status(op.ID)
		if err != nil {
			return err
		}
	}
}

func (e *Engine) idleAvailableLocked(workload string) bool {
	if e.draining || e.busy(workload) {
		return false
	}
	background := 0
	for _, op := range e.pending {
		if op.Workload == workload {
			return false
		}
		if op.Action == "idle_sleep" || op.Action == "idle_wake" {
			background++
		}
	}
	return background < 4
}

func (e *Engine) sleepIdleWorkload(ctx context.Context, workload string) error {
	e.mu.Lock()
	available := e.idleAvailableLocked(workload)
	e.mu.Unlock()
	if !available {
		return nil
	}
	state, err := e.readIdle(workload)
	if err != nil || state == nil {
		return err
	}
	suspension, err := e.readSuspension(workload)
	if err != nil || suspension != nil {
		return err
	}
	if err = e.idleEligibility(ctx, workload); err != nil {
		return err
	}
	token, err := idleToken()
	if err != nil {
		return err
	}
	episode := "idle-" + token[:32]
	code, err := e.idleControl(ctx, workload, "POST", "/quiesce", map[string]any{"seconds": state.Policy.normalized().AfterSeconds, "episode": episode}, nil)
	if err != nil {
		return err
	}
	if code == 409 {
		var gateway idleGatewayState
		if status, readErr := e.idleControl(ctx, workload, "GET", "/status", nil, &gateway); readErr != nil || status != 200 {
			return errors.New("could not inspect idle quiesce state")
		}
		if gateway.Mode == "quiescing" {
			op, readErr := e.Status(gateway.Episode)
			if readErr != nil && !os.IsNotExist(readErr) {
				return readErr
			}
			if readErr != nil || op.State != "queued" && op.State != "running" {
				current, released, err := readCurrent(filepath.Join(e.Root, "workloads", workload, "current.json"))
				if err != nil {
					return err
				}
				if released {
					_, err = e.refreshIdleGateway(ctx, workload, current.Routes)
					return err
				}
			}
		}
		return nil
	}
	if code != 204 {
		return errors.New("could not quiesce the request gateway")
	}
	e.mu.Lock()
	currentPolicy, readErr := e.readIdle(workload)
	if readErr != nil {
		err = readErr
	} else if currentPolicy == nil || currentPolicy.Policy != state.Policy || currentPolicy.Config.Token != state.Config.Token {
		err = errors.New("idle policy changed before sleep")
	} else if e.idleAvailableLocked(workload) {
		_, err = e.submitLocked(Request{ID: episode, Workload: workload, Action: "idle_sleep"})
	} else {
		err = errors.New("resource became busy before sleep")
	}
	e.mu.Unlock()
	if err != nil {
		current, released, readErr := readCurrent(filepath.Join(e.Root, "workloads", workload, "current.json"))
		if readErr != nil || !released {
			return errors.Join(err, errors.New("idle quiesce rollback needs a deployed release"))
		}
		if _, restoreErr := e.refreshIdleGateway(ctx, workload, current.Routes); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
	}
	return err
}

func (e *Engine) idleLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		entries, err := os.ReadDir(filepath.Join(e.Root, "workloads"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Idle resource scan failed:", err)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !workloadIdentifier.MatchString(entry.Name()) {
				continue
			}
			check, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := e.sleepIdleWorkload(check, entry.Name())
			cancel()
			e.idleMu.Lock()
			e.mu.Lock()
			if e.idleAvailableLocked(entry.Name()) {
				state, readErr := e.readIdle(entry.Name())
				if readErr == nil && state != nil {
					message := ""
					if err != nil {
						message = err.Error()
					}
					if state.LastError != message {
						state.LastError = message
						if writeErr := e.writeIdle(entry.Name(), state); writeErr != nil {
							err = errors.Join(err, writeErr)
						}
					}
				}
			}
			e.mu.Unlock()
			e.idleMu.Unlock()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Idle sleep check failed for", entry.Name()+":", err)
			}
		}
	}
}
