package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNixpacksGeneratedDockerfilePinsImagesAndEnforcesNoneAndSelectedSecrets(t *testing.T) {
	data := []byte("FROM " + nixpacksBaseTag + "\nARG PUBLIC_NAME\nENV PUBLIC_NAME=${PUBLIC_NAME}\nWORKDIR /app\nCOPY . /app\nRUN --mount=type=cache,id=fixture,target=/root/.cache printf fixture\\\n && test -r /run/secrets/token\nCMD [\"./app\"]\n")
	result, err := validateNixpacksDockerfile(data, "none", []string{"token"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(result)
	if !strings.Contains(text, "FROM "+NixpacksBaseImage+"\n") || strings.Contains(text, nixpacksBaseTag) || !strings.Contains(text, "RUN --mount=type=cache,id=fixture,target=/root/.cache --network=none --mount=type=secret,id=token,target=/run/secrets/token,required=true printf fixture ") || strings.Count(text, "RUN ") != 1 {
		t.Fatal("image, network, command continuation or selected secret changed", text)
	}
	defaultNetwork, err := validateNixpacksDockerfile([]byte("FROM "+nixpacksBaseTag+"\nRUN echo fixture\n"), "default", nil)
	if err != nil || strings.Contains(string(defaultNetwork), "--network=") {
		t.Fatal("default networking changed", err)
	}
}

func TestNixpacksRejectsGeneratedImageNetworkMountAndParserOverrides(t *testing.T) {
	for name, suffix := range map[string]string{
		"frontend":   "# syntax=docker/dockerfile:1\nFROM " + nixpacksBaseTag + "\n",
		"escape":     "# escape=`\nFROM " + nixpacksBaseTag + "\n",
		"build base": "FROM attacker.invalid/base:latest\n",
		"run base":   "FROM " + nixpacksBaseTag + "\nFROM ubuntu:noble\n",
		"network":    "RUN --network=host echo fixture\n",
		"security":   "RUN --security=insecure echo fixture\n",
		"secret":     "RUN --mount=type=secret,id=unselected echo fixture\n",
		"bind":       "RUN --mount=type=bind,source=/,target=/host echo fixture\n",
		"cache from": "RUN --mount=type=cache,from=attacker,target=/tmp echo fixture\n",
		"cache root": "RUN --mount=type=cache,target=/ echo fixture\n",
		"copy image": "COPY --from=attacker.invalid/image /file /file\n",
		"add url":    "ADD https://attacker.invalid/file /file\n",
		"onbuild":    "ONBUILD RUN echo fixture\n",
		"syntax arg": "ARG BUILDKIT_SYNTAX\n",
		"proxy":      "ENV http_proxy=http://example.test\n",
		"cove env":   "ENV COVE_SECRET=x\n",
		"heredoc":    "RUN <<EOF\nFROM attacker\nEOF\n",
		"truncated":  "RUN echo fixture\\\n",
	} {
		t.Run(name, func(t *testing.T) {
			data := suffix
			if !strings.HasPrefix(data, "FROM ") && !strings.HasPrefix(data, "#") {
				data = "FROM " + nixpacksBaseTag + "\n" + data
			}
			if _, err := validateNixpacksDockerfile([]byte(data), "none", []string{"selected"}); err == nil {
				t.Fatal("unsafe generated Dockerfile accepted", data)
			}
		})
	}
}

func TestNixpacksSecretPolicyCannotExpandDockerfileBeyondItsBound(t *testing.T) {
	var selected []string
	for index := 0; index < 64; index++ {
		selected = append(selected, fmt.Sprintf("TOKEN_%d_", index)+strings.Repeat("X", 110))
	}
	data := []byte("FROM " + nixpacksBaseTag + "\n" + strings.Repeat("RUN echo fixture\n", 4095))
	if _, err := validateNixpacksDockerfile(data, "none", selected); err == nil || !strings.Contains(err.Error(), "exceeds 2 MiB") {
		t.Fatal("policy expansion was not bounded", err)
	}
}

func TestNixpacksSupportingOutputIsBoundedAndNeverReturnsBuildScript(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, ".nixpacks"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"Dockerfile": "FROM " + nixpacksBaseTag, "build.sh": "never execute this", "fixture.nix": "{}"} {
		if err := os.WriteFile(filepath.Join(directory, ".nixpacks", name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := readNixpacksGenerated(root)
	if err != nil || len(files) != 2 || files[".nixpacks/build.sh"] != nil {
		t.Fatal("generated build script exposed", files, err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".nixpacks", "large"), make([]byte, nixpacksOutputLimit+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNixpacksGenerated(root); err == nil {
		t.Fatal("unbounded output accepted")
	}
}

func TestNixpacksArtifactArchiveRequiresExactChecksumAndSafeEntries(t *testing.T) {
	archive := func(name string, kind byte, contents []byte) ([]byte, nixpacksArtifact) {
		var data bytes.Buffer
		zipped := gzip.NewWriter(&data)
		packed := tar.NewWriter(zipped)
		if err := packed.WriteHeader(&tar.Header{Name: name, Mode: 0555, Typeflag: kind, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := packed.Write(contents); err != nil {
			t.Fatal(err)
		}
		if err := packed.Close(); err != nil {
			t.Fatal(err)
		}
		if err := zipped.Close(); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data.Bytes())
		return data.Bytes(), nixpacksArtifact{Name: "fixture", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(data.Len())}
	}
	data, artifact := archive("nixpacks", tar.TypeReg, []byte{0x7f, 'E', 'L', 'F', 'f'})
	if _, err := extractNixpacksArchive(data, artifact); err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), data...)
	changed[len(changed)-1] ^= 1
	if _, err := extractNixpacksArchive(changed, artifact); err == nil {
		t.Fatal("tampered pinned archive accepted")
	}
	for _, name := range []string{"../nixpacks", "/nixpacks", "C:/nixpacks", "other"} {
		data, artifact := archive(name, tar.TypeReg, []byte{0x7f, 'E', 'L', 'F'})
		if _, err := extractNixpacksArchive(data, artifact); err == nil {
			t.Fatal("unsafe archive entry accepted", name)
		}
	}
	for _, url := range []string{"http://github.com/file", "https://attacker.invalid/file", "https://user:secret@github.com/file", "https://github.com:444/file"} {
		request, err := http.NewRequest("GET", url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if nixpacksArtifactRedirect(request, nil) == nil {
			t.Fatal("unsafe artifact redirect accepted", url)
		}
	}
}

func TestNixpacksGeneratedSupportRejectsSymlinks(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, ".nixpacks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "outside"), []byte("not-generated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "outside"), filepath.Join(directory, ".nixpacks", "Dockerfile")); err != nil {
		t.Skip("symlink creation is unavailable", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := readNixpacksGenerated(root); err == nil {
		t.Fatal("a symlink was returned as generated output")
	}
}

func TestNixpacksRecipePolicyAndCapabilityAreExplicit(t *testing.T) {
	recipe := BuildRecipe{Engine: "nixpacks", Service: "app", Directory: ".", BuildCommand: "echo build", StartCommand: "./app"}
	for _, kind := range []string{"service", "job"} {
		if err := ValidateRecipeSource(recipe, &GitSource{InlineCompose: true}, "services:\n  app:\n    image: fixture\n    build:\n      network: none\n", kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, build := range []map[string]any{{"network": "host"}, {"network": "application"}, {"args": map[string]any{"HTTP_PROXY": "http://proxy.test"}}, {"args": []any{"HTTP_PROXY"}}, {"args": map[string]any{"SECRET": "value"}}} {
		if validateNixpacksBuildPolicy(build) == nil {
			t.Fatal("unhonored policy accepted", build)
		}
	}
	probe := Probe{OS: "linux", Arch: "amd64", Nixpacks: true, BuildRecipes: true}
	status := NixpacksDetails(probe, true)
	if !status.Supported || !status.Maintenance || status.ProxySupported || status.PlannerSHA256 != NixpacksPlannerDigest("amd64") || status.BaseImage != NixpacksBaseImage {
		t.Fatal("incorrect supported details", status)
	}
	probe.Nixpacks = false
	if status := NixpacksDetails(probe, true); status.Supported || !strings.Contains(status.Reason, "upgrade") {
		t.Fatal("old worker accepted", status)
	}
	probe.Arch = "unknown"
	if status := NixpacksDetails(probe, true); status.Supported || status.PlannerSHA256 != "" {
		t.Fatal("unknown platform silently defaulted", status)
	}
}

func TestNixpacksRefusesDockerArchitectureMismatchBeforeAnyPlannerCommand(t *testing.T) {
	if !NixpacksSupported() {
		t.Skip("requires a supported Linux worker")
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	calls := 0
	e := &Engine{Run: func(_ context.Context, _ string, arguments, _ []string, out io.Writer) error {
		calls++
		if len(arguments) < 2 || arguments[0] != "docker" || arguments[1] != "info" {
			t.Fatal("planner or artifact preparation started before platform refusal", arguments)
		}
		_, err := fmt.Fprint(out, "linux/"+other)
		return err
	}}
	if err := e.checkNixpacksPlatform(context.Background(), "linux/"+runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "native Linux architecture") || calls != 1 {
		t.Fatal("mismatched Docker architecture accepted", err, calls)
	}
}
