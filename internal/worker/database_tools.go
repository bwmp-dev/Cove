package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (e *Engine) removeDatabaseTool(ctx context.Context, helper, id string) error {
	if !identifier.MatchString(id) {
		return errors.New("invalid tool access ID")
	}
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^"+helper+"$", "--filter", fmt.Sprintf("label=cove.database-tool.root=%x", sha256.Sum256([]byte(e.Root))))
	if err != nil {
		return err
	}
	for _, container := range strings.Fields(ids) {
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", "-v", container}, nil, io.Discard); err != nil {
			return errors.New("private admin tool removal requires retry")
		}
	}
	return os.RemoveAll(filepath.Join(e.Root, "database-tools", id))
}

type DatabaseToolCredentials struct {
	Engine        string `json:"engine"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	Database      string `json:"database"`
	CACertificate string `json:"ca_certificate,omitempty"`
}

func DatabaseAdminTools(engine string) []string {
	switch engine {
	case "postgresql", "mysql", "mariadb":
		return []string{"adminer"}
	case "mongodb":
		return []string{"mongo-express"}
	case "redis", "valkey":
		return []string{"redisinsight"}
	}
	return []string{}
}

func DatabaseToolPort(tool string) int {
	switch tool {
	case "adminer":
		return 8080
	case "mongo-express":
		return 8081
	case "redisinsight":
		return 5540
	}
	return 0
}

func DatabaseToolImage(tool string) string {
	switch tool {
	case "adminer":
		return "adminer:6.3.0-standalone"
	case "mongo-express":
		return "mongo-express:1.0.2-20-alpine3.19"
	case "redisinsight":
		return "redis/redisinsight:3.10.0"
	}
	return ""
}

func validateDatabaseTool(in TunnelRequest) error {
	if in.Tool == "" {
		if in.Credentials != nil {
			return errors.New("native tunnels do not accept tool credentials")
		}
		return nil
	}
	if DatabaseToolPort(in.Tool) != in.Port {
		return errors.New("select a supported tool on its private service port")
	}
	if in.Credentials == nil {
		if in.Tool == "adminer" && (in.Service == "postgres" || in.Service == "mysql" || in.Service == "mariadb") {
			return nil
		}
		return errors.New("database tool requires scoped connection credentials")
	}
	c := in.Credentials
	allowed := false
	for _, tool := range DatabaseAdminTools(c.Engine) {
		allowed = allowed || tool == in.Tool
	}
	if !allowed || (!serviceIdentifier.MatchString(c.Host) && !ValidLinkAlias(c.Host)) || !databaseSecret.MatchString(c.Password) || c.Port < 1 || c.Port > 65535 || (c.User != "app" && c.User != "default") || (c.Database != "app" && c.Database != "0") {
		return errors.New("invalid database tool connection")
	}
	port := map[string]int{"postgresql": 5432, "mysql": 3306, "mariadb": 3306, "mongodb": 27017, "redis": 6379, "valkey": 6379}[c.Engine]
	service := map[string]string{"postgresql": "postgres", "mysql": "mysql", "mariadb": "mariadb", "mongodb": "mongo", "redis": "redis", "valkey": "valkey"}[c.Engine]
	user, database := "app", "app"
	if c.Engine == "redis" || c.Engine == "valkey" {
		user, database = "default", "0"
		if c.CACertificate != "" {
			port = 6380
		}
	}
	if c.Port != port || c.User != user || c.Database != database || in.Service != service {
		return errors.New("database tool connection does not match its managed engine")
	}
	if c.CACertificate != "" {
		roots := x509.NewCertPool()
		if len(c.CACertificate) > 65536 || !roots.AppendCertsFromPEM([]byte(c.CACertificate)) {
			return errors.New("invalid database tool CA certificate")
		}
	}
	return nil
}

func (e *Engine) databaseToolCommand(in TunnelRequest) (image, memory string, command []string, environment []string, mounts []string, err error) {
	image = DatabaseToolImage(in.Tool)
	env := map[string]string{}
	host := in.Service
	if in.Credentials != nil {
		host = in.Credentials.Host
	}
	switch in.Tool {
	case "adminer":
		memory = "128m"
		command = []string{"entrypoint.sh", "docker-php-entrypoint", "php", "-S", "0.0.0.0:8080", "-t", "/var/www/html"}
		port, driver := 3306, "server"
		if in.Service == "postgres" {
			port, driver = 5432, "pgsql"
		}
		if in.Credentials != nil {
			port = in.Credentials.Port
		}
		server := net.JoinHostPort(host, strconv.Itoa(port))
		env["ADMINER_DEFAULT_SERVER"] = server
		dir := filepath.Join(e.Root, "database-tools", in.ID)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return
		}
		plugin := filepath.Join(dir, "servers.php")
		content := fmt.Sprintf("<?php\nrequire_once '/var/www/html/plugins/login-servers.php';\nreturn new AdminerLoginServers([%q=>['server'=>%q,'driver'=>%q]]);\n", server, server, driver)
		if err = os.WriteFile(plugin, []byte(content), 0444); err != nil {
			return
		}
		mounts = append(mounts, "--mount", "type=bind,source="+plugin+",target=/var/www/html/plugins-enabled/cove-servers.php,readonly")
	case "mongo-express":
		memory = "256m"
		command = []string{"/sbin/tini", "--", "/docker-entrypoint.sh", "mongo-express"}
		c := in.Credentials
		u := &url.URL{Scheme: "mongodb", Host: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), User: url.UserPassword(c.User, c.Password), Path: "/" + c.Database, RawQuery: "authSource=admin"}
		if c.CACertificate != "" {
			q := u.Query()
			q.Set("tls", "true")
			q.Set("tlsCAFile", "/run/cove-database-ca.pem")
			u.RawQuery = q.Encode()
		}
		env["ME_CONFIG_MONGODB_URL"] = u.String()
		env["ME_CONFIG_BASICAUTH"] = "true"
		env["ME_CONFIG_BASICAUTH_USERNAME"], env["ME_CONFIG_BASICAUTH_PASSWORD"] = c.User, c.Password
	case "redisinsight":
		memory = "512m"
		command = []string{"/usr/src/app/docker-entry.sh", "node", "redisinsight/api/dist/src/main"}
		c := in.Credentials
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return
		}
		env["RI_ENCRYPTION_KEY"] = hex.EncodeToString(key)
		env["RI_REDIS_HOST"], env["RI_REDIS_PORT"], env["RI_REDIS_USERNAME"], env["RI_REDIS_PASSWORD"] = c.Host, strconv.Itoa(c.Port), c.User, c.Password
		env["RI_FILES_LOGGER"], env["RI_STDOUT_LOGGER"] = "false", "false"
		env["RI_DATABASE_MANAGEMENT"] = "false"
		if c.CACertificate != "" {
			env["RI_REDIS_TLS"] = "true"
			env["RI_REDIS_TLS_CA_PATH"] = "/run/cove-database-ca.pem"
		}
	default:
		err = errors.New("unsupported database tool")
		return
	}
	if in.Credentials != nil && in.Credentials.CACertificate != "" {
		dir := filepath.Join(e.Root, "database-tools", in.ID)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return
		}
		path := filepath.Join(dir, "ca.pem")
		if err = os.WriteFile(path, []byte(in.Credentials.CACertificate), 0444); err != nil {
			return
		}
		if err = os.Chmod(path, 0444); err != nil {
			return
		}
		mounts = append(mounts, "--mount", "type=bind,source="+path+",target=/run/cove-database-ca.pem,readonly")
		if in.Tool == "adminer" {
			plugin := filepath.Join(dir, "tls.php")
			if err = os.WriteFile(plugin, []byte("<?php\nrequire_once '/var/www/html/plugins/login-ssl.php';\nreturn new AdminerLoginSsl(['mode'=>'verify-full','ca'=>'/run/cove-database-ca.pem','verify'=>true]);\n"), 0444); err != nil {
				return
			}
			if err = os.Chmod(plugin, 0444); err != nil {
				return
			}
			mounts = append(mounts, "--mount", "type=bind,source="+plugin+",target=/var/www/html/plugins-enabled/cove-tls.php,readonly")
			env["PGSSLROOTCERT"], env["PGSSLMODE"] = "/run/cove-database-ca.pem", "verify-full"
		}
	}
	for name, value := range env {
		environment = append(environment, name+"="+value)
	}
	return
}
