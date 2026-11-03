// Package proxy implements the credential and egress proxy: the agent sandbox
// holds no credentials and has no direct network, so it talks to this proxy,
// which injects the model API credential and tunnels only to an allowlist.
package proxy

import (
	"errors"
	"io"
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

// Validate checks the config. New assumes a config that passed Validate.
func Validate(cfg Config) error {
	if cfg.Upstream == nil {
		return errors.New("proxy: upstream must be set")
	}
	if (cfg.APIKey == "") == (cfg.OAuthToken == "") {
		return errors.New("proxy: exactly one of API key and OAuth token must be set")
	}
	return nil
}

// New returns the proxy handler. cfg must be valid (see Validate).
func New(cfg Config) http.Handler {
	allowed := make(map[string]bool, len(cfg.AllowConnect))
	for _, a := range cfg.AllowConnect {
		allowed[strings.ToLower(a)] = true
	}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
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
			if h.Get("anthropic-beta") == "" {
				h.Set("anthropic-beta", oauthBetaValue)
			}
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
