package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// treeDigest summarises a directory tree: relative paths, types, permissions, ownership, sizes and
// contents. Modification times are excluded because copying a directory's children updates them.
type treeDigest struct {
	Files  int64
	Bytes  int64
	Digest string
}

func digestTree(root string) (treeDigest, error) {
	var result treeDigest
	hash := sha256.New()
	err := walkSorted(root, func(relative string, info fs.FileInfo) error {
		mode := info.Mode()
		uid, gid := fileOwner(info)
		line := fmt.Sprintf("%s\x00%s\x00%o\x00%d\x00%d", relative, mode.Type().String(), mode.Perm()|mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky), uid, gid)
		switch {
		case mode.IsRegular():
			sum, err := fileSum(filepath.Join(root, relative))
			if err != nil {
				return err
			}
			line += fmt.Sprintf("\x00%d\x00%s", info.Size(), sum)
			result.Files++
			result.Bytes += info.Size()
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(filepath.Join(root, relative))
			if err != nil {
				return err
			}
			line += "\x00" + target
			result.Files++
		case mode.IsDir(), mode&fs.ModeNamedPipe != 0:
		case mode&fs.ModeSocket != 0:
			return nil
		default:
			return fmt.Errorf("%s is a device file and cannot be copied", relative)
		}
		_, err := io.WriteString(hash, line+"\n")
		return err
	})
	result.Digest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return result, err
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// walkSorted visits every entry under root except root itself, in lexical order, without following
// symbolic links.
func walkSorted(root string, visit func(string, fs.FileInfo) error) error {
	var walk func(string) error
	walk = func(relative string) error {
		entries, err := os.ReadDir(filepath.Join(root, relative))
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			child := filepath.Join(relative, entry.Name())
			info, err := os.Lstat(filepath.Join(root, child))
			if err != nil {
				return err
			}
			if err = visit(filepath.ToSlash(child), info); err != nil {
				return err
			}
			if info.IsDir() {
				if err = walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("")
}

func copyTree(from, to string) error {
	directories := []string{}
	err := walkSorted(from, func(relative string, info fs.FileInfo) error {
		source, target := filepath.Join(from, relative), filepath.Join(to, relative)
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			directories = append(directories, relative)
			return nil
		case mode.IsRegular():
			if err := copyFile(source, target); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(source)
			if err != nil {
				return err
			}
			if err = os.Symlink(link, target); err != nil {
				return err
			}
		case mode&fs.ModeNamedPipe != 0:
			if err := makeFifo(target, uint32(mode.Perm())); err != nil {
				return err
			}
		case mode&fs.ModeSocket != 0:
			return nil
		default:
			return fmt.Errorf("%s is a device file and cannot be copied", relative)
		}
		return applyMetadata(target, info)
	})
	if err != nil {
		return err
	}
	// Directory permissions and times are applied last so restrictive modes do not block children.
	for i := len(directories) - 1; i >= 0; i-- {
		info, err := os.Lstat(filepath.Join(from, directories[i]))
		if err != nil {
			return err
		}
		if err = applyMetadata(filepath.Join(to, directories[i]), info); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

func clearDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// VolumeCopy copies from into the empty directory to and verifies that the copy matches the source
// before and after copying. A source that changes while it is copied is copied again, at most three
// times, so the result is never a silently inconsistent mixture.
func VolumeCopy(from, to string, out io.Writer) error {
	before, err := digestTree(from)
	if err != nil {
		return err
	}
	free, err := freeBytes(to)
	if err == nil && free < before.Bytes+before.Bytes/10+256<<20 {
		return fmt.Errorf("not enough free Docker storage: %d bytes free, about %d needed", free, before.Bytes+before.Bytes/10+256<<20)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if before, err = digestTree(from); err != nil {
				return err
			}
		}
		if err = clearDirectory(to); err != nil {
			return err
		}
		if err = copyTree(from, to); err != nil {
			return err
		}
		after, err := digestTree(from)
		if err != nil {
			return err
		}
		copied, err := digestTree(to)
		if err != nil {
			return err
		}
		if before.Digest == after.Digest && copied.Digest == before.Digest {
			free, _ = freeBytes(to)
			return json.NewEncoder(out).Encode(volumeCopyResult{Files: copied.Files, Bytes: copied.Bytes, Digest: copied.Digest, Free: free})
		}
	}
	return errors.New("the source kept changing while it was copied; stop writes to it or choose an empty volume")
}

// VolumeFree reports free space on the filesystem that holds to.
func VolumeFree(to string, out io.Writer) error {
	free, err := freeBytes(to)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(volumeCopyResult{Free: free})
}

// VolumeDigest reports the digest of a directory tree; acceptance tests use it to compare copies.
func VolumeDigest(from string, out io.Writer) error {
	digest, err := digestTree(from)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(volumeCopyResult{Files: digest.Files, Bytes: digest.Bytes, Digest: digest.Digest})
}
