// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mcpLoggedArgs are the tool arguments worth a place in the call log, in the
// order they are written. Anything else (snippet sizes, ranking knobs) is
// left out to keep one call to one readable line.
var mcpLoggedArgs = []string{"query", "path", "path_filter", "file", "language", "include_ext", "snippet_mode", "start_line", "end_line"}

// mcpRemoteAddrKey is the context key for the HTTP client's address.
type mcpRemoteAddrKey struct{}

// withMCPRemoteAddr is a server.HTTPContextFunc that records the request's
// remote address, so tool middleware can log which machine made the call.
func withMCPRemoteAddr(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, mcpRemoteAddrKey{}, r.RemoteAddr)
}

// mcpCallLogMiddleware logs one line per tool call: the tool, the calling
// client, the interesting arguments, how long it took, the outcome and, for
// search, the match count. It should be the outermost middleware, so a panic
// turned into an error by server.WithRecovery is logged as an error.
func mcpCallLogMiddleware(logger *slog.Logger) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			result, err := next(ctx, request)
			logger.LogAttrs(ctx, slog.LevelInfo, "mcp.tool", mcpCallLogAttrs(ctx, request, result, err, time.Since(start))...)
			return result, err
		}
	}
}

// mcpCallLogAttrs builds the attributes for one call log line.
func mcpCallLogAttrs(ctx context.Context, request mcp.CallToolRequest, result *mcp.CallToolResult, err error, elapsed time.Duration) []slog.Attr {
	attrs := []slog.Attr{slog.String("tool", request.Params.Name)}

	if name := mcpClientName(ctx, request); name != "" {
		attrs = append(attrs, slog.String("client", name))
	}
	if addr, ok := ctx.Value(mcpRemoteAddrKey{}).(string); ok && addr != "" {
		attrs = append(attrs, slog.String("remote", addr))
	}

	args := request.GetArguments()
	for _, key := range mcpLoggedArgs {
		if v, ok := args[key]; ok {
			attrs = append(attrs, slog.Any(key, v))
		}
	}

	attrs = append(attrs, slog.Float64("duration_s", elapsed.Seconds()))

	switch {
	case err != nil:
		attrs = append(attrs, slog.String("outcome", "error"), slog.String("error", err.Error()))
	case ctx.Err() != nil:
		attrs = append(attrs, slog.String("outcome", "cancelled"))
	case result != nil && result.IsError:
		attrs = append(attrs, slog.String("outcome", "error_result"))
		if text := mcpResultText(result); text != "" {
			attrs = append(attrs, slog.String("error", text))
		}
	default:
		attrs = append(attrs, slog.String("outcome", "ok"))
		if matches, ok := mcpSearchMatches(result); ok {
			attrs = append(attrs, slog.Int("matches", matches))
		}
	}
	return attrs
}

// mcpClientName returns "name version" for the calling client: from the
// session, where mcp-go records both initialize clients and 2026-07-28
// clients that identify themselves per request, else from the request's own
// _meta. It is empty when the client did not say.
func mcpClientName(ctx context.Context, request mcp.CallToolRequest) string {
	var info mcp.Implementation
	if session, ok := server.ClientSessionFromContext(ctx).(server.SessionWithClientInfo); ok {
		info = session.GetClientInfo()
	}
	if info.Name == "" && request.Params.Meta != nil {
		if fromMeta := request.Params.Meta.ClientInfo(); fromMeta != nil {
			info = *fromMeta
		}
	}
	if info.Version == "" {
		return info.Name
	}
	return info.Name + " " + info.Version
}

// mcpResultText returns the first text content of a result, which is where
// both cs tools put their error messages.
func mcpResultText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	if text, ok := result.Content[0].(mcp.TextContent); ok {
		return text.Text
	}
	return ""
}

// mcpSearchMatches extracts total_matches from a search result's JSON body.
func mcpSearchMatches(result *mcp.CallToolResult) (int, bool) {
	text := mcpResultText(result)
	if text == "" {
		return 0, false
	}
	var parsed struct {
		TotalMatches *int `json:"total_matches"`
	}
	if json.Unmarshal([]byte(text), &parsed) != nil || parsed.TotalMatches == nil {
		return 0, false
	}
	return *parsed.TotalMatches, true
}
