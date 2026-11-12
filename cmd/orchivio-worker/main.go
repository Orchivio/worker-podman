// Command orchivio-worker runs the Orchivio worker subcommands.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	workerv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/worker/v1"
	"github.com/orchivio/worker-podman/internal/proxy"
	"github.com/orchivio/worker-podman/internal/runner"
	"github.com/orchivio/worker-podman/internal/sandbox"
)

const (
	defaultProxyAddr = ":8090"
	defaultUpstream  = "https://api.anthropic.com"

	readHeaderTimeout = 10 * time.Second

	defaultServerAddr     = "localhost:7070"
	defaultSandboxImage   = "localhost/orchivio-sandbox-claude:dev"
	defaultSandboxNetwork = "orchivio-sandbox"
	defaultSandboxProxy   = "http://orchivio-proxy:8090"
	defaultPollInterval   = 2 * time.Second
	podmanBinary          = "podman"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "proxy":
		if err := runProxy(); err != nil {
			slog.Error("proxy failed", "error", err)
			os.Exit(1)
		}
	case "run":
		if err := runWorker(); err != nil {
			slog.Error("worker failed", "error", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: orchivio-worker proxy|run")
	os.Exit(2)
}

func runProxy() error {
	upstream := envOr("ORCHIVIO_PROXY_UPSTREAM", defaultUpstream)
	u, err := url.Parse(upstream)
	if err != nil {
		return fmt.Errorf("parse upstream: %w", err)
	}
	cfg := proxy.Config{
		Upstream:   u,
		APIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		OAuthToken: os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"),
	}
	for _, a := range strings.Split(os.Getenv("ORCHIVIO_PROXY_ALLOW_CONNECT"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.AllowConnect = append(cfg.AllowConnect, a)
		}
	}
	if err := proxy.Validate(cfg); err != nil {
		return err
	}
	addr := envOr("ORCHIVIO_PROXY_ADDR", defaultProxyAddr)
	slog.Info("proxy listening", "addr", addr, "upstream", u.Redacted(), "allow_connect", cfg.AllowConnect)
	srv := &http.Server{
		Addr:              addr,
		Handler:           proxy.New(cfg),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          proxy.ErrorLogger(),
	}
	return srv.ListenAndServe()
}

func runWorker() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("hostname: %w", err)
	}
	poll, err := time.ParseDuration(envOr("ORCHIVIO_POLL_INTERVAL", defaultPollInterval.String()))
	if err != nil || poll <= 0 {
		return fmt.Errorf("invalid ORCHIVIO_POLL_INTERVAL")
	}
	// The work dir must be under /Users on macOS: podman machine only mounts that.
	cfg := runner.Config{
		WorkerID:     envOr("ORCHIVIO_WORKER_ID", host),
		WorkDir:      envOr("ORCHIVIO_WORK_DIR", filepath.Join(home, ".orchivio", "work")),
		Image:        envOr("ORCHIVIO_SANDBOX_IMAGE", defaultSandboxImage),
		Network:      envOr("ORCHIVIO_SANDBOX_NETWORK", defaultSandboxNetwork),
		ProxyURL:     envOr("ORCHIVIO_PROXY_URL", defaultSandboxProxy),
		PollInterval: poll,
	}
	addr := envOr("ORCHIVIO_SERVER_ADDR", defaultServerAddr)
	// ponytail: plaintext gRPC on one dev host; mTLS between worker and server comes later.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("worker starting", "server", addr, "worker", cfg.WorkerID, "work_dir", cfg.WorkDir, "image", cfg.Image)
	r := runner.New(cfg, workerv1.NewWorkerServiceClient(conn), sandbox.Podman{Binary: podmanBinary}, time.Now)
	return r.Loop(ctx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
