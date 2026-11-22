package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SourceFiles struct {
	Files    map[string]string `json:"files"`
	Revision string            `json:"revision"`
}

func (e *Engine) ReadSourceFiles(ctx context.Context, request Request) (result *SourceFiles, resultErr error) {
	if request.Source == nil || request.Source.InlineCompose || ValidateRequest(request) != nil {
		return nil, errors.New("a valid repository file selection is required")
	}
	if !e.preflightMu.TryLock() {
		return nil, errors.New("another source inspection is running; try again when it completes")
	}
	defer e.preflightMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	directory, err := os.MkdirTemp(e.Root, "source-files-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(directory); err != nil {
			result, resultErr = nil, errors.Join(resultErr, errors.New("could not clean up private source inspection"))
		}
	}()
	primary, revision, err := e.checkout(ctx, request, directory, io.Discard)
	if err != nil {
		return nil, errors.New("could not fetch selected repository files")
	}
	paths := map[string]string{request.Source.ComposePath: primary}
	root := filepath.Join(directory, "releases", request.ID)
	if request.ComposeBundle != nil {
		for _, name := range request.ComposeBundle.Paths {
			paths[name] = filepath.Join(root, filepath.FromSlash(name))
		}
	}
	result = &SourceFiles{Files: map[string]string{}, Revision: revision}
	for name, candidate := range paths {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return nil, errors.New("selected repository file is unavailable")
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New("selected repository file escapes its checkout")
		}
		file, err := os.Open(candidate)
		if err != nil {
			return nil, errors.New("selected repository file is unavailable")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > 1<<20 || strings.ContainsRune(string(data), 0) {
			return nil, errors.New("selected repository files must contain at most 1 MiB of text each")
		}
		result.Files[name] = string(data)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > (2<<20)-(16<<10) {
		return nil, errors.New("selected repository files exceed the worker response limit")
	}
	return result, nil
}
