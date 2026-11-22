package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cove/internal/client"
)

func TestCLINamedContextsDeployEnvironmentAndExec(t *testing.T) {
	token := "fixture-personal-token"
	var imported string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Cove-Team") != "team" {
			t.Fatal("CLI lost named context auth")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/session":
			fmt.Fprint(w, `{"user_id":"fixture-user"}`)
		case "/api/v1/workloads/app/operations":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"id":"operation","state":"queued"}`)
		case "/api/v1/operations/operation":
			fmt.Fprint(w, `{"id":"operation","state":"succeeded"}`)
		case "/api/v1/workloads/app/variables/import":
			var in struct {
				Dotenv string `json:"dotenv"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil {
				t.Fatal("bad dotenv payload")
			}
			imported = in.Dotenv
			fmt.Fprint(w, `{"keys":["VALUE"]}`)
		case "/api/v1/workloads/app/variables/export":
			fmt.Fprint(w, `{"dotenv":"VALUE=\"fixture-secret\"\n"}`)
		case "/api/v1/workloads/app/exec":
			var in struct {
				Command []string `json:"command"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if len(in.Command) != 2 || in.Command[1] != "--json" {
				t.Fatal("CLI consumed command arguments")
			}
			fmt.Fprint(w, `{"output":"fixture-output","exit_code":7,"truncated":false,"timed_out":false}`)
		default:
			t.Fatalf("unexpected CLI route %s", r.URL.Path)
		}
	}))
	defer server.Close()
	factory := func(address, token, team string) (*client.Client, error) {
		api, err := client.New(address, token, team)
		if err == nil {
			api.HTTP.Transport = server.Client().Transport
		}
		return api, err
	}
	var output bytes.Buffer
	options := options{File: filepath.Join(t.TempDir(), "contexts.json"), In: strings.NewReader(token + "\n"), Out: &output, Err: &output, HTTPClient: factory, JSON: true}
	if err := run(context.Background(), []string{"login", "--name", "test", "--url", server.URL, "--team", "team", "--token-stdin"}, options); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), token) {
		t.Fatal("login displayed credential")
	}
	output.Reset()
	if err := run(context.Background(), []string{"context", "list"}, options); err != nil || strings.Contains(output.String(), token) {
		t.Fatal("context list disclosed token", err)
	}
	output.Reset()
	if err := run(context.Background(), []string{"--json", "--context", "test", "deploy", "app"}, options); err != nil || !strings.Contains(output.String(), "succeeded") {
		t.Fatal("deploy wait failed", err)
	}
	options.In = strings.NewReader("VALUE=fixture-secret\n")
	output.Reset()
	if err := run(context.Background(), []string{"env", "push", "app"}, options); err != nil || imported != "VALUE=fixture-secret\n" {
		t.Fatal("env push failed", err)
	}
	file := filepath.Join(t.TempDir(), "output.env")
	if err := run(context.Background(), []string{"env", "pull", "--file", file, "app"}, options); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(raw), "fixture-secret") {
		t.Fatal("env pull failed", err)
	}
	output.Reset()
	err = run(context.Background(), []string{"exec", "--service", "web", "app", "--", "printf", "--json"}, options)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 7 || !strings.Contains(output.String(), "fixture-output") {
		t.Fatal("exec exit status lost", err)
	}
}
func TestLogDeltaRotatingSnapshots(t *testing.T) {
	for _, test := range []struct{ previous, current, want string }{{"a\nb\n", "b\nc\n", "c\n"}, {"a\n", "a\n", ""}, {"a\n", "a\nb\n", "b\n"}, {"old\n", "replacement\n", "replacement\n"}} {
		if actual := logDelta(test.previous, test.current); actual != test.want {
			t.Fatalf("unexpected log continuation %q", actual)
		}
	}
}
