package worker

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxRecipeInputBytes int64 = 1 << 30
const maxRecipeInputEntries = 100000

func recipeDirectory(checkout, relative string) (string, error) {
	if !recipeRelativePath(relative) {
		return "", errors.New("build recipe paths must stay inside the checkout")
	}
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", errors.New("build recipe checkout is unavailable")
	}
	selected, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return "", errors.New("selected build recipe directory is unavailable")
	}
	within, err := filepath.Rel(root, selected)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", errors.New("selected build recipe directory escapes the checkout")
	}
	info, err := os.Stat(selected)
	if err != nil || !info.IsDir() {
		return "", errors.New("select an existing build recipe directory")
	}
	return selected, nil
}

func copyRecipeTree(ctx context.Context, source, target string, public bool) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	var total int64
	entries, files := 0, 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, failure error) error {
		if failure != nil {
			return failure
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name != "." && (entry.Name() == ".git" || strings.HasPrefix(entry.Name(), ".cove-compose-") || public && strings.HasPrefix(entry.Name(), ".")) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxRecipeInputEntries {
			return errors.New("build recipe inputs exceed 100000 entries")
		}
		destination := filepath.Join(target, filepath.FromSlash(name))
		if entry.IsDir() {
			if err := os.MkdirAll(destination, 0700); err != nil {
				return err
			}
			return os.Chmod(destination, 0755)
		}
		if !entry.Type().IsRegular() {
			return errors.New("build recipe inputs cannot contain symlinks or special files")
		}
		input, err := root.Open(name)
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxRecipeInputBytes-total {
			return errors.New("build recipe inputs exceed the 1 GiB limit or changed while copying")
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		count, copyErr := io.Copy(output, io.LimitReader(input, maxRecipeInputBytes-total+1))
		mode := os.FileMode(0644)
		if !public && info.Mode()&0111 != 0 {
			mode = 0755
		}
		modeErr := output.Chmod(mode)
		closeErr := output.Close()
		if copyErr != nil || modeErr != nil || closeErr != nil {
			return errors.Join(copyErr, modeErr, closeErr)
		}
		total += count
		if total > maxRecipeInputBytes {
			return errors.New("build recipe inputs exceed the 1 GiB limit")
		}
		files++
		return nil
	})
	if err == nil && files == 0 {
		return errors.New("the selected build recipe directory has no usable files")
	}
	return err
}
