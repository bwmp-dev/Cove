package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cove/internal/client"
)

type panelContext struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Team  string `json:"team"`
}
type contexts struct {
	Current string                  `json:"current"`
	Panels  map[string]panelContext `json:"contexts"`
}
type options struct {
	JSON       bool
	Context    string
	File       string
	In         io.Reader
	Out, Err   io.Writer
	HTTPClient func(string, string, string) (*client.Client, error)
}

var Commands = map[string]bool{"login": true, "context": true, "deploy": true, "logs": true, "exec": true, "env": true, "run": true, "port-forward": true, "api": true}

func Run(ctx context.Context, args []string) error {
	err := run(ctx, args, options{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
	var exit *ExitError
	if err != nil && !errors.As(err, &exit) {
		for _, arg := range args {
			if arg == "--" {
				break
			}
			if arg == "--json" {
				json.NewEncoder(os.Stdout).Encode(map[string]string{"error": err.Error()})
				return &ExitError{1}
			}
		}
	}
	return err
}
func contextPath() (string, error) {
	if file := os.Getenv("COVE_CONTEXT_FILE"); file != "" {
		return file, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cove", "contexts.json"), nil
}
func loadContexts(path string) (contexts, error) {
	result := contexts{Panels: map[string]panelContext{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, errors.New("could not read Cove contexts")
	}
	if len(raw) > 1024*1024 || json.Unmarshal(raw, &result) != nil || result.Panels == nil {
		return result, errors.New("invalid Cove context file")
	}
	return result, nil
}
func saveContexts(path string, value contexts) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, raw)
}
func writePrivateFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".contexts-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
func emit(o options, value any) error { return json.NewEncoder(o.Out).Encode(value) }
func run(ctx context.Context, args []string, o options) error {
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Err == nil {
		o.Err = os.Stderr
	}
	filtered := []string{}
	verbatim := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			verbatim = true
			filtered = append(filtered, arg)
			continue
		}
		if !verbatim && arg == "--json" {
			o.JSON = true
			continue
		}
		if !verbatim && arg == "--context" {
			i++
			if i >= len(args) {
				return errors.New("--context requires a name")
			}
			o.Context = args[i]
			continue
		}
		filtered = append(filtered, arg)
	}
	args = filtered
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		names := []string{}
		for name := range Commands {
			names = append(names, name)
		}
		sort.Strings(names)
		return emit(o, map[string]any{"commands": names, "usage": "cove COMMAND [--context NAME] [--json]; no arguments starts the control plane"})
	}
	if len(args) == 0 {
		return errors.New("usage: cove login|context|deploy|logs|exec|env|run|port-forward|api [--context NAME] [--json]")
	}
	if o.File == "" {
		var err error
		o.File, err = contextPath()
		if err != nil {
			return err
		}
	}
	saved, err := loadContexts(o.File)
	if err != nil {
		return err
	}
	if args[0] == "login" {
		return login(ctx, args[1:], o, saved)
	}
	if args[0] == "context" {
		if len(args) < 2 {
			return errors.New("usage: cove context list|use NAME|delete NAME")
		}
		switch args[1] {
		case "list":
			names := []string{}
			for name := range saved.Panels {
				names = append(names, name)
			}
			sort.Strings(names)
			entries := []map[string]any{}
			for _, name := range names {
				panel := saved.Panels[name]
				entries = append(entries, map[string]any{"name": name, "url": panel.URL, "team": panel.Team, "current": name == saved.Current})
			}
			return emit(o, entries)
		case "use", "delete":
			if len(args) != 3 {
				return errors.New("context name required")
			}
			if _, ok := saved.Panels[args[2]]; !ok {
				return errors.New("unknown context")
			}
			if args[1] == "use" {
				saved.Current = args[2]
			} else {
				delete(saved.Panels, args[2])
				if saved.Current == args[2] {
					saved.Current = ""
				}
			}
			if err = saveContexts(o.File, saved); err != nil {
				return err
			}
			return emit(o, map[string]any{"current": saved.Current, "ok": true})
		}
		return errors.New("unknown context command")
	}
	name := o.Context
	if name == "" {
		name = saved.Current
	}
	panel, ok := saved.Panels[name]
	if !ok {
		return errors.New("choose a saved context with cove login or --context")
	}
	factory := o.HTTPClient
	if factory == nil {
		factory = client.New
	}
	api, err := factory(panel.URL, panel.Token, panel.Team)
	if err != nil {
		return err
	}
	return execute(ctx, api, args, o)
}
func login(ctx context.Context, args []string, o options, saved contexts) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(o.Err)
	name := flags.String("name", "default", "context name")
	address := flags.String("url", "", "HTTPS panel origin")
	team := flags.String("team", "", "selected team ID")
	stdin := flags.Bool("token-stdin", false, "read a scoped API token from standard input")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*stdin || strings.TrimSpace(*name) == "" || len(*name) > 120 {
		return errors.New("use --name NAME --url HTTPS_ORIGIN --team TEAM --token-stdin; create a scoped token in API keys first")
	}
	raw, err := io.ReadAll(io.LimitReader(o.In, 8193))
	if err != nil || len(raw) > 8192 {
		return errors.New("token input exceeds limit")
	}
	token := strings.TrimSpace(string(raw))
	factory := o.HTTPClient
	if factory == nil {
		factory = client.New
	}
	api, err := factory(*address, token, *team)
	if err != nil {
		return err
	}
	var identity map[string]any
	if err = api.Do(ctx, "GET", "/session", nil, &identity); err != nil {
		return err
	}
	saved.Panels[*name] = panelContext{URL: *address, Token: token, Team: *team}
	saved.Current = *name
	if err = saveContexts(o.File, saved); err != nil {
		return err
	}
	return emit(o, map[string]any{"context": *name, "url": *address, "team": *team, "ok": true})
}
func commandFlags(name string, o options) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(o.Err)
	return flags
}
func usage(format string) error { return fmt.Errorf("usage: cove %s", format) }
