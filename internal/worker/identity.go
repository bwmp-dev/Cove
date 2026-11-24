package worker

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/ssh"
)

type IdentityRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Old    string `json:"old"`
	New    string `json:"new"`
}

type IdentityState struct {
	IdentityRequest
	State string `json:"state"`
}

func identityPublicKey(value string) (string, error) {
	if len(value) > 1024 {
		return "", errors.New("SSH public identity exceeds its size limit")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(value))
	if err != nil || key.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("one Ed25519 public identity required")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), nil
}

func identityLine(key string) string {
	return `restrict,command="/usr/local/bin/cove-worker rpc" ` + key
}

func (e *Engine) ChangeIdentity(request IdentityRequest) (*IdentityState, error) {
	if runtime.GOOS != "linux" || !snapshotOwnerID.MatchString(request.ID) || request.Action != "stage" && request.Action != "commit" && request.Action != "abort" {
		return nil, errors.New("invalid SSH identity change")
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	old, err := identityPublicKey(request.Old)
	if err != nil {
		return nil, err
	}
	newKey, err := identityPublicKey(request.New)
	if err != nil || old == newKey {
		return nil, errors.New("a different valid SSH identity is required")
	}
	request.Old, request.New = old, newKey
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.draining && request.Action == "stage" {
		return nil, errors.New("wait for worker maintenance before identity rotation")
	}
	parent := filepath.Join(e.Root, ".ssh")
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("worker SSH directory unavailable")
	}
	filename := filepath.Join(parent, "authorized_keys")
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("worker authorized identities unavailable or oversized")
	}
	input, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	closeErr := input.Close()
	if err != nil || closeErr != nil || len(data) > 64<<10 {
		return nil, errors.New("worker authorized identities unavailable or oversized")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	oldCount, newCount := 0, 0
	for _, line := range lines {
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(line))
		if parseErr != nil {
			continue
		}
		canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if canonical == old {
			if strings.TrimSpace(line) != identityLine(old) {
				return nil, errors.New("existing worker identity has unexpected restrictions; reconcile it before rotating")
			}
			oldCount++
		}
		if canonical == newKey {
			if strings.TrimSpace(line) != identityLine(newKey) {
				return nil, errors.New("new identity already exists with different restrictions")
			}
			newCount++
		}
	}
	if oldCount > 1 || newCount > 1 {
		return nil, errors.New("duplicate worker identities require reconciliation")
	}
	stateFile := filepath.Join(e.Root, "ssh-rotation.json")
	var saved IdentityState
	err = readSnapshotMetadata(stateFile, &saved)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && (saved.State != "staged" && saved.State != "committed" && saved.State != "aborted" || !snapshotOwnerID.MatchString(saved.ID)) {
		return nil, errors.New("invalid saved SSH rotation state")
	}
	if err == nil && saved.State == "staged" && saved.ID != request.ID {
		return nil, errors.New("another SSH identity rotation is unfinished")
	}
	if err == nil && saved.ID == request.ID && (saved.Old != old || saved.New != newKey) {
		return nil, errors.New("SSH rotation input differs from its saved intent")
	}
	stageFile := filename + ".cove-stage"
	stageInfo, stageErr := os.Lstat(stageFile)
	if stageErr == nil {
		uid, gid := fileOwner(info)
		stageUID, stageGID := fileOwner(stageInfo)
		if saved.ID == "" || !stageInfo.Mode().IsRegular() || stageInfo.Size() > 64<<10 || stageUID != uid || stageGID != gid || stageInfo.Mode().Perm() != 0600 {
			return nil, errors.New("unexpected SSH identity staging file requires inspection")
		}
		if err = os.Remove(stageFile); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(stageErr) {
		return nil, stageErr
	}
	state := IdentityState{IdentityRequest: request, State: "staged"}
	switch request.Action {
	case "stage":
		if saved.ID == request.ID && saved.State == "aborted" {
			return nil, errors.New("cancelled SSH rotations cannot be replayed")
		}
		if saved.ID == request.ID && saved.State == "committed" && newCount == 1 {
			return &saved, nil
		}
		if oldCount != 1 {
			return nil, errors.New("current restricted worker identity is missing")
		}
		if saved.ID != request.ID && newCount != 0 {
			return nil, errors.New("new identity is already present")
		}
		if err = writeJSON(stateFile, state); err != nil {
			return nil, err
		}
		if newCount == 0 {
			lines = append(lines, identityLine(newKey))
		}
	case "commit":
		if saved.ID != request.ID || saved.State == "aborted" || newCount != 1 {
			return nil, errors.New("new SSH identity has not been staged")
		}
		lines = removeIdentityLine(lines, identityLine(old))
		state.State = "committed"
	case "abort":
		if saved.ID == request.ID && saved.State == "committed" {
			return nil, errors.New("committed SSH identities cannot be rolled back")
		}
		if oldCount != 1 {
			return nil, errors.New("old identity must remain available before aborting")
		}
		if saved.ID != request.ID && newCount != 0 {
			return nil, errors.New("cannot remove an identity without its matching rotation intent")
		}
		lines = removeIdentityLine(lines, identityLine(newKey))
		state.State = "aborted"
	}
	updated := []byte(strings.Join(lines, "\n") + "\n")
	if len(updated) > 64<<10 {
		return nil, errors.New("authorized identity size limit reached")
	}
	if !bytes.Equal(data, updated) {
		if err = replaceIdentityFile(filename, info, updated); err != nil {
			return nil, err
		}
	}
	if err = writeJSON(stateFile, state); err != nil {
		return nil, err
	}
	return &state, nil
}

func removeIdentityLine(lines []string, target string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != target {
			out = append(out, line)
		}
	}
	return out
}

func replaceIdentityFile(filename string, info os.FileInfo, data []byte) (result error) {
	file, err := os.OpenFile(filename+".cove-stage", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() {
		file.Close()
		if err := os.Remove(file.Name()); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}()
	uid, gid := fileOwner(info)
	if err = file.Chown(int(uid), int(gid)); err == nil {
		err = file.Chmod(0600)
	}
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filename); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(filename))
}
