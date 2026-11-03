package proxy_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/soemii/orchivio-worker/internal/proxy"
)

// echoUpstream returns the received headers as JSON-less text lines.
func echoUpstream(t *testing.T) (*url.URL, *http.Header) {
	t.Helper()
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		seen.Set("X-Seen-Host", r.Host)
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u, &seen
}

func TestProxyReplacesAPIKey(t *testing.T) {
	up, seen := echoUpstream(t)
	h := proxy.New(proxy.Config{Upstream: up, APIKey: "real-key"})
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", "dummy")
	req.Header.Set("Authorization", "Bearer dummy")
	req.Header.Set("Proxy-Authorization", "Basic dummy")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if got := seen.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if seen.Get("Authorization") != "" || seen.Get("Proxy-Authorization") != "" {
		t.Errorf("leaked auth headers: %v", *seen)
	}
	if seen.Get("X-Seen-Host") != up.Host {
		t.Errorf("host = %q, want %q", seen.Get("X-Seen-Host"), up.Host)
	}
}

func TestProxyUsesOAuthBearer(t *testing.T) {
	up, seen := echoUpstream(t)
	h := proxy.New(proxy.Config{Upstream: up, OAuthToken: "tok"})
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", "dummy")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := seen.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	if seen.Get("x-api-key") != "" {
		t.Errorf("x-api-key leaked")
	}
	if got := seen.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q", got)
	}
}

func TestProxyKeepsExistingBetaHeader(t *testing.T) {
	up, seen := echoUpstream(t)
	h := proxy.New(proxy.Config{Upstream: up, OAuthToken: "tok"})
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("anthropic-beta", "custom")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := seen.Get("anthropic-beta"); got != "custom" {
		t.Errorf("anthropic-beta = %q", got)
	}
}

func TestProxyRejectsAbsoluteFormToOtherHost(t *testing.T) {
	up, seen := echoUpstream(t)
	h := proxy.New(proxy.Config{Upstream: up, APIKey: "k"})
	req := httptest.NewRequest("GET", "http://other.example/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("code = %d", rec.Code)
	}
	if len(*seen) != 0 {
		t.Errorf("upstream was contacted")
	}
}

func TestProxyRejectsConnectNotAllowed(t *testing.T) {
	up, _ := echoUpstream(t)
	h := proxy.New(proxy.Config{Upstream: up, APIKey: "k", AllowConnect: []string{"ok.example:443"}})
	req := httptest.NewRequest("CONNECT", "evil.example:443", nil)
	req.Host = "evil.example:443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestProxyAllowsConnectListed(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c) // echo
	}()
	up, _ := echoUpstream(t)
	addr := target.Addr().String()
	srv := httptest.NewServer(proxy.New(proxy.Config{Upstream: up, APIKey: "k", AllowConnect: []string{strings.ToUpper(addr)}}))
	defer srv.Close()

	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT "+addr+" HTTP/1.1\r\nHost: "+addr+"\r\n\r\n")
	const want = "HTTP/1.1 200 Connection Established\r\n\r\n"
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != want {
		t.Fatalf("handshake %q err=%v", buf, err)
	}
	io.WriteString(c, "ping")
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("tunnel %q err=%v", got, err)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	u, _ := url.Parse("https://api.anthropic.com")
	cases := map[string]proxy.Config{
		"none":         {Upstream: u},
		"both":         {Upstream: u, APIKey: "a", OAuthToken: "b"},
		"nil upstream": {APIKey: "a"},
	}
	for name, cfg := range cases {
		if err := proxy.Validate(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := proxy.Validate(proxy.Config{Upstream: u, APIKey: "a"}); err != nil {
		t.Errorf("valid config: %v", err)
	}
}
