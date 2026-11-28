package control

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"cove/internal/worker"
	"go.yaml.in/yaml/v3"
)

func databaseCAFile(database *databaseResource) string {
	host := database.PrivateHost
	if host == "" {
		host = database.Host
	}
	return "cove_ca_" + strings.TrimSuffix(strings.TrimPrefix(host, "db-"), ".internal")
}

func (s *Server) databaseBindingCAs(ctx context.Context, workload string) (map[string]string, error) {
	rows, err := s.DB.Query(ctx, `WITH RECURSIVE sources(id,path,depth) AS (
 SELECT source_id,ARRAY[workload_id,source_id],1 FROM resource_variable_bindings WHERE workload_id=$1
 UNION ALL SELECT b.source_id,s.path||b.source_id,s.depth+1 FROM sources s JOIN resource_variable_bindings b ON b.workload_id=s.id WHERE s.depth<16 AND NOT b.source_id=ANY(s.path)
) SELECT DISTINCT src.configuration FROM sources s JOIN workloads src ON src.id=s.id JOIN workloads dst ON dst.id=$1 AND dst.team_id=src.team_id`, workload)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	certificates := map[string]string{}
	for rows.Next() {
		var raw []byte
		var source workloadInput
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = s.unsealJSON(raw, &source); err != nil {
			return nil, err
		}
		if source.Database != nil && source.Database.PublicAccess != nil {
			certificates[databaseCAFile(source.Database)] = source.Environment["COVE_DB_TLS_CA"]
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(certificates) > 0 {
		var capable bool
		if err = s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'managed_databases')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, workload).Scan(&capable); err != nil {
			return nil, err
		}
		if !capable {
			return nil, errors.New("upgrade and verify the consumer worker before using database TLS certificate bindings")
		}
	}
	return certificates, nil
}

type databasePublicAccess struct {
	Hostname    string    `json:"hostname"`
	BindAddress string    `json:"bind_address"`
	Port        int       `json:"port"`
	Fingerprint string    `json:"fingerprint"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type databasePublicInput struct {
	Hostname      string `json:"hostname"`
	BindAddress   string `json:"bind_address"`
	Port          int    `json:"port"`
	Certificate   string `json:"certificate"`
	PrivateKey    string `json:"private_key"`
	CACertificate string `json:"ca_certificate"`
}

func validateDatabaseCertificate(in databasePublicInput) (*databasePublicAccess, string, error) {
	in.Hostname = strings.ToLower(strings.TrimSpace(in.Hostname))
	if !worker.ValidLinkAlias(in.Hostname) || net.ParseIP(in.Hostname) != nil || in.Port < 1024 || in.Port > 65535 {
		return nil, "", errors.New("provide a DNS hostname and public port between 1024 and 65535")
	}
	bind := net.ParseIP(in.BindAddress)
	if bind == nil || bind.IsMulticast() || bind.IsLinkLocalUnicast() {
		return nil, "", errors.New("provide the worker's bind IP address, or 0.0.0.0 or ::")
	}
	if len(in.Certificate) > 32768 || len(in.PrivateKey) > 16384 || len(in.CACertificate) > 65536 {
		return nil, "", errors.New("TLS certificate material exceeds its size limit")
	}
	for remaining := []byte(in.Certificate); len(strings.TrimSpace(string(remaining))) > 0; {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, "", errors.New("server certificate must contain certificates only")
		}
		remaining = rest
	}
	if _, err := worker.ParseTLSCertificate(in.Certificate, in.PrivateKey, time.Now(), true); err != nil {
		return nil, "", err
	}
	pair, err := tls.X509KeyPair([]byte(in.Certificate), []byte(in.PrivateKey))
	if err != nil || len(pair.Certificate) == 0 {
		return nil, "", errors.New("provide a matching PEM server certificate and private key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(in.Hostname) != nil || time.Now().Before(leaf.NotBefore) || time.Until(leaf.NotAfter) < 24*time.Hour {
		return nil, "", errors.New("server certificate must match the hostname, be valid now, and remain valid for at least 24 hours")
	}
	ca := in.CACertificate
	if ca == "" {
		ca = in.Certificate
	}
	roots := x509.NewCertPool()
	intermediates := x509.NewCertPool()
	anchors := 0
	remaining := []byte(ca)
	for len(strings.TrimSpace(string(remaining))) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			return nil, "", errors.New("CA bundle must contain valid PEM certificates only")
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			return nil, "", errors.New("CA bundle must contain certificates only")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, "", errors.New("provide a valid PEM CA certificate bundle")
		}
		if certificate.Subject.String() == certificate.Issuer.String() && certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature) == nil {
			roots.AddCert(certificate)
			anchors++
		} else {
			intermediates.AddCert(certificate)
		}
	}
	if anchors == 0 {
		return nil, "", errors.New("CA bundle must include its root certificate; a self-signed server certificate may be used directly")
	}
	for _, raw := range pair.Certificate[1:] {
		certificate, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, "", errors.New("invalid server certificate chain")
		}
		intermediates.AddCert(certificate)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{DNSName: in.Hostname, Roots: roots, Intermediates: intermediates}); err != nil {
		return nil, "", errors.New("server certificate must verify against the supplied CA bundle")
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	return &databasePublicAccess{Hostname: in.Hostname, BindAddress: bind.String(), Port: in.Port, Fingerprint: hex.EncodeToString(fingerprint[:]), ExpiresAt: leaf.NotAfter}, ca, nil
}

func isolateDatabaseCopy(config *workloadInput, id string) error {
	if config.Database == nil {
		return nil
	}
	database := *config.Database
	database.Extensions = slices.Clone(database.Extensions)
	config.Environment = maps.Clone(config.Environment)
	config.Limits = maps.Clone(config.Limits)
	config.FileSecrets = slices.Clone(config.FileSecrets)
	config.Database = &database
	database.Host, database.PrivateHost, database.SettingsOperation = databaseHostname(id), databaseHostname(id), ""
	return configureDatabasePublic(config, false, databasePublicInput{})
}

func configureDatabasePublic(config *workloadInput, enabled bool, in databasePublicInput) error {
	if config.Database == nil {
		return errors.New("select a managed database")
	}
	database := config.Database
	engine, ok := findDatabaseEngine(database.Engine)
	if !ok {
		return errors.New("unsupported database engine")
	}
	base := config.Compose
	if database.PublicAccess != nil {
		base = database.PrivateCompose
		if base == "" {
			return errors.New("retained private database configuration unavailable")
		}
	}
	var err error
	var access *databasePublicAccess
	var ca string
	if enabled {
		access, ca, err = validateDatabaseCertificate(in)
		if err != nil {
			return err
		}
	}
	var compose map[string]any
	if err = yaml.Unmarshal([]byte(base), &compose); err != nil {
		return err
	}
	service := compose["services"].(map[string]any)[database.Service].(map[string]any)
	service["image"] = database.Image
	config.FileSecrets = slices.DeleteFunc(config.FileSecrets, func(secret worker.FileSecret) bool { return strings.HasPrefix(secret.Name, "cove_db_") })
	for key := range config.Environment {
		if strings.HasPrefix(key, "COVE_DB_TLS_") || key == "COVE_DB_PUBLIC_HOST" {
			delete(config.Environment, key)
		}
	}
	if database.PrivateHost == "" {
		database.PrivateHost = database.Host
	}
	database.Host = database.PrivateHost
	database.PublicAccess = access
	database.PrivateCompose = ""
	if enabled {
		database.PrivateCompose = base
		database.Host = access.Hostname
		config.Environment["COVE_DB_PUBLIC_HOST"] = access.Hostname
		environment, ok := service["environment"].(map[string]any)
		if !ok {
			environment = map[string]any{}
			service["environment"] = environment
		}
		environment["COVE_DB_PUBLIC_HOST"] = "${COVE_DB_PUBLIC_HOST}"
		service["extra_hosts"] = []string{access.Hostname + ":127.0.0.1"}
		service["networks"] = map[string]any{"default": map[string]any{"aliases": []string{access.Hostname}}}
		secrets := map[string]string{"cert": in.Certificate, "key": in.PrivateKey, "ca": ca}
		target := engine.port
		switch engine.id {
		case "postgresql":
			secrets["hba"] = "local all all trust\nhostnossl all all 0.0.0.0/0 reject\nhostnossl all all ::/0 reject\nhostssl all all 0.0.0.0/0 scram-sha-256\nhostssl all all ::/0 scram-sha-256\n"
			service["entrypoint"] = []string{"sh", "-ec", `cp /run/secrets/cove_db_key /tmp/cove-db.key; chown postgres:postgres /tmp/cove-db.key; chmod 600 /tmp/cove-db.key; exec /usr/local/bin/docker-entrypoint.sh "$$@"`, "cove"}
			service["command"] = []string{"postgres", "-c", "ssl=on", "-c", "ssl_cert_file=/run/secrets/cove_db_cert", "-c", "ssl_key_file=/tmp/cove-db.key", "-c", "hba_file=/run/secrets/cove_db_hba"}
		case "mysql", "mariadb":
			service["command"] = []string{"--require-secure-transport=ON", "--ssl-ca=/run/secrets/cove_db_ca", "--ssl-cert=/run/secrets/cove_db_cert", "--ssl-key=/run/secrets/cove_db_key"}
		case "mongodb":
			secrets["pem"] = in.Certificate + "\n" + in.PrivateKey
			service["entrypoint"] = []string{"sh", "-ec", `cp /run/secrets/cove_db_pem /tmp/cove-db.pem; chown mongodb:mongodb /tmp/cove-db.pem; chmod 600 /tmp/cove-db.pem; exec /usr/local/bin/docker-entrypoint.sh "$$@"`, "cove"}
			service["command"] = []string{"mongod", "--tlsMode", "requireTLS", "--tlsCertificateKeyFile", "/tmp/cove-db.pem", "--tlsCAFile", "/run/secrets/cove_db_ca", "--tlsAllowConnectionsWithoutCertificates"}
			environment := service["environment"].(map[string]any)
			environment["COVE_DB_PUBLIC_HOST"] = "${COVE_DB_PUBLIC_HOST}"
			health := service["healthcheck"].(map[string]any)
			health["test"] = []string{"CMD-SHELL", `mongosh --tls --tlsCAFile /run/secrets/cove_db_ca --host "$$COVE_DB_PUBLIC_HOST" --quiet --eval 'quit(db.adminCommand({ping:1}).ok ? 0 : 1)'`}
		case "redis", "valkey":
			target = 6380
			command, ok := service["command"].([]any)
			if !ok || len(command) == 0 {
				return errors.New("cache command is incompatible with TLS")
			}
			service["command"] = append(command, "--tls-port", "6380", "--tls-cert-file", "/run/secrets/cove_db_cert", "--tls-key-file", "/run/secrets/cove_db_key", "--tls-ca-cert-file", "/run/secrets/cove_db_ca", "--tls-auth-clients", "no", "--tls-protocols", "TLSv1.2 TLSv1.3")
		case "clickhouse":
			target = 9440
			secrets["clickhouse"] = "<clickhouse><tcp_port_secure>9440</tcp_port_secure><openSSL><server><certificateFile>/run/secrets/cove_db_cert</certificateFile><privateKeyFile>/run/secrets/cove_db_key</privateKeyFile><disableProtocols>sslv2,sslv3,tlsv1,tlsv1_1</disableProtocols></server></openSSL></clickhouse>"
			service["entrypoint"] = []string{"sh", "-ec", `cp /run/secrets/cove_db_clickhouse /etc/clickhouse-server/config.d/cove-tls.xml; exec /entrypoint.sh "$$@"`, "cove"}
		}
		service["ports"] = []any{map[string]any{"host_ip": access.BindAddress, "published": strconv.Itoa(access.Port), "target": target, "protocol": "tcp"}}
		for _, name := range slices.Sorted(maps.Keys(secrets)) {
			value := secrets[name]
			variable := "COVE_DB_TLS_" + strings.ToUpper(name)
			config.Environment[variable] = value
			config.FileSecrets = append(config.FileSecrets, worker.FileSecret{Name: "cove_db_" + name, Variable: variable, Services: []string{database.Service}})
		}
	}
	encoded, err := yaml.Marshal(compose)
	if err != nil {
		return err
	}
	config.Compose = string(encoded)
	if engine.id == "postgresql" {
		if err = configureDatabaseExtensions(config); err != nil {
			return err
		}
		return configureDatabasePooler(config, database.Pooler)
	}
	return nil
}
