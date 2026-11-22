package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type SMTPConfig struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
}

type instanceIntegrations struct {
	SMTP   SMTPConfig `json:"smtp"`
	Github struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"github"`
}

func (s *Server) integrationConfig(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (instanceIntegrations, bool, error) {
	var config instanceIntegrations
	var encrypted []byte
	if err := db.QueryRow(ctx, "SELECT integration_settings FROM control_settings WHERE id=1").Scan(&encrypted); err != nil {
		return config, false, err
	}
	if encrypted == nil {
		config.SMTP = s.SMTP
		config.Github.ClientID, config.Github.ClientSecret = s.GithubClientID, s.GithubClientSecret
		return config, false, nil
	}
	plain, err := unseal(s.Key, encrypted)
	if err != nil {
		return config, true, err
	}
	err = json.Unmarshal(plain, &config)
	return config, true, err
}

func (s *Server) mailer(ctx context.Context) (func(context.Context, string, string, string) error, error) {
	config, saved, err := s.integrationConfig(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	if !saved && config.SMTP == (SMTPConfig{}) && s.SendMail != nil {
		return s.SendMail, nil
	}
	c := config.SMTP
	return SMTPMailer(c.Host, c.Port, c.Username, c.Password, c.From)
}

// Import host settings once so subsequent dashboard edits survive restarts.
func (s *Server) importIntegrationSettings(ctx context.Context) error {
	config, saved, err := s.integrationConfig(ctx, s.DB)
	if err != nil || saved {
		return err
	}
	if config.SMTP == (SMTPConfig{}) && config.Github.ClientID == "" && config.Github.ClientSecret == "" {
		return nil
	}
	if err = validateIntegrations(config); err != nil {
		return err
	}
	plain, err := json.Marshal(config)
	if err != nil {
		return err
	}
	encrypted, err := seal(s.Key, plain)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, "UPDATE control_settings SET integration_settings=$1 WHERE id=1 AND integration_settings IS NULL", encrypted)
	return err
}

func validateIntegrations(config instanceIntegrations) error {
	c := config.SMTP
	if c != (SMTPConfig{}) {
		port, err := strconv.Atoi(c.Port)
		if err != nil || port < 1 || port > 65535 || strings.TrimSpace(c.Host) != c.Host || strings.ContainsAny(c.Host, "/\\ \t\r\n") || strings.Contains(c.Host, ":") && net.ParseIP(c.Host) == nil || len(c.Host) > 253 || len(c.Username) > 254 || len(c.Password) > 4096 {
			return errors.New("valid SMTP host and port from 1 to 65535 required")
		}
		if _, err = SMTPMailer(c.Host, c.Port, c.Username, c.Password, c.From); err != nil {
			return err
		}
	}
	g := config.Github
	if (g.ClientID == "") != (g.ClientSecret == "") || len(g.ClientID) > 200 || len(g.ClientSecret) > 4096 || strings.ContainsAny(g.ClientID+g.ClientSecret, " \t\r\n") {
		return errors.New("both GitHub client ID and client secret are required")
	}
	return nil
}

func (s *Server) integrations(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		config, _, err := s.integrationConfig(r.Context(), s.DB)
		if err != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
		respondIntegrations(w, config, s.PublicURL)
		return
	}
	var in struct {
		SMTP *struct {
			Host     string  `json:"host"`
			Port     string  `json:"port"`
			Username string  `json:"username"`
			Password *string `json:"password"`
			From     string  `json:"from"`
		} `json:"smtp"`
		Github *struct {
			ClientID     string  `json:"client_id"`
			ClientSecret *string `json:"client_secret"`
		} `json:"github"`
	}
	if !decode(w, r, &in) {
		return
	}
	if (in.SMTP == nil) == (in.Github == nil) {
		http.Error(w, "update one integration at a time", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var registration bool
	if err = tx.QueryRow(r.Context(), "SELECT registration_enabled FROM control_settings WHERE id=1 FOR UPDATE").Scan(&registration); err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	config, _, err := s.integrationConfig(r.Context(), tx)
	if err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	if c := in.SMTP; c != nil {
		password := config.SMTP.Password
		if c.Password != nil {
			password = *c.Password
		}
		config.SMTP = SMTPConfig{Host: strings.TrimSpace(c.Host), Port: strings.TrimSpace(c.Port), Username: strings.TrimSpace(c.Username), Password: password, From: strings.TrimSpace(c.From)}
	}
	if g := in.Github; g != nil {
		config.Github.ClientID = strings.TrimSpace(g.ClientID)
		if g.ClientSecret != nil {
			config.Github.ClientSecret = *g.ClientSecret
		}
	}
	if err = validateIntegrations(config); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if registration && config.SMTP.Host == "" {
		http.Error(w, "close registration before disabling email delivery", 409)
		return
	}
	plain, err := json.Marshal(config)
	if err != nil {
		http.Error(w, "could not save settings", 500)
		return
	}
	encrypted, err := seal(s.Key, plain)
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE control_settings SET integration_settings=$1 WHERE id=1", encrypted)
	}
	if err == nil && in.Github != nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM oauth_states")
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save settings", 500)
		return
	}
	respondIntegrations(w, config, s.PublicURL)
}

func respondIntegrations(w http.ResponseWriter, config instanceIntegrations, publicURL string) {
	smtp := config.SMTP
	respond(w, 200, map[string]any{
		"smtp":   map[string]any{"host": smtp.Host, "port": smtp.Port, "username": smtp.Username, "from": smtp.From, "password_set": smtp.Password != ""},
		"github": map[string]any{"client_id": config.Github.ClientID, "client_secret_set": config.Github.ClientSecret != "", "callback_url": publicURL + "/auth/github/callback"},
	})
}

func (s *Server) testEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &in) {
		return
	}
	email, valid := normalizeEmail(in.Email)
	if !valid {
		http.Error(w, "valid recipient email required", 400)
		return
	}
	sendMail, err := s.mailer(r.Context())
	if err != nil {
		http.Error(w, "email settings unavailable", 503)
		return
	}
	if sendMail == nil {
		http.Error(w, "configure email delivery first", 409)
		return
	}
	if err = sendMail(r.Context(), email, "Cove email delivery test", "Your Cove instance can send email using its saved SMTP settings."); err != nil {
		http.Error(w, "email delivery failed; check the SMTP host, TLS port and credentials", 502)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
