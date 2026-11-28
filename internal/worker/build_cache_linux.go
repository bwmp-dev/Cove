package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type cacheMount struct{ root, point, filesystem, source, options string }

func cacheMountAt(data, path string) (cacheMount, error) {
	if len(data) > 1024*1024 {
		return cacheMount{}, errors.New("mount inventory exceeds bound")
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var found cacheMount
	count := 0
	for scanner.Scan() {
		count++
		if count > 4096 {
			return found, errors.New("mount inventory exceeds bound")
		}
		columns := strings.Fields(scanner.Text())
		if len(columns) < 10 {
			return found, errors.New("mount inventory is invalid")
		}
		split := -1
		for i := 6; i < len(columns); i++ {
			if columns[i] == "-" {
				split = i
				break
			}
		}
		if split < 0 || len(columns) < split+4 {
			return found, errors.New("mount inventory is invalid")
		}
		point := unescape.Replace(columns[4])
		if point != path {
			continue
		}
		if found.point != "" {
			return found, errors.New("stacked mounts cannot be verified")
		}
		found = cacheMount{root: unescape.Replace(columns[3]), point: point, filesystem: columns[split+1], source: unescape.Replace(columns[split+2]), options: columns[5]}
	}
	if err := scanner.Err(); err != nil {
		return found, err
	}
	if found.point == "" || found.root != "/" {
		return found, errors.New("build cache must be the root of a dedicated filesystem")
	}
	for _, option := range strings.Split(found.options, ",") {
		if option == "ro" {
			return found, errors.New("build cache filesystem is read only")
		}
	}
	return found, nil
}

func inspectCacheFilesystem(path, dockerRoot string) (cacheFilesystem, uint64, error) {
	var identity cacheFilesystem
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || dockerRoot == "" {
		return identity, 0, errors.New("an absolute dedicated build cache mount is required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return identity, 0, errors.New("build cache path must exist without symlink components")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return identity, 0, errors.New("build cache mount identity cannot be read")
	}
	mount, err := cacheMountAt(string(data), path)
	if err != nil {
		return identity, 0, err
	}
	var state, root, docker syscall.Statfs_t
	if syscall.Statfs(path, &state) != nil || syscall.Statfs("/", &root) != nil || syscall.Statfs(dockerRoot, &docker) != nil {
		return identity, 0, errors.New("build cache and Docker filesystem identities cannot be verified")
	}
	if state.Fsid == root.Fsid || state.Fsid == docker.Fsid {
		return identity, 0, errors.New("build cache must use a different filesystem from Docker and the system root")
	}
	identity, available, err := cacheFilesystemIdentity(path)
	identity.Source = mount.source
	return identity, available, err
}

func cacheFilesystemIdentity(path string) (cacheFilesystem, uint64, error) {
	var state syscall.Statfs_t
	if syscall.Statfs(path, &state) != nil {
		return cacheFilesystem{}, 0, errors.New("build cache filesystem identity is unavailable")
	}
	if state.Bsize <= 0 || state.Blocks == 0 || state.Blocks > ^uint64(0)/uint64(state.Bsize) {
		return cacheFilesystem{}, 0, errors.New("build cache filesystem size is unavailable")
	}
	var metadata syscall.Stat_t
	if syscall.Stat(path, &metadata) != nil {
		return cacheFilesystem{}, 0, errors.New("build cache mount metadata unavailable")
	}
	identity := cacheFilesystem{Filesystem: fmt.Sprintf("%x:%s:%s", state.Type, strconv.FormatInt(int64(state.Fsid.X__val[0]), 16), strconv.FormatInt(int64(state.Fsid.X__val[1]), 16)), Device: uint64(metadata.Dev), Capacity: state.Blocks * uint64(state.Bsize)}
	return identity, state.Bavail * uint64(state.Bsize), nil
}

func BuildCacheFilesystem(out io.Writer) error {
	identity, _, err := cacheFilesystemIdentity("/state")
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(identity)
}
