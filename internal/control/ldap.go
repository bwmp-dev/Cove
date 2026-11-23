package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/jackc/pgx/v5"
)

type ldapConfiguration struct {
	Name          string `json:"display_name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Transport     string `json:"transport"`
	CA            string `json:"ca_pem"`
	BindDN        string `json:"bind_dn"`
	Password      string `json:"bind_password,omitempty"`
	Base          string `json:"user_base_dn"`
	Filter        string `json:"user_filter"`
	Subject       string `json:"subject_attribute"`
	Email         string `json:"email_attribute"`
	NameAttribute string `json:"name_attribute"`
	GroupBase     string `json:"group_base_dn"`
	GroupFilter   string `json:"group_filter"`
	Registration  bool   `json:"allow_registration"`
}

type ldapProvider struct {
	Config  ldapConfiguration
	Enabled bool
	Version int64
}

var ldapAttributePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,63}$`)

func validateLDAP(c ldapConfiguration) error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 || len(c.Host) > 253 || c.Host == "" || strings.ContainsAny(c.Host, "/\\:@ \r\n\x00") && net.ParseIP(c.Host) == nil || c.Port < 1 || c.Port > 65535 || c.Transport != "ldaps" && c.Transport != "starttls" || c.Password == "" || len(c.Password) > 4096 || len(c.CA) > 65536 {
		return errors.New("valid directory name, host, port, verified TLS transport and bind credentials required")
	}
	for _, dn := range []string{c.BindDN, c.Base, c.GroupBase} {
		if len(dn) > 2048 {
			return errors.New("directory DN is too long")
		}
		if _, err := ldap.ParseDN(dn); err != nil || dn == "" {
			return errors.New("valid nonempty bind, user and group base DNs required")
		}
	}
	for _, attribute := range []string{c.Subject, c.Email, c.NameAttribute} {
		if !ldapAttributePattern.MatchString(attribute) {
			return errors.New("valid directory attribute names required")
		}
	}
	if !strings.EqualFold(c.Subject, "entryUUID") && !strings.EqualFold(c.Subject, "objectGUID") {
		return errors.New("use immutable entryUUID or objectGUID identity attributes")
	}
	if len(c.Filter) > 4096 || !strings.Contains(c.Filter, "{username}") || len(c.GroupFilter) > 4096 || !strings.Contains(c.GroupFilter, "{dn}") && !strings.Contains(c.GroupFilter, "{username}") {
		return errors.New("user and group filters must contain their escaped identity placeholders")
	}
	for _, filter := range []string{c.Filter, c.GroupFilter} {
		filter = strings.NewReplacer("{username}", "fixture", "{dn}", "uid=fixture,dc=example").Replace(filter)
		if strings.ContainsAny(filter, "{}\x00") {
			return errors.New("unsupported directory filter placeholder")
		}
		if _, err := ldap.CompileFilter(filter); err != nil {
			return errors.New("valid LDAP filters required")
		}
	}
	_, err := ldapTLSConfig(c)
	return err
}

func ldapTLSConfig(c ldapConfiguration) (*tls.Config, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("system certificate authorities unavailable")
	}
	if c.CA != "" && !roots.AppendCertsFromPEM([]byte(c.CA)) {
		return nil, errors.New("valid PEM certificate authorities required")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.Host, RootCAs: roots}, nil
}

func (s *Server) loadLDAP(ctx context.Context, tx interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (ldapProvider, error) {
	var p ldapProvider
	var raw []byte
	err := tx.QueryRow(ctx, "SELECT configuration,enabled,version FROM ldap_providers WHERE id=$1", id).Scan(&raw, &p.Enabled, &p.Version)
	if err == nil {
		raw, err = unseal(s.Key, raw)
	}
	if err == nil {
		err = json.Unmarshal(raw, &p.Config)
	}
	return p, err
}

func ldapAllowedAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	prefixes := []netip.Prefix{}
	for _, text := range strings.Split(os.Getenv("COVE_LDAP_ALLOWED_CIDRS"), ",") {
		if text = strings.TrimSpace(text); text != "" {
			prefix, err := netip.ParsePrefix(text)
			if err != nil {
				return nil, errors.New("invalid host directory network policy")
			}
			prefixes = append(prefixes, prefix)
		}
	}
	if len(prefixes) == 0 {
		return nil, errors.New("set COVE_LDAP_ALLOWED_CIDRS on the panel host before connecting a directory")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("directory address unavailable")
	}
	for _, address := range addresses {
		address = address.Unmap()
		allowed := false
		for _, prefix := range prefixes {
			allowed = allowed || prefix.Contains(address)
		}
		if !allowed || address.IsUnspecified() || address.IsMulticast() {
			return nil, errors.New("directory address is outside the host directory network policy")
		}
	}
	return addresses, nil
}

func dialLDAP(ctx context.Context, c ldapConfiguration) (*ldap.Conn, error) {
	if c.Transport != "ldaps" && c.Transport != "starttls" {
		return nil, errors.New("verified LDAPS or StartTLS is required")
	}
	addresses, err := ldapAllowedAddresses(ctx, c.Host)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := ldapTLSConfig(c)
	if err != nil {
		return nil, err
	}
	var socket net.Conn
	dialer := net.Dialer{Timeout: 5 * time.Second}
	for _, address := range addresses {
		socket, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(address.String(), strconv.Itoa(c.Port)))
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, errors.New("directory connection failed")
	}
	deadline := time.Now().Add(12 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	_ = socket.SetDeadline(deadline)
	if c.Transport == "ldaps" {
		secure := tls.Client(socket, tlsConfig)
		if err = secure.HandshakeContext(ctx); err != nil {
			socket.Close()
			return nil, errors.New("directory TLS verification failed")
		}
		socket = secure
	}
	connection := ldap.NewConn(socket, c.Transport == "ldaps")
	connection.Start()
	connection.SetTimeout(10 * time.Second)
	if c.Transport == "starttls" {
		if err = connection.StartTLS(tlsConfig); err != nil {
			connection.Close()
			return nil, errors.New("directory StartTLS verification failed")
		}
	}
	return connection, nil
}

func (s *Server) ldapPublicProviders(w http.ResponseWriter, r *http.Request) {
	out, err := s.enabledLDAPProviders(r.Context())
	if err != nil {
		http.Error(w, "directory providers unavailable", 503)
		return
	}
	respond(w, 200, out)
}

func (s *Server) enabledLDAPProviders(ctx context.Context) ([]map[string]string, error) {
	rows, err := s.DB.Query(ctx, "SELECT id,configuration FROM ldap_providers WHERE enabled ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		plain, e := unseal(s.Key, raw)
		var c ldapConfiguration
		if e != nil {
			return nil, e
		}
		if err = json.Unmarshal(plain, &c); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"id": id, "display_name": c.Name})
	}
	return out, rows.Err()
}

func (s *Server) ldapRegistrationEnabled(ctx context.Context) (bool, error) {
	rows, err := s.DB.Query(ctx, "SELECT configuration FROM ldap_providers WHERE enabled")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	registration := false
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return false, err
		}
		plain, e := unseal(s.Key, raw)
		if e != nil {
			return false, e
		}
		var c ldapConfiguration
		if err = json.Unmarshal(plain, &c); err != nil {
			return false, err
		}
		registration = registration || c.Registration
	}
	return registration, rows.Err()
}
