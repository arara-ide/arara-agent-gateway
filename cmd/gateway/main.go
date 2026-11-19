package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	gateway "github.com/arara-ide/arara-agent-gateway"
	"github.com/arara-ide/arara-agent-gateway/internal/api"
	"github.com/arara-ide/arara-agent-gateway/internal/config"
	"github.com/arara-ide/arara-agent-gateway/internal/connect"
	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
	"github.com/arara-ide/arara-agent-gateway/internal/idle"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/secret"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

var version = "dev"

func main() {
	if err := runMain(); err != nil {
		fmt.Fprintln(os.Stderr, "arara-agent-gateway:", err)
		os.Exit(1)
	}
}

func logLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func runMain() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	box, err := secret.New(cfg.EncryptionKey)
	if err != nil {
		return err
	}
	engine := docker.NewClient(cfg.DockerSocket)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = engine.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("docker engine unavailable at %s: %w", cfg.DockerSocket, err)
	}

	instances := instance.NewManager(st, engine, instance.Settings{
		Image: cfg.RuntimeImage, Network: cfg.ContainerNetwork, Runtime: cfg.ContainerRuntime,
		Memory: cfg.ContainerMemory, CPUs: cfg.ContainerCPUs, Pids: cfg.ContainerPids, MaxActive: cfg.MaxActiveInstances,
	}, nil, log)
	conns := connect.New(st, box, instances, engine, nil, log)
	gh := github.New(github.Config{ClientID: cfg.GitHubClientID, ClientSecret: cfg.GitHubClientSecret, RedirectURL: cfg.GitHubRedirectURL, Scopes: cfg.GitHubScopes}, st, box, nil, nil)
	ws := github.NewWorkspace(gh, engine)
	runs := run.NewManager(st, instances, engine, conns, ws, run.Settings{MaxPerInstance: cfg.MaxAgentsPerInstance, Timeout: cfg.RunTimeout}, nil, log)
	instances.BeforeStop = func(ctx context.Context, id string) {
		conns.CancelInstance(id)
		runs.CancelInstance(id, "instance_stopped")
	}

	if err := instances.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	go idle.New(st, instances, cfg.IdleTimeout, cfg.IdleCheckInterval, log).Run(ctx)

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           api.New(api.Deps{Store: st, Engine: engine, Instances: instances, Runs: runs, Connections: conns, GitHub: gh, Workspace: ws, APIKey: cfg.APIKey, IntegrationAPIKey: cfg.IntegrationAPIKey, IdleTimeout: cfg.IdleTimeout, AutoStart: cfg.AutoStart, Web: gateway.Web(), OpenAPI: gateway.OpenAPI, Log: log, Version: version}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "port", cfg.Port, "version", version, "github", cfg.GitHubEnabled())
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
