package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"edgeswarm/internal/control"
	"edgeswarm/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address; use a TLS reverse proxy for public access")
	driver := flag.String("driver", "sqlite", "sqlite (development only) or pgx")
	dsn := flag.String("dsn", "data/swarm.db", "SQLite path; PostgreSQL DSN should use SWARM_DATABASE_URL")
	check := flag.Bool("check", false, "check configuration, listen address and PostgreSQL connectivity without starting the server")
	flag.Parse()
	if value := os.Getenv("SWARM_DATABASE_URL"); value != "" {
		*driver = "pgx"
		*dsn = value
	}
	var keys map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("SWARM_API_KEYS")), &keys); err != nil || len(keys) == 0 {
		return fmt.Errorf("SWARM_API_KEYS must be a JSON object mapping API key to tenant")
	}
	admin := os.Getenv("SWARM_ADMIN_TOKEN")
	if len(admin) < 32 {
		return fmt.Errorf("SWARM_ADMIN_TOKEN must contain at least 32 characters")
	}
	for key, tenant := range keys {
		if len(key) < 32 || tenant == "" || key == admin {
			return fmt.Errorf("API keys must be >=32 characters, have a tenant, and differ from admin token")
		}
	}
	var modelAccess map[string][]string
	if raw := os.Getenv("SWARM_MODEL_ACCESS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &modelAccess); err != nil || modelAccess == nil {
			return fmt.Errorf("SWARM_MODEL_ACCESS must be a JSON object mapping tenants to allowed model names")
		}
	}
	if *check {
		return checkDeployment(*listen, *driver, *dsn)
	}
	if *driver == "sqlite" {
		if err := os.MkdirAll(filepath.Dir(*dsn), 0700); err != nil {
			return err
		}
		log.Print("SQLite development mode: single control process only")
	}
	db, err := store.Open(*driver, *dsn)
	if err != nil {
		return fmt.Errorf("database startup failed (verify configuration)")
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service := control.New(db, control.Config{APIKeys: keys, AdminToken: admin, ModelAccess: modelAccess})
	server := &http.Server{Addr: *listen, Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024, BaseContext: func(net.Listener) context.Context { return ctx }}
	runDone := make(chan struct{})
	go func() { defer close(runDone); service.Run(ctx) }()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("control listening on %s; terminate TLS before public exposure", *listen)
	err = server.ListenAndServe()
	stop()
	<-runDone
	<-shutdownDone
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func checkDeployment(listen, driver, dsn string) error {
	if driver != "sqlite" && driver != "pgx" {
		return fmt.Errorf("driver must be sqlite or pgx")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen address unavailable: %w", err)
	}
	listener.Close()
	if driver == "pgx" {
		db, err := sql.Open(driver, dsn)
		if err != nil {
			return fmt.Errorf("invalid PostgreSQL connection configuration")
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("PostgreSQL connectivity check failed; verify host, credentials and TLS settings")
		}
	}
	log.Printf("control configuration and listen address checks passed (%s)", driver)
	return nil
}
