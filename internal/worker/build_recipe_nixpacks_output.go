package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

var nixpacksStageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func readNixpacksGenerated(root *os.Root) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := int64(0)
	entries := 0
	err := fs.WalkDir(root.FS(), ".nixpacks", func(name string, entry fs.DirEntry, failure error) error {
		if failure != nil {
			return failure
		}
		entries++
		if entries > 256 || path.Clean(name) != name || !strings.HasPrefix(name, ".nixpacks") || strings.ContainsAny(name, "\\\x00\r\n:") {
			return errors.New("Nixpacks supporting files exceed their bound or contain an unsafe path")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("Nixpacks supporting files cannot contain symlinks or special files")
		}
		if name == ".nixpacks/build.sh" {
			return nil
		}
		if len(files) >= 128 || info.Size() > nixpacksOutputLimit-total {
			return errors.New("Nixpacks supporting files exceed the 8 MiB limit")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, nixpacksOutputLimit-total+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(data)) != info.Size() {
			return errors.New("Nixpacks supporting files changed or could not be read")
		}
		total += int64(len(data))
		files[name] = data
		return nil
	})
	if err == nil && len(files[".nixpacks/Dockerfile"]) == 0 {
		err = errors.New("Nixpacks did not generate a Dockerfile")
	}
	return files, err
}

func ReadNixpacksOutput(out io.Writer) error {
	root, err := os.OpenRoot("/output")
	if err != nil {
		return err
	}
	defer root.Close()
	files, err := readNixpacksGenerated(root)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(files)
}

func validateNixpacksCacheMount(flag string) error {
	options := map[string]string{}
	for _, item := range strings.Split(strings.TrimPrefix(flag, "--mount="), ",") {
		key, value, ok := strings.Cut(item, "=")
		if !ok || options[key] != "" {
			return errors.New("Nixpacks emitted an unsupported cache mount")
		}
		switch key {
		case "type", "id", "target", "sharing", "uid", "gid", "mode":
		default:
			return errors.New("Nixpacks cannot add bind, image or secret mounts")
		}
		options[key] = value
	}
	target := options["target"]
	if options["type"] != "cache" || !strings.HasPrefix(target, "/") || path.Clean(target) != target || target == "/" || target == "/run" || strings.HasPrefix(target, "/run/secrets") || strings.ContainsAny(target, "\\\x00\r\n\"'") {
		return errors.New("Nixpacks emitted an unsafe cache mount target")
	}
	return nil
}

func validateNixpacksDockerfile(data []byte, network string, selectedSecrets []string) ([]byte, error) {
	if len(data) == 0 || len(data) > 2<<20 || !utf8.Valid(data) || bytes.ContainsAny(data, "\x00\r") || bytes.Contains(data, []byte("<<")) {
		return nil, errors.New("Nixpacks Dockerfile is empty, oversized or uses unsupported control characters or heredocs")
	}
	var output strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	logical := ""
	stages := map[string]bool{}
	froms, instructions := 0, 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if logical == "" && (line == "" || strings.HasPrefix(line, "#")) {
			if name, _, found := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#")), "="); found {
				switch strings.ToLower(strings.TrimSpace(name)) {
				case "syntax", "escape", "check":
					return nil, errors.New("Nixpacks cannot select Dockerfile parser directives or a frontend")
				}
			}
			continue
		}
		if strings.HasSuffix(line, "\\") {
			logical += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		line = strings.TrimSpace(logical + line)
		logical = ""
		instruction, rest, found := strings.Cut(line, " ")
		if !found {
			instruction, rest, found = strings.Cut(line, "\t")
		}
		instruction = strings.ToUpper(instruction)
		rest = strings.TrimSpace(rest)
		instructions++
		if !found || rest == "" || instructions > 4096 || froms == 0 && instruction != "FROM" {
			return nil, errors.New("Nixpacks emitted an invalid or excessive Dockerfile instruction")
		}
		switch instruction {
		case "FROM":
			fields := strings.Fields(rest)
			if len(fields) != 1 && (len(fields) != 3 || !strings.EqualFold(fields[1], "as") || !nixpacksStageName.MatchString(fields[2])) {
				return nil, errors.New("Nixpacks emitted an unsupported image stage")
			}
			if fields[0] != nixpacksBaseTag && fields[0] != NixpacksBaseImage || froms >= 4 {
				return nil, errors.New("Nixpacks cannot replace Cove's digest-pinned base or runtime image")
			}
			rest = NixpacksBaseImage
			if len(fields) == 3 {
				name := strings.ToLower(fields[2])
				if stages[name] {
					return nil, errors.New("Nixpacks image stages must be unique")
				}
				stages[name] = true
				rest += " AS " + fields[2]
			}
			stages[fmt.Sprint(froms)] = true
			froms++
		case "RUN":
			flags := ""
			for strings.HasPrefix(rest, "--") {
				flag, remaining, ok := strings.Cut(rest, " ")
				if !ok || !strings.HasPrefix(flag, "--mount=") {
					return nil, errors.New("Nixpacks cannot override build networking, security or mounts")
				}
				if err := validateNixpacksCacheMount(flag); err != nil {
					return nil, err
				}
				flags += flag + " "
				rest = strings.TrimSpace(remaining)
			}
			prefix := ""
			if network == "none" {
				prefix = "--network=none "
			}
			for _, name := range selectedSecrets {
				if !environmentIdentifier.MatchString(name) || len(name) > 128 {
					return nil, errors.New("Nixpacks selected build secret has an invalid name")
				}
				prefix += "--mount=type=secret,id=" + name + ",target=/run/secrets/" + name + ",required=true "
			}
			rest = flags + prefix + rest
		case "COPY":
			fields := strings.Fields(rest)
			for _, field := range fields {
				if !strings.HasPrefix(field, "--") {
					break
				}
				switch {
				case strings.HasPrefix(field, "--from="):
					if !stages[strings.ToLower(strings.TrimPrefix(field, "--from="))] {
						return nil, errors.New("Nixpacks cannot copy from an unapproved external image")
					}
				case strings.HasPrefix(field, "--chmod="), strings.HasPrefix(field, "--chown="):
				default:
					return nil, errors.New("Nixpacks emitted unsupported copy flags")
				}
			}
		case "ARG", "ENV":
			for _, field := range strings.Fields(rest) {
				name, _, _ := strings.Cut(field, "=")
				if !environmentIdentifier.MatchString(name) {
					return nil, errors.New("Nixpacks emitted an unsupported variable declaration")
				}
				upper := strings.ToUpper(name)
				if strings.HasPrefix(upper, "BUILDKIT_") || strings.HasPrefix(upper, "DOCKER_") || strings.HasPrefix(upper, "COVE_") || upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" {
					return nil, errors.New("Nixpacks cannot override Cove, Docker, frontend or proxy configuration")
				}
			}
		case "WORKDIR", "ENTRYPOINT", "CMD", "EXPOSE", "USER", "LABEL", "SHELL":
		default:
			return nil, errors.New("Nixpacks emitted an unsupported Dockerfile instruction: " + instruction)
		}
		if output.Len()+len(instruction)+len(rest)+2 > 2<<20 {
			return nil, errors.New("the validated Nixpacks Dockerfile exceeds 2 MiB after applying build policy")
		}
		if instruction != "" {
			output.WriteString(instruction + " ")
		}
		output.WriteString(rest + "\n")
	}
	if err := scanner.Err(); err != nil || logical != "" || froms == 0 {
		return nil, errors.New("Nixpacks emitted a truncated Dockerfile")
	}
	return []byte(output.String()), nil
}
