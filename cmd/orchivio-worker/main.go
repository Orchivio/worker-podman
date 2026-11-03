// Command orchivio-worker runs the Orchivio worker subcommands.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/soemii/orchivio-worker/internal/proxy"
)

const (
	defaultProxyAddr = ":8090"
	defaultUpstream  = "https://api.anthropic.com"

	readHeaderTimeout = 10 * time.Second
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
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: orchivio-worker proxy")
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
