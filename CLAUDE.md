# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**cs (codespelunker)** is a command-line code search tool written in Go. It searches files recursively using boolean queries, regex, and fuzzy matching, with relevance ranking (BM25/TF-IDF). Four modes: console output, interactive TUI, HTTP server, and MCP server (stdio or Streamable HTTP).

## Build & Test Commands

```bash
go build -o cs                        # Build binary
go test ./...                         # Run all tests
go test -v ./...                      # Verbose tests
go test -run TestPreParseQuery ./...  # Run a single test
```

**Linting** (golangci-lint v2):
```bash
golangci-lint run ./...                         # Linters
golangci-lint fmt --enable gofmt --diff ./...   # Formatting check; v2 treats gofmt as a formatter, not a linter
gofmt -s -w -l .                                # Format in place
```

## Architecture

Root package `main` plus subpackages under `pkg/`. The search pipeline is orchestrated by `DoSearch()` in `search.go`:

```
FileWalker → [read + filter files] → AST query evaluation → matched FileJobs channel
```

### Query Pipeline (`pkg/search/`)

Queries are parsed into an AST and evaluated against each file:

```
Lexer → Parser → Transformer → Planner → Executor
```

- **Lexer** (`lexer.go`): tokenizes query string (terms, quotes, regex, operators, fuzzy markers)
- **Parser** (`parser.go`): builds AST with boolean logic (AND/OR/NOT), quoted phrases, regex `/pattern/`, fuzzy `~1`/`~2`
- **Transformer** (`transformer.go`): semantic rewrites (e.g. `complexity=high`)
- **Planner** (`planner.go`): query optimization
- **Executor** (`executor.go`): evaluates AST against file content, returns match locations
- **Extractor** (`extractor.go`): extracts matched terms for highlighting
- **AST** (`ast.go`): AST node type definitions (AndNode, OrNode, NotNode, KeywordNode, PhraseNode, RegexNode, FuzzyNode, FilterNode)
- **Document** (`document.go`): Document and SearchResult structs

See `pkg/search/README.md` for detailed query syntax documentation.

### Core Root Files

- **`main.go`**: Cobra CLI entry point. Routes to `ConsoleSearch()`, TUI (`initialModel()`), `StartHttpServer()`, `StartMCPServer()` or `StartMCPHTTPServer()` based on flags/args
- **`config.go`**: `Config` struct with all CLI-configurable fields. `DefaultConfig()` provides sensible defaults
- **`search.go`**: `DoSearch()` — orchestrates the full search pipeline: file walking, reading, binary/minified filtering, AST evaluation, and result streaming via channels. Uses `SearchCache` for prefix-based caching
- **`console.go`**: `ConsoleSearch()` — collects results, ranks, and outputs in text/JSON/vimgrep format
- **`tui.go`**: bubbletea TUI with lipgloss styling. Debounced search input, incremental result streaming, syntax-highlighted preview
- **`http.go`**: HTTP server with embedded templates, search/display endpoints, theme support (dark/light/bare), custom template overrides
- **`cache.go`**: `SearchCache` — LRU cache with TTL for query results; supports prefix matching for progressive refinement in TUI
- **`syntax.go`**: Syntax highlighting with keyword tables for 80+ languages
- **`language.go`**: Language detection via scc processor's language database
- **`mcp.go`**: MCP (Model Context Protocol) server; `newMCPServer()` registers the `search` and `get_file` tools for AI agents, `StartMCPServer()` serves them over stdio
- **`mcp_http.go`**: `StartMCPHTTPServer()` serves the same tools over Streamable HTTP at `/mcp` (`--mcp-http`), stateless, with optional bearer-token auth (`--mcp-http-token-file`); `validateMCPFlags()` refuses a non-loopback bind without `--mcp-lock-dir` or a token
- **`mcp_tools.go`**: navigation tools shared by both MCP transports: `search_facets` (match counts by directory/language/extension), `file_outline` (declarations via `pkg/ranker` heuristics), `list_dir`, `find_files`, `code_stats` (scc per language)
- **`mcp_git.go`**: git-backed reads: `get_file`/`file_outline` `rev` (`git cat-file blob rev:path`, revs validated so they can't be read as options) and `list_refs`
- **`mcp_related.go`**: `related_files`, which searches for a file's most distinctive identifiers and ranks the results with BM25
- **`mcp_catalogue.go`**: `list_repos` and the `tag` search parameter (web and MCP) over a `repos.csv`/`repos-meta.csv` catalogue (`--catalogue`), reloaded when the files change
- **`search_budget.go`**: server-side search limits shared by the web and MCP servers: `collectResults()` stops a search at `--max-result-files`/`--max-result-mb` and marks it partial, and a gate runs at most `--max-concurrent-searches` at once
- **`mcp_log.go`**: one `slog` line per tool call in `--mcp-http` mode
- **`templates.go`**: Template loading with `//go:embed` for built-in HTML templates; supports custom template paths

### Subpackages

- **`pkg/common/`**: `FileJob` struct — the core data type passed through the pipeline
- **`pkg/ranker/`**: `RankResults()` — BM25 (default), TF-IDF, or simple ranking with location-based boosting. Includes declaration detection (`declarations.go`), deduplication (`dedup.go`), and stopword filtering (`stopwords.go`)
- **`pkg/snippet/`**: Snippet extraction (sliding window) and line-based extraction; auto-selects mode by file type

## Key Conventions

- Dependencies are vendored (`vendor/` directory)
- SPDX license headers on all source files (MIT)
- TUI uses bubbletea + bubbles (text input) + lipgloss (styling)
- Releases via GoReleaser (`.goreleaser.yaml`)
- Go 1.26.4 (`go.mod`), module: `github.com/boyter/cs`
