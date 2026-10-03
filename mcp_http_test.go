// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestValidateMCPHTTPAddress(t *testing.T) {
	cases := []struct {
		addr    string
		locked  bool
		authed  bool
		wantErr bool
	}{
		{"127.0.0.1:24134", false, false, false},
		{"localhost:24134", false, false, false},
		{"[::1]:24134", false, false, false},
		{"127.0.0.2:24134", false, false, false},
		{":24134", false, false, true},
		{"0.0.0.0:24134", false, false, true},
		{"192.168.1.10:24134", false, false, true},
		{":24134", true, false, false},
		{"0.0.0.0:24134", true, false, false},
		{"192.168.1.10:24134", false, true, false},
		{"192.168.1.10:24134", true, true, false},
		{"24134", false, false, true},
		{"127.0.0.1:", false, false, true},
	}
	for _, c := range cases {
		err := validateMCPHTTPAddress(c.addr, c.locked, c.authed)
		if (err != nil) != c.wantErr {
			t.Errorf("validateMCPHTTPAddress(%q, locked=%v, authed=%v) err=%v, wantErr=%v", c.addr, c.locked, c.authed, err, c.wantErr)
		}
	}
}

func TestLoadMCPHTTPToken(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("0123456789abcdef0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := loadMCPHTTPToken(good)
	if err != nil || token != "0123456789abcdef0123" {
		t.Errorf("loadMCPHTTPToken(good) = %q, %v; want trimmed token", token, err)
	}

	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMCPHTTPToken(short); err == nil {
		t.Error("loadMCPHTTPToken(short) succeeded, want error")
	}
	if _, err := loadMCPHTTPToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("loadMCPHTTPToken(missing) succeeded, want error")
	}
}

// startTestMCPHTTP serves the MCP handler over a loopback httptest server
// rooted at a temp directory holding one Go file, and returns the /mcp URL.
// A non-empty token turns on bearer authentication.
func startTestMCPHTTP(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\n\nfunc greetSpelunker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Directory = dir
	cfg.MCPLockDir = true

	mux := http.NewServeMux()
	mux.Handle(mcpHTTPEndpoint, newMCPHTTPHandler(&cfg, token))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + mcpHTTPEndpoint
}

// TestMCPHTTPRoundTrip drives the endpoint with mcp-go's client on the
// stateless 2026-07-28 revision and on the last session-based revision, and
// checks both can list tools and run a search with a valid bearer token.
func TestMCPHTTPRoundTrip(t *testing.T) {
	versions := []string{mcp.ProtocolVersion20260728, mcp.ProtocolVersion20251125}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			const token = "test-token-0123456789"
			url := startTestMCPHTTP(t, token)
			ctx := context.Background()

			tr, err := transport.NewStreamableHTTP(url,
				transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
			if err != nil {
				t.Fatal(err)
			}
			c := client.NewClient(tr, client.WithProtocolVersion(version))
			defer func() { _ = c.Close() }()
			if err := c.Start(ctx); err != nil {
				t.Fatalf("start: %v", err)
			}

			var initReq mcp.InitializeRequest
			initReq.Params.ClientInfo = mcp.Implementation{Name: "cs-test", Version: "0"}
			initRes, err := c.Initialize(ctx, initReq)
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if initRes.ProtocolVersion != version {
				t.Errorf("negotiated %q, want %q", initRes.ProtocolVersion, version)
			}
			if initRes.ServerInfo.Name != "codespelunker" {
				t.Errorf("server name %q, want codespelunker", initRes.ServerInfo.Name)
			}

			tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			names := map[string]bool{}
			for _, tool := range tools.Tools {
				names[tool.Name] = true
			}
			if !names["search"] || !names["get_file"] {
				t.Errorf("tools %v, want search and get_file", names)
			}

			var callReq mcp.CallToolRequest
			callReq.Params.Name = "search"
			callReq.Params.Arguments = map[string]any{"query": "greetSpelunker"}
			res, err := c.CallTool(ctx, callReq)
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if res.IsError || len(res.Content) == 0 {
				t.Fatalf("search failed: %+v", res)
			}
			text, ok := res.Content[0].(mcp.TextContent)
			if !ok {
				t.Fatalf("expected TextContent, got %T", res.Content[0])
			}
			var parsed mcpSearchResponse
			if err := json.Unmarshal([]byte(text.Text), &parsed); err != nil {
				t.Fatalf("result is not JSON: %v", err)
			}
			if parsed.TotalMatches != 1 || len(parsed.Results) != 1 || parsed.Results[0].Filename != "hello.go" {
				t.Errorf("want one match in hello.go, got %+v", parsed)
			}
		})
	}
}

// TestMCPHTTPRejectsForeignHost checks DNS rebinding protection is on: a
// request over a loopback connection with a non-loopback Host is refused.
func TestMCPHTTPRejectsForeignHost(t *testing.T) {
	url := startTestMCPHTTP(t, "")
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example.com"
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

// TestMCPHTTPBearerToken checks requests without the right bearer token are
// refused before reaching the MCP handler.
func TestMCPHTTPBearerToken(t *testing.T) {
	const token = "test-token-0123456789"
	url := startTestMCPHTTP(t, token)
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "Bearer not-the-token-at-all", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + token, http.StatusUnauthorized},
		{"prefix only", "Bearer " + token[:10], http.StatusUnauthorized},
		{"valid", "Bearer " + token, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := postDiscover(t, url, c.header)
			if resp.StatusCode != c.want {
				t.Errorf("status %d, want %d", resp.StatusCode, c.want)
			}
			if c.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate header")
			}
		})
	}
}

// postDiscover sends a stateless 2026-07-28 server/discover request to url,
// with the given Authorization header if non-empty. The body is closed;
// status and headers remain readable.
func postDiscover(t *testing.T, url, authorization string) *http.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "server/discover")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestValidateMCPFlags(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"defaults", func(c *Config) {}, ""},
		{"stdio mcp", func(c *Config) { c.MCPServer = true }, ""},
		{"loopback http", func(c *Config) { c.MCPHTTPAddress = "127.0.0.1:24134" }, ""},
		{"with --mcp", func(c *Config) { c.MCPHTTPAddress = "127.0.0.1:24134"; c.MCPServer = true }, "cannot be combined"},
		{"with --http-server", func(c *Config) { c.MCPHTTPAddress = "127.0.0.1:24134"; c.HttpServer = true }, "cannot be combined"},
		{"lan unprotected", func(c *Config) { c.MCPHTTPAddress = "192.168.1.10:24134" }, "not a loopback address"},
		{"lan locked", func(c *Config) { c.MCPHTTPAddress = "192.168.1.10:24134"; c.MCPLockDir = true }, ""},
		{"lan token", func(c *Config) { c.MCPHTTPAddress = "192.168.1.10:24134"; c.MCPHTTPTokenFile = "/tmp/token" }, ""},
		{"bad address", func(c *Config) { c.MCPHTTPAddress = "24134" }, "invalid address"},
		{"token without http", func(c *Config) { c.MCPHTTPTokenFile = "/tmp/token" }, "requires --mcp-http"},
		{"token with stdio", func(c *Config) { c.MCPServer = true; c.MCPHTTPTokenFile = "/tmp/token" }, "requires --mcp-http"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			c.mutate(&cfg)
			err := validateMCPFlags(&cfg)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("error %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// TestServeMCPHTTPShutsDownOnCancel checks the server built by
// newMCPHTTPServer answers requests, and that cancelling the context (what
// SIGINT/SIGTERM do in StartMCPHTTPServer) returns cleanly and closes the port.
func TestServeMCPHTTPShutsDownOnCancel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Directory = t.TempDir()
	cfg.MCPLockDir = true
	srv := newMCPHTTPServer(&cfg, "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveMCPHTTP(ctx, srv, ln) }()

	if resp := postDiscover(t, "http://"+addr+mcpHTTPEndpoint, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("server/discover status %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveMCPHTTP returned %v, want nil after cancel", err)
		}
	case <-time.After(mcpHTTPShutdownTimeout + 5*time.Second):
		t.Fatal("serveMCPHTTP did not return after cancel")
	}

	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Error("port still accepting connections after shutdown")
	}
}
