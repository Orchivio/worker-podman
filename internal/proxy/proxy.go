// Package proxy implements the credential and egress proxy: the agent sandbox
// holds no credentials and has no direct network, so it talks to this proxy,
// which injects the model API credential and tunnels only to an allowlist.
package proxy

import (
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	dialTimeout    = 10 * time.Second
	oauthBetaValue = "oauth-2025-04-20"
)

// Config configures the proxy. Exactly one of APIKey and OAuthToken must be set.
type Config struct {
	Upstream     *url.URL // https://api.anthropic.com
	APIKey       string   // sent as x-api-key
	OAuthToken   string   // sent as Authorization: Bearer
	AllowConnect []string // exact host:port entries for CONNECT
}

// ErrorLogger routes net/http error logs (which may contain request details)
// through slog at error level.
func ErrorLogger() *log.Logger {
	return slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)
}

// Validate checks the config. New assumes a config that passed Validate.
func Validate(cfg Config) error {
	if cfg.Upstream == nil || cfg.Upstream.Host == "" {
		return errors.New("proxy: upstream with host must be set")
	}
	if !validScheme(cfg.Upstream) {
		return errors.New("proxy: upstream must be https (http only for loopback hosts)")
	}
	if (cfg.APIKey == "") == (cfg.OAuthToken == "") {
		return errors.New("proxy: exactly one of API key and OAuth token must be set")
	}
	return nil
}

func validScheme(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// withOAuthBeta appends oauthBetaValue to the comma-separated anthropic-beta
// values (across all header lines) unless that exact token is already present.
func withOAuthBeta(h http.Header) {
	var tokens []string
	for _, line := range h.Values("anthropic-beta") {
		for _, t := range strings.Split(line, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tokens = append(tokens, t)
			}
		}
	}
	for _, t := range tokens {
		if t == oauthBetaValue {
			h.Set("anthropic-beta", strings.Join(tokens, ","))
			return
		}
	}
	h.Set("anthropic-beta", strings.Join(append(tokens, oauthBetaValue), ","))
}

// New returns the proxy handler. cfg must be valid (see Validate).
func New(cfg Config) http.Handler {
	allowed := make(map[string]bool, len(cfg.AllowConnect))
	for _, a := range cfg.AllowConnect {
		allowed[strings.ToLower(a)] = true
	}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		ErrorLog:      ErrorLogger(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Upstream)
			pr.Out.Host = cfg.Upstream.Host
			h := pr.Out.Header
			h.Del("x-api-key")
			h.Del("Authorization")
			h.Del("Proxy-Authorization")
			if cfg.APIKey != "" {
				h.Set("x-api-key", cfg.APIKey)
				return
			}
			h.Set("Authorization", "Bearer "+cfg.OAuthToken)
			withOAuthBeta(h)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodConnect:
			connect(w, r, allowed)
		case r.URL.IsAbs() && !strings.EqualFold(r.URL.Host, cfg.Upstream.Host):
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			rp.ServeHTTP(w, r)
		}
	})
}

func connect(w http.ResponseWriter, r *http.Request, allowed map[string]bool) {
	if !allowed[strings.ToLower(r.Host)] {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	target, err := net.DialTimeout("tcp", r.Host, dialTimeout)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		target.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		client.Close()
		target.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, buf); done <- struct{}{} }()
	go func() { io.Copy(client, target); done <- struct{}{} }()
	<-done // either side finished: close both, which unblocks the other copy
	client.Close()
	target.Close()
	<-done
}
