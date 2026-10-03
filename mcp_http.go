// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// mcpHTTPEndpoint is the path the Streamable HTTP transport is served on.
const mcpHTTPEndpoint = "/mcp"

// validateMCPHTTPAddress checks a --mcp-http listen address. Binding anywhere
// other than loopback needs some protection: --mcp-lock-dir confines what a
// caller can read to --dir, and a bearer token keeps out callers without it.
// Unprotected, anyone who can reach the port could read any file the process
// can via get_file. An empty host (":24134") listens on every interface and
// counts as non-loopback.
func validateMCPHTTPAddress(addr string, locked, authed bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--mcp-http: invalid address %q, want host:port (e.g. 127.0.0.1:24134): %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("--mcp-http: address %q has no port", addr)
	}
	if isLoopbackHost(host) || locked || authed {
		return nil
	}
	return fmt.Errorf("--mcp-http: %q is not a loopback address; listening on it requires --mcp-lock-dir and/or "+
		"--mcp-http-token-file (or bind 127.0.0.1 and reach it through an SSH tunnel)", addr)
}

// validateMCPFlags checks the --mcp-http flags against the rest of the
// configuration, so a bad combination fails before any mode starts.
func validateMCPFlags(cfg *Config) error {
	if cfg.MCPHTTPAddress != "" {
		if cfg.MCPServer || cfg.HttpServer {
			return errors.New("--mcp-http cannot be combined with --mcp or --http-server")
		}
		if err := validateMCPHTTPAddress(cfg.MCPHTTPAddress, cfg.MCPLockDir, cfg.MCPHTTPTokenFile != ""); err != nil {
			return err
		}
	}
	if cfg.MCPHTTPTokenFile != "" && cfg.MCPHTTPAddress == "" {
		return errors.New("--mcp-http-token-file requires --mcp-http")
	}
	return nil
}

// minMCPHTTPTokenLen is the shortest bearer token accepted, to rule out
// placeholder or guessable values.
const minMCPHTTPTokenLen = 16

// loadMCPHTTPToken reads the bearer token from path, trimming surrounding
// whitespace so a trailing newline from `openssl rand -hex 32 > file` is fine.
func loadMCPHTTPToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--mcp-http-token-file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if len(token) < minMCPHTTPTokenLen {
		return "", fmt.Errorf("--mcp-http-token-file: token in %s is shorter than %d characters; generate one with `openssl rand -hex 32`",
			path, minMCPHTTPTokenLen)
	}
	return token, nil
}

// requireBearerToken wraps next so that only requests carrying
// "Authorization: Bearer <token>" reach it; anything else gets a 401. The
// comparison hashes both sides first so it is constant-time regardless of
// the presented token's length.
func requireBearerToken(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(strings.TrimSpace(presented)))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="codespelunker"`)
			http.Error(w, "unauthorised: missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether host (no port) is "localhost" or a loopback IP.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}

// newMCPHTTPHandler returns the Streamable HTTP handler for the MCP server.
//
// It runs stateless: no Mcp-Session-Id is issued or required. Clients on the
// 2026-07-28 revision are stateless by definition, and earlier clients work
// because every cs tool call is self-contained, so each request is handled as
// its own session. mcp-go picks the protocol era per request. DNS rebinding
// protection stays on: a request over a loopback connection must carry a
// loopback Host header, which an SSH-forwarded 127.0.0.1 URL does. A non-empty
// token puts bearer authentication in front of everything.
func newMCPHTTPHandler(cfg *Config, token string) http.Handler {
	h := http.Handler(server.NewStreamableHTTPServer(newMCPServer(cfg), server.WithStateLess(true)))
	if token != "" {
		h = requireBearerToken(token, h)
	}
	return h
}

// mcpHTTPShutdownTimeout is how long in-flight requests get to finish once
// the server is asked to stop.
const mcpHTTPShutdownTimeout = 5 * time.Second

// newMCPHTTPServer returns the http.Server for --mcp-http, with the MCP
// handler mounted at mcpHTTPEndpoint.
func newMCPHTTPServer(cfg *Config, token string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle(mcpHTTPEndpoint, newMCPHTTPHandler(cfg, token))
	return &http.Server{
		Addr:              cfg.MCPHTTPAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a broad search can legitimately take a while.
	}
}

// serveMCPHTTP serves srv on ln until ctx is cancelled, then shuts it down,
// giving in-flight requests mcpHTTPShutdownTimeout to finish. It returns nil
// after a clean shutdown, and the serve or shutdown error otherwise.
func serveMCPHTTP(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// StartMCPHTTPServer serves MCP over Streamable HTTP on cfg.MCPHTTPAddress
// until SIGINT or SIGTERM, then shuts down gracefully.
func StartMCPHTTPServer(cfg *Config) {
	token := ""
	if cfg.MCPHTTPTokenFile != "" {
		var err error
		if token, err = loadMCPHTTPToken(cfg.MCPHTTPTokenFile); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	srv := newMCPHTTPServer(cfg, token)

	ln, err := net.Listen("tcp", cfg.MCPHTTPAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP HTTP server error: %v\n", err)
		os.Exit(1)
	}

	root, _ := defaultSearchRoot(cfg)
	lock := ""
	if cfg.MCPLockDir {
		lock = " (locked to it)"
	}
	auth := "no authentication"
	if token != "" {
		auth = "bearer token required"
	}
	fmt.Fprintf(os.Stderr, "cs-mcp: serving MCP on http://%s%s, default directory %s%s, %s\n",
		ln.Addr(), mcpHTTPEndpoint, root, lock, auth)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serveMCPHTTP(ctx, srv, ln); err != nil {
		fmt.Fprintf(os.Stderr, "MCP HTTP server error: %v\n", err)
		os.Exit(1)
	}
}
