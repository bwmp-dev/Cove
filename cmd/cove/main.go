package main

import (
	"context"
	"cove/internal/cli"
	"cove/internal/connect"
	"cove/internal/control"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "connect" {
		if err := connect.Run(ctx, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 {
		if err := cli.Run(ctx, os.Args[1:]); err != nil {
			var exit *cli.ExitError
			if errors.As(err, &exit) {
				os.Exit(exit.Code)
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("COVE_ENCRYPTION_KEY"))
	if err != nil {
		log.Fatal("COVE_ENCRYPTION_KEY must be base64")
	}
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("invalid database configuration")
	}
	defer db.Close()
	dir := os.Getenv("COVE_BINARY_DIR")
	if dir == "" {
		dir = "bin"
	}
	s := &control.Server{DB: db, Key: key, AdminToken: os.Getenv("COVE_ADMIN_TOKEN"), PublicURL: os.Getenv("COVE_PUBLIC_URL"), BinaryDir: dir}
	s.FrontendDir = os.Getenv("COVE_FRONTEND_DIR")
	s.LocalSocket = os.Getenv("COVE_LOCAL_SOCKET")
	s.GithubClientID = os.Getenv("COVE_GITHUB_CLIENT_ID")
	s.GithubClientSecret = os.Getenv("COVE_GITHUB_CLIENT_SECRET")
	s.SMTP = control.SMTPConfig{Host: os.Getenv("COVE_SMTP_HOST"), Port: os.Getenv("COVE_SMTP_PORT"), Username: os.Getenv("COVE_SMTP_USERNAME"), Password: os.Getenv("COVE_SMTP_PASSWORD"), From: os.Getenv("COVE_SMTP_FROM")}
	if err = s.Init(ctx); err != nil {
		log.Fatal(err)
	}
	http.DefaultTransport = s.OutboundTransport(http.DefaultTransport.(*http.Transport))
	if err = s.InitAdministrator(ctx, os.Getenv("COVE_ADMIN_PASSWORD")); err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("COVE_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	server := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		if err := s.Dispatch(ctx); err != nil {
			log.Print("operation dispatcher stopped; shutting down for recovery")
			stop()
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Print("HTTP shutdown failed")
		}
	}()
	log.Printf("Cove API listening on %s", addr)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("HTTP server failed")
	}
}
