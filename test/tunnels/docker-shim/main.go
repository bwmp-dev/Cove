package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

const pin = "docker.io/cloudflare/cloudflared:2026.11.1@sha256:e613aadbc2d9511785d2e67a86b0c8ac18dd63a3f9af0d06faf04cbfdcb35f2d"

var identity = regexp.MustCompile(`^[a-f0-9]{32}$`)
var containerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func command(args []string) *exec.Cmd {
	command := exec.Command("/usr/local/bin/docker", args...)
	command.Stdin = os.Stdin
	command.Stderr = os.Stderr
	return command
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "isolated Tunnel fixture Docker command failed")
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
	fixture := os.Getenv("COVE_TUNNEL_FIXTURE_OWNER")
	image := os.Getenv("COVE_TUNNEL_FIXTURE_IMAGE")
	if !regexp.MustCompile(`^cove-browser-[a-f0-9]{12}$`).MatchString(fixture) || image != fixture+"-tunnel-provider:test" {
		return errors.New("fixture identity missing")
	}
	marker, err := os.ReadFile("/var/lib/cove/.browser-owner")
	if err != nil || string(marker) != fixture {
		return errors.New("worker fixture marker differs")
	}
	provider, err := url.Parse(os.Getenv("COVE_TUNNEL_FIXTURE_PROVIDER"))
	if err != nil || provider.Scheme != "https" || provider.User != nil || provider.Path != "" || provider.RawQuery != "" {
		return errors.New("invalid fixture provider")
	}
	address, err := netip.ParseAddr(provider.Hostname())
	if err != nil || !address.IsPrivate() {
		return errors.New("fixture provider must be a private literal")
	}
	for _, arg := range args {
		if strings.Contains(arg, "cloudflare/cloudflared:") && arg != pin {
			return errors.New("native connectors are forbidden in this fake fixture")
		}
	}
	if len(args) > 2 && args[0] == "image" && args[1] == "inspect" && args[len(args)-1] == pin {
		args = slices.Clone(args)
		args[len(args)-1] = image
	}
	if len(args) > 2 && args[0] == "create" {
		position := slices.Index(args, pin)
		if position >= 0 {
			data, err := os.ReadFile("/var/lib/cove/build-owner.json")
			var owner string
			if err != nil || json.Unmarshal(data, &owner) != nil || !identity.MatchString(owner) || !slices.Contains(args, "cove.managed=cloudflare-tunnel") || !slices.Contains(args, "cove.cloudflare-tunnel-owner="+owner) {
				return errors.New("connector ownership differs")
			}
			copy := slices.Clone(args)
			copy[position] = image
			copy = append(copy[:position], append([]string{"--env", "COVE_TUNNEL_FIXTURE_PROVIDER=" + provider.String(), "--label", "cove.browser-fixture=" + fixture, "--mount", "type=bind,source=/var/lib/cove/tunnel-fixture-ca.pem,target=/fake-edge/cert.pem,readonly"}, copy[position:]...)...)
			output, err := command(copy).Output()
			if err != nil {
				return err
			}
			id := strings.TrimSpace(string(output))
			if !containerID.MatchString(id) {
				return errors.New("invalid fixture container")
			}
			attach := command([]string{"network", "connect", fixture, id})
			attach.Stdin = nil
			attach.Stdout = os.Stderr
			if err := attach.Run(); err != nil {
				return err
			}
			fmt.Println(id)
			return nil
		}
	}
	command := command(args)
	command.Stdout = os.Stdout
	return command.Run()
}
