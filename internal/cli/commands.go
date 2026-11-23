package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"cove/internal/client"
	"cove/internal/connect"
	"golang.org/x/crypto/ssh"
)

type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("command exited with status %d", e.Code) }
func pathID(id string) string      { return url.PathEscape(id) }
func execute(ctx context.Context, api *client.Client, args []string, o options) error {
	switch args[0] {
	case "deploy":
		flags := commandFlags("deploy", o)
		wait := flags.Bool("wait", true, "wait for terminal deployment state")
		reason := flags.String("reason", "", "optional audited deployment reason")
		timeout := flags.Duration("timeout", 10*time.Minute, "maximum wait")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *timeout < time.Second || *timeout > 24*time.Hour {
			return usage("deploy [--wait=false] [--timeout 10m] RESOURCE")
		}
		var operation map[string]any
		if err := api.Do(ctx, "POST", "/workloads/"+pathID(flags.Arg(0))+"/operations", map[string]string{"action": "deploy", "reason": *reason}, &operation); err != nil {
			return err
		}
		if *wait {
			id, ok := operation["id"].(string)
			if !ok || id == "" {
				return errors.New("deployment response omitted its operation ID")
			}
			var err error
			operation, err = waitOperation(ctx, api, id, *timeout)
			if err != nil {
				return err
			}
		}
		if err := emit(o, operation); err != nil {
			return err
		}
		if state, _ := operation["state"].(string); *wait && state != "succeeded" {
			return &ExitError{1}
		}
		return nil
	case "logs":
		flags := commandFlags("logs", o)
		follow := flags.Bool("follow", false, "poll log snapshots")
		operation := flags.Bool("operation", false, "read operation logs")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return usage("logs [--follow] [--operation] ID")
		}
		path := "/workloads/"
		if *operation {
			path = "/operations/"
		}
		path += pathID(flags.Arg(0)) + "/logs"
		previous := ""
		for {
			var body struct {
				Logs string `json:"logs"`
			}
			if err := api.Do(ctx, "GET", path, nil, &body); err != nil {
				return err
			}
			chunk := logDelta(previous, body.Logs)
			previous = body.Logs
			if chunk != "" {
				if o.JSON {
					if err := emit(o, map[string]string{"logs": chunk}); err != nil {
						return err
					}
				} else if _, err := io.WriteString(o.Out, chunk); err != nil {
					return err
				}
			}
			if !*follow {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
	case "exec":
		flags := commandFlags("exec", o)
		service := flags.String("service", "", "Compose service name")
		container := flags.String("container", "", "replica container ID")
		timeout := flags.Int("timeout", 30, "maximum seconds,1-30")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		parts := flags.Args()
		if len(parts) < 2 || *service == "" {
			return usage("exec --service SERVICE [--container ID] RESOURCE -- COMMAND [ARG...]")
		}
		command := parts[1:]
		if command[0] == "--" {
			command = command[1:]
		}
		var result struct {
			Output    string `json:"output"`
			ExitCode  int    `json:"exit_code"`
			Truncated bool   `json:"truncated"`
			TimedOut  bool   `json:"timed_out"`
		}
		body := map[string]any{"service": *service, "container": *container, "command": command, "timeout_seconds": *timeout}
		if err := api.Do(ctx, "POST", "/workloads/"+pathID(parts[0])+"/exec", body, &result); err != nil {
			return err
		}
		if o.JSON {
			if err := emit(o, result); err != nil {
				return err
			}
		} else {
			if _, err := io.WriteString(o.Out, result.Output); err != nil {
				return err
			}
			if result.Truncated {
				fmt.Fprintln(o.Err, "Output truncated at 64 KiB.")
			}
		}
		if result.TimedOut {
			fmt.Fprintln(o.Err, "Command wait timed out; the process may still be running. Inspect the container before retrying.")
		}
		if result.ExitCode != 0 || result.TimedOut {
			code := result.ExitCode
			if code < 1 || code > 255 {
				code = 1
			}
			return &ExitError{code}
		}
		return nil
	case "env":
		return environment(ctx, api, args[1:], o)
	case "run":
		if len(args) < 3 {
			return usage("run RESOURCE -- COMMAND [ARG...]")
		}
		command := args[2:]
		if command[0] == "--" {
			command = command[1:]
		}
		if len(command) == 0 {
			return usage("run RESOURCE -- COMMAND [ARG...]")
		}
		var result struct {
			Environment map[string]string `json:"environment"`
		}
		if err := api.Do(ctx, "POST", "/workloads/"+pathID(args[1])+"/variables/resolve", nil, &result); err != nil {
			return err
		}
		process := exec.CommandContext(ctx, command[0], command[1:]...)
		process.Stdin = o.In
		process.Env = os.Environ()
		for name, value := range result.Environment {
			process.Env = append(process.Env, name+"="+value)
		}
		output := &boundedOutput{limit: 4 * 1024 * 1024}
		if o.JSON {
			process.Stdout = output
			process.Stderr = output
		} else {
			process.Stdout = o.Out
			process.Stderr = o.Err
		}
		err := process.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				return errors.New("could not start the local command")
			}
			code = exit.ExitCode()
		}
		if o.JSON {
			if err := emit(o, map[string]any{"output": output.text, "exit_code": code, "truncated": output.truncated}); err != nil {
				return err
			}
		}
		if code != 0 {
			return &ExitError{code}
		}
		return nil
	case "port-forward":
		return portForward(ctx, api, args[1:], o)
	case "api":
		if len(args) < 3 || len(args) > 4 {
			return usage("api METHOD /PATH [JSON_FILE|-]")
		}
		var input any
		if len(args) == 4 {
			raw, err := readInput(args[3], o.In, 2*1024*1024)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(raw, &input); err != nil {
				return err
			}
		}
		var output any
		if err := api.Do(ctx, strings.ToUpper(args[1]), args[2], input, &output); err != nil {
			return err
		}
		return emit(o, output)
	}
	return errors.New("unknown Cove command")
}
func waitOperation(ctx context.Context, api *client.Client, id string, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var operation map[string]any
		if err := api.Do(ctx, "GET", "/operations/"+pathID(id), nil, &operation); err != nil {
			return nil, err
		}
		state, _ := operation["state"].(string)
		switch state {
		case "succeeded", "failed", "cancelled", "interrupted":
			return operation, nil
		case "queued", "running":
		default:
			return nil, errors.New("operation returned an unknown state")
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("deployment wait expired or interrupted; inspect the operation before retrying")
		case <-time.After(time.Second):
		}
	}
}
func logDelta(previous, current string) string {
	if previous == "" {
		return current
	}
	if previous == current {
		return ""
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	maxSize := min(len(previous), len(current))
	for size := maxSize; size > 0; size-- {
		if (size == len(previous) || previous[len(previous)-size-1] == '\n') && strings.HasSuffix(previous, current[:size]) {
			return current[size:]
		}
	}
	return current
}

type boundedOutput struct {
	text      string
	limit     int
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	size := len(p)
	remaining := b.limit - len(b.text)
	if size > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.text += string(p)
	return size, nil
}
func readInput(file string, input io.Reader, limit int) ([]byte, error) {
	if file != "-" {
		handle, err := os.Open(file)
		if err != nil {
			return nil, errors.New("could not read input file")
		}
		defer handle.Close()
		input = handle
	}
	raw, err := io.ReadAll(io.LimitReader(input, int64(limit+1)))
	if err != nil || len(raw) > limit {
		return nil, errors.New("input could not be read or exceeds limit")
	}
	return raw, nil
}
func environment(ctx context.Context, api *client.Client, args []string, o options) error {
	if len(args) < 1 {
		return usage("env pull|push [--file PATH] RESOURCE")
	}
	flags := commandFlags("env", o)
	file := flags.String("file", "-", "dotenv file; - for standard input/output")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return usage("env pull|push [--file PATH] RESOURCE")
	}
	path := "/workloads/" + pathID(flags.Arg(0)) + "/variables/"
	if args[0] == "push" {
		raw, err := readInput(*file, o.In, 1024*1024)
		if err != nil {
			return err
		}
		var result any
		if err = api.Do(ctx, "POST", path+"import", map[string]string{"dotenv": string(raw)}, &result); err != nil {
			return err
		}
		return emit(o, result)
	}
	if args[0] != "pull" {
		return usage("env pull|push [--file PATH] RESOURCE")
	}
	var result struct {
		Dotenv string `json:"dotenv"`
	}
	if err := api.Do(ctx, "POST", path+"export", nil, &result); err != nil {
		return err
	}
	if *file != "-" {
		if err := writePrivateFile(*file, []byte(result.Dotenv)); err != nil {
			return err
		}
		return emit(o, map[string]string{"file": *file})
	}
	if o.JSON {
		return emit(o, result)
	}
	_, err := io.WriteString(o.Out, result.Dotenv)
	return err
}
func portForward(ctx context.Context, api *client.Client, args []string, o options) error {
	flags := commandFlags("port-forward", o)
	service := flags.String("service", "", "Compose service")
	remote := flags.Int("port", 0, "service port")
	local := flags.Int("local-port", 0, "loopback listen port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || *service == "" || *remote < 1 || *remote > 65535 || *local < 1 || *local > 65535 {
		return usage("port-forward --service SERVICE --port PORT --local-port PORT RESOURCE")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	key, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "cove-tunnel-key-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(pem.EncodeToMemory(key))
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	path := "/workloads/" + pathID(flags.Arg(0)) + "/database-access"
	var lease struct {
		ID      string `json:"id"`
		Host    string `json:"host"`
		SSHPort int    `json:"ssh_port"`
		HostKey string `json:"host_key"`
		Expires string `json:"expires_at"`
	}
	if err = api.Do(ctx, "POST", path, map[string]any{"service": *service, "port": *remote, "public_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}, &lease); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := api.Do(cleanup, "DELETE", path, map[string]string{"id": lease.ID}, nil); err != nil {
			fmt.Fprintln(o.Err, "Lease revocation was not confirmed; access expires at its original deadline.")
		}
	}()
	if err = emit(o, map[string]any{"lease_id": lease.ID, "local_port": *local, "expires_at": lease.Expires}); err != nil {
		return err
	}
	return connect.Run(ctx, []string{"--host", lease.Host, "--ssh-port", fmt.Sprint(lease.SSHPort), "--host-key", lease.HostKey, "--identity", file.Name(), "--local-port", fmt.Sprint(*local), "--expires", lease.Expires})
}
