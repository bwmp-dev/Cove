package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseToolsValidateConnectionAndKeepSecretsOutOfArguments(t *testing.T) {
	for _, test := range []struct {
		engine, tool, host string
		port               int
	}{
		{"postgresql", "adminer", "postgres", 5432},
		{"mongodb", "mongo-express", "mongo", 27017},
		{"redis", "redisinsight", "redis", 6379},
		{"valkey", "redisinsight", "valkey", 6379},
	} {
		t.Run(test.engine, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			secret := strings.Repeat("b", 64)
			in := TunnelRequest{ID: "tool", Service: test.host, Tool: test.tool, Port: DatabaseToolPort(test.tool), Credentials: &DatabaseToolCredentials{Engine: test.engine, Host: test.host, Port: test.port, User: "app", Password: secret, Database: "app"}}
			if test.engine == "redis" || test.engine == "valkey" {
				in.Credentials.User, in.Credentials.Database = "default", "0"
			}
			if err = validateDatabaseTool(in); err != nil {
				t.Fatal(err)
			}
			image, _, command, environment, mounts, err := e.databaseToolCommand(in)
			if err != nil || image == "" || len(environment) == 0 {
				t.Fatal("missing tool configuration", err)
			}
			if strings.Contains(strings.Join(append(command, mounts...), " "), secret) {
				t.Fatal("secret in tool arguments")
			}
			if test.tool == "mongo-express" && !strings.Contains(strings.Join(environment, "\n"), "ME_CONFIG_BASICAUTH=true") {
				t.Fatal("web authentication missing")
			}
			if test.tool == "redisinsight" && !strings.Contains(strings.Join(environment, "\n"), "RI_DATABASE_MANAGEMENT=false") {
				t.Fatal("tool can manage unrelated database connections")
			}
			in.Credentials.Password = "untrusted"
			if validateDatabaseTool(in) == nil {
				t.Fatal("invalid credentials accepted")
			}
			in.Credentials.Password, in.Tool = secret, "unsupported"
			if validateDatabaseTool(in) == nil {
				t.Fatal("unsupported tool accepted")
			}
		})
	}
}

func TestDatabaseToolRemovesOnlyItsOwnedFiles(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = e.removeDatabaseTool(t.Context(), "cove-dbtool-other", "../other"); err == nil {
		t.Fatal("unsafe cleanup accepted")
	}
	path := filepath.Join(e.Root, "database-tools", "lease")
	if err = os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "ca.pem"), []byte("public trust"), 0444); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(e.Root, "database-tools", "other")
	if err = os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if args[1] == "ps" {
			if !strings.Contains(strings.Join(args, " "), "label=cove.database-tool.root=") {
				t.Fatal("cleanup lacks ownership filter")
			}
			_, err := io.WriteString(out, "owned-container")
			return err
		}
		if strings.Join(args, " ") != "docker rm -f -v owned-container" {
			t.Fatal("unexpected cleanup", args)
		}
		return nil
	}
	if err = e.removeDatabaseTool(t.Context(), "cove-dbtool-lease", "lease"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("lease trust files remain", err)
	}
	if _, err = os.Stat(foreign); err != nil {
		t.Fatal("foreign files removed", err)
	}
}
