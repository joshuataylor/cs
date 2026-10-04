// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/boyter/cs/v3/pkg/common"
	"github.com/boyter/cs/v3/pkg/ranker"
	"github.com/boyter/gocodewalker"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// The tools in this file help an agent find its way around a tree before and
// between searches: count where a term lives (search_facets), see a file's
// shape without reading it (file_outline), list and locate files (list_dir,
// find_files), and profile a directory (code_stats).

// mcpRejectUnknownArgs returns an error result naming any argument not in
// allowed, or nil. Like search, the other tools fail loudly on a misspelt
// parameter instead of silently ignoring it.
func mcpRejectUnknownArgs(tool string, args map[string]any, allowed []string) *mcp.CallToolResult {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		set[a] = struct{}{}
	}
	var unknown []string
	for k := range args {
		if _, ok := set[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return mcp.NewToolResultError(fmt.Sprintf("unknown parameter(s) for %s: %s. Accepted: %s",
		tool, strings.Join(unknown, ", "), strings.Join(allowed, ", ")))
}

// mcpStringArg returns a string argument, or "" when absent or not a string.
func mcpStringArg(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

// mcpBoolArg returns a boolean argument, or def when absent.
func mcpBoolArg(args map[string]any, key string, def bool) bool {
	if b, ok := args[key].(bool); ok {
		return b
	}
	return def
}

// mcpIntArg returns a numeric argument clamped to [lo, hi], or def when
// absent. Numbers decoded from JSON arrive as float64; an in-process caller
// may pass an int.
func mcpIntArg(args map[string]any, key string, def, lo, hi int) int {
	var v int
	switch n := args[key].(type) {
	case float64:
		v = int(n)
	case int:
		v = n
	default:
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// resolveMCPFilePath turns a get_file style "path" into an absolute path:
// relative paths resolve against the default root, a leading ~ is expanded,
// and --mcp-lock-dir confines it to that root. The file need not exist (a
// revision read may name a file deleted since).
func resolveMCPFilePath(cfg *Config, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	base, err := defaultSearchRoot(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project directory: %v", err)
	}
	resolved := expandHome(path)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(base, resolved)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to resolve file path: %v", err)
	}
	// Search can name any directory, so the file tools must be able to read
	// what search returned. --mcp-lock-dir restores the single-tree rule.
	if cfg.MCPLockDir && !withinRoot(base, abs) {
		return "", fmt.Errorf("path is outside %s and this server was started with --mcp-lock-dir", base)
	}
	return abs, nil
}

// mcpWalkFiles walks dir with the same ignore rules as a search (.gitignore,
// .ignore, hidden files, --exclude-dir), calling fn for each file until fn
// returns false or ctx is cancelled. It reports whether the walk was cut
// short.
func mcpWalkFiles(ctx context.Context, cfg *Config, dir string, fn func(*gocodewalker.File) bool) bool {
	queue := make(chan *gocodewalker.File, 1000)
	walker := gocodewalker.NewParallelFileWalker([]string{dir}, queue)
	walker.IgnoreIgnoreFile = cfg.IgnoreIgnoreFile
	walker.IgnoreGitIgnore = cfg.IgnoreGitIgnore
	walker.LocationExcludePattern = cfg.LocationExcludePattern
	walker.IncludeHidden = cfg.IncludeHidden
	walker.ExcludeDirectory = cfg.PathDenylist
	go func() { _ = walker.Start() }()

	stopped := false
	for f := range queue {
		if stopped {
			continue // drain until the walker closes the queue
		}
		if ctx.Err() != nil || !fn(f) {
			stopped = true
			walker.Terminate()
		}
	}
	return stopped
}

// mcpRelPath returns path relative to root with forward slashes, or path
// itself when it is not beneath root.
func mcpRelPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

// mcpIsBinary reports whether content looks binary: a NUL byte in the first
// 10KB, the same check get_file uses.
func mcpIsBinary(content []byte) bool {
	check := content
	if len(check) > 10_000 {
		check = content[:10_000]
	}
	return bytes.IndexByte(check, 0) != -1
}

// mcpJSONResult marshals v as the tool result.
func mcpJSONResult(v any) (*mcp.CallToolResult, error) {
	result, err := mcp.NewToolResultJSON(v)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to marshal result: %v", err)), nil
	}
	return result, nil
}

// search_facets

var mcpFacetsParams = []string{"query", "path", "path_filter", "file", "include_ext", "language", "case_sensitive", "depth", "limit"}

type mcpFacet struct {
	Name    string `json:"name"`
	Files   int    `json:"files"`
	Matches int    `json:"matches"`
}

type mcpFacetsResponse struct {
	SearchedDirectory string     `json:"searched_directory"`
	TotalFiles        int        `json:"total_files"`
	TotalMatches      int        `json:"total_matches"`
	Depth             int        `json:"depth"`
	Directories       []mcpFacet `json:"by_directory"`
	Languages         []mcpFacet `json:"by_language"`
	Extensions        []mcpFacet `json:"by_extension"`
	Truncated         bool       `json:"truncated"`
	Message           string     `json:"message,omitempty"`
}

func newMCPFacetsTool() mcp.Tool {
	return mcp.NewTool("search_facets",
		mcp.WithDescription("Count where a query matches instead of returning snippets: matching files and matches grouped by directory, language and extension. "+
			"Use it before a full search to find WHICH repositories or directories use something (e.g. which projects call WithStateLess), "+
			"then run search with 'path' set to the interesting one. Accepts the same query syntax and filters as search. "+
			"'depth' sets how many directory levels to group by (default 2, e.g. org/repo in a tree of repositories)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("query", mcp.Required(), mcp.Description("The search query, same syntax as search.")),
		mcp.WithString("path", mcp.Description("Directory to search in, like search's 'path'. Defaults to the server's default directory.")),
		mcp.WithString("path_filter", mcp.Description("Restrict to files whose full path matches (substring or glob), ANDed with the whole query.")),
		mcp.WithString("file", mcp.Description("Restrict to files whose filename matches (substring or glob), ANDed with the whole query.")),
		mcp.WithString("include_ext", mcp.Description("Comma-separated file extensions to search (e.g. \"go,py\").")),
		mcp.WithString("language", mcp.Description("Comma-separated languages to search (e.g. \"Go,Python\").")),
		mcp.WithBoolean("case_sensitive", mcp.Description("Make the search case sensitive.")),
		mcp.WithNumber("depth", mcp.Description("Directory levels to group by, 1-5. Default 2.")),
		mcp.WithNumber("limit", mcp.Description("Maximum entries per facet, 1-200. Default 25.")),
	)
}

func mcpFacetsHandler(cfg *Config, cache *SearchCache) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("search_facets", args, mcpFacetsParams); res != nil {
			return res, nil
		}
		query := strings.TrimSpace(mcpStringArg(args, "query"))
		if query == "" {
			return mcp.NewToolResultError("missing required parameter: query"), nil
		}
		root, explicit, err := resolveSearchRoot(cfg, mcpStringArg(args, "path"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		depth := mcpIntArg(args, "depth", 2, 1, 5)
		limit := mcpIntArg(args, "limit", 25, 1, 200)

		searchCfg := *cfg
		searchCfg.Directory = root
		if explicit {
			searchCfg.FindRoot = false
		}
		searchCfg.Format = "json"
		searchCfg.MaxQueryChars = common.MaxQueryCharsMCP
		searchCfg.MaxQueryTerms = common.MaxQueryTermsMCP
		if s := mcpStringArg(args, "include_ext"); s != "" {
			searchCfg.AllowListExtensions = strings.Split(s, ",")
		}
		if s := mcpStringArg(args, "language"); s != "" {
			searchCfg.LanguageTypes = strings.Split(s, ",")
		}
		searchCfg.CaseSensitive = mcpBoolArg(args, "case_sensitive", searchCfg.CaseSensitive)

		composed := composeSearchQuery(query, mcpStringArg(args, "path_filter"), mcpStringArg(args, "file"))
		ch, _, searchErr := DoSearch(ctx, &searchCfg, composed, cache)
		if searchErr != nil {
			return mcp.NewToolResultError(searchErr.Error()), nil
		}

		dirs := map[string]*mcpFacet{}
		langs := map[string]*mcpFacet{}
		exts := map[string]*mcpFacet{}
		add := func(m map[string]*mcpFacet, name string, matches int) {
			f, ok := m[name]
			if !ok {
				f = &mcpFacet{Name: name}
				m[name] = f
			}
			f.Files++
			f.Matches += matches
		}

		resp := mcpFacetsResponse{SearchedDirectory: root, Depth: depth}
		for fj := range ch {
			matches := 0
			for _, locs := range fj.MatchLocations {
				matches += len(locs)
			}
			resp.TotalFiles++
			resp.TotalMatches += matches

			dir := filepath.Dir(mcpRelPath(root, fj.Location))
			parts := strings.Split(filepath.ToSlash(dir), "/")
			if len(parts) > depth {
				parts = parts[:depth]
			}
			add(dirs, strings.Join(parts, "/"), matches)

			lang := fj.Language
			if lang == "" {
				lang = "(unknown)"
			}
			add(langs, lang, matches)

			ext := gocodewalker.GetExtension(fj.Filename)
			if ext == "" || ext == fj.Filename {
				ext = "(none)"
			}
			add(exts, ext, matches)
		}

		flatten := func(m map[string]*mcpFacet) []mcpFacet {
			out := make([]mcpFacet, 0, len(m))
			for _, f := range m {
				out = append(out, *f)
			}
			sort.Slice(out, func(i, j int) bool {
				if out[i].Files != out[j].Files {
					return out[i].Files > out[j].Files
				}
				return out[i].Name < out[j].Name
			})
			if len(out) > limit {
				out = out[:limit]
				resp.Truncated = true
			}
			return out
		}
		resp.Directories = flatten(dirs)
		resp.Languages = flatten(langs)
		resp.Extensions = flatten(exts)
		if resp.Truncated {
			resp.Message = fmt.Sprintf("Some facets have more than %d entries; only the largest are shown. Raise 'limit' or narrow with 'path'.", limit)
		}
		return mcpJSONResult(resp)
	}
}

// file_outline

var mcpOutlineParams = []string{"path", "rev"}

// mcpOutlineMax caps the declarations returned for one file.
const mcpOutlineMax = 1000

type mcpOutlineEntry struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type mcpOutlineResponse struct {
	Path         string            `json:"path"`
	Rev          string            `json:"rev,omitempty"`
	Language     string            `json:"language"`
	TotalLines   int               `json:"total_lines"`
	Declarations []mcpOutlineEntry `json:"declarations"`
	Truncated    bool              `json:"truncated"`
}

func newMCPOutlineTool() mcp.Tool {
	return mcp.NewTool("file_outline",
		mcp.WithDescription("List the declarations in a file (functions, methods, types, classes, structs, interfaces, constants) with their line numbers, "+
			"without the bodies. Use it on a large file before get_file, then read only the line ranges you need with start_line/end_line. "+
			"Uses the same line-start heuristics as search's code_filter='only-declarations', so it supports: "+
			strings.Join(ranker.SupportedDeclarationLanguages(), ", ")+"."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("path", mcp.Required(), mcp.Description("File path, as for get_file.")),
		mcp.WithString("rev", mcp.Description("Optional git revision (tag, branch or commit) to outline the file as of, as for get_file.")),
	)
}

func mcpOutlineHandler(cfg *Config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("file_outline", args, mcpOutlineParams); res != nil {
			return res, nil
		}
		path := mcpStringArg(args, "path")
		rev := strings.TrimSpace(mcpStringArg(args, "rev"))
		abs, err := resolveMCPFilePath(cfg, path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		content, err := mcpReadFile(ctx, cfg, abs, rev)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if mcpIsBinary(content) {
			return mcp.NewToolResultError("file appears to be binary"), nil
		}

		lang := detectLanguage(filepath.Base(abs), content)
		if !ranker.HasDeclarationPatterns(lang) {
			if lang == "" {
				lang = "unknown"
			}
			return mcp.NewToolResultError(fmt.Sprintf("no declaration patterns for language %s; supported: %s. Use get_file instead",
				lang, strings.Join(ranker.SupportedDeclarationLanguages(), ", "))), nil
		}

		lines := strings.Split(string(content), "\n")
		resp := mcpOutlineResponse{Path: abs, Rev: rev, Language: lang, TotalLines: len(lines), Declarations: []mcpOutlineEntry{}}
		for i, line := range lines {
			trimmed := bytes.TrimSpace([]byte(line))
			if len(trimmed) == 0 || !ranker.IsDeclarationLine(trimmed, lang) {
				continue
			}
			if len(resp.Declarations) == mcpOutlineMax {
				resp.Truncated = true
				break
			}
			text := string(trimmed)
			if len(text) > 200 {
				text = text[:200] + "..."
			}
			resp.Declarations = append(resp.Declarations, mcpOutlineEntry{Line: i + 1, Text: text})
		}
		return mcpJSONResult(resp)
	}
}

// list_dir

var mcpListDirParams = []string{"path", "depth", "include_hidden", "limit"}

type mcpDirEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

type mcpListDirResponse struct {
	Directory string        `json:"directory"`
	Depth     int           `json:"depth"`
	Entries   []mcpDirEntry `json:"entries"`
	Truncated bool          `json:"truncated"`
	Message   string        `json:"message,omitempty"`
}

func newMCPListDirTool() mcp.Tool {
	return mcp.NewTool("list_dir",
		mcp.WithDescription("List a directory's contents to a given depth: subdirectories and files with sizes, paths relative to the directory. "+
			"Use it to orient yourself in a repository (top-level layout, where the source lives) before searching or reading. "+
			"Lists what is on disk: .git and, unless include_hidden is set, other dot-entries are skipped, but .gitignore is not applied."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("path", mcp.Description("Directory to list. Defaults to the server's default directory; relative paths resolve against it.")),
		mcp.WithNumber("depth", mcp.Description("Levels to descend, 1-4. Default 1.")),
		mcp.WithBoolean("include_hidden", mcp.Description("Include dot-files and dot-directories other than .git.")),
		mcp.WithNumber("limit", mcp.Description("Maximum entries, 1-2000. Default 200.")),
	)
}

func mcpListDirHandler(cfg *Config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("list_dir", args, mcpListDirParams); res != nil {
			return res, nil
		}
		root, _, err := resolveSearchRoot(cfg, mcpStringArg(args, "path"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		depth := mcpIntArg(args, "depth", 1, 1, 4)
		limit := mcpIntArg(args, "limit", 200, 1, 2000)
		includeHidden := mcpBoolArg(args, "include_hidden", false)

		resp := mcpListDirResponse{Directory: root, Depth: depth, Entries: []mcpDirEntry{}}
		var walk func(dir string, level int) bool
		walk = func(dir string, level int) bool {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return true // unreadable subdirectory: list what we can
			}
			for _, e := range entries {
				if ctx.Err() != nil {
					return false
				}
				name := e.Name()
				if name == ".git" || (!includeHidden && strings.HasPrefix(name, ".")) {
					continue
				}
				if len(resp.Entries) == limit {
					resp.Truncated = true
					return false
				}
				full := filepath.Join(dir, name)
				entry := mcpDirEntry{Path: mcpRelPath(root, full), Type: "file"}
				if e.IsDir() {
					entry.Type = "dir"
				} else if e.Type()&os.ModeSymlink != 0 {
					entry.Type = "symlink"
				} else if info, err := e.Info(); err == nil {
					entry.Size = info.Size()
				}
				resp.Entries = append(resp.Entries, entry)
				if e.IsDir() && level < depth {
					if !walk(full, level+1) {
						return false
					}
				}
			}
			return true
		}
		walk(root, 1)
		if resp.Truncated {
			resp.Message = fmt.Sprintf("Stopped at %d entries. Raise 'limit', lower 'depth', or list a subdirectory.", limit)
		}
		return mcpJSONResult(resp)
	}
}

// find_files

var mcpFindFilesParams = []string{"pattern", "path", "limit"}

type mcpFindFilesResponse struct {
	SearchedDirectory string   `json:"searched_directory"`
	Pattern           string   `json:"pattern"`
	Files             []string `json:"files"`
	Truncated         bool     `json:"truncated"`
	Message           string   `json:"message,omitempty"`
}

func newMCPFindFilesTool() mcp.Tool {
	return mcp.NewTool("find_files",
		mcp.WithDescription("Find files by NAME, without searching their contents: e.g. pattern='streamable.go', '*_test.go' or 'Dockerfile'. "+
			"A pattern containing * ? or [ is a glob matched against the filename; anything else is a case-insensitive substring. "+
			"Respects .gitignore and the same ignore rules as search. Paths are relative to the searched directory. "+
			"Scope with 'path' where you can: there is no index, so a large tree takes a while to walk."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Filename glob or substring.")),
		mcp.WithString("path", mcp.Description("Directory to search in. Defaults to the server's default directory.")),
		mcp.WithNumber("limit", mcp.Description("Maximum files to return, 1-1000. Default 100. The walk stops once it is reached.")),
	)
}

// mcpFilenameMatcher returns a case-insensitive matcher for a find_files
// pattern: a glob when it has glob characters, else a substring.
func mcpFilenameMatcher(pattern string) (func(name string) bool, error) {
	p := strings.ToLower(pattern)
	if strings.ContainsAny(p, "*?[") {
		if _, err := filepath.Match(p, ""); err != nil {
			return nil, fmt.Errorf("invalid glob %q: %v", pattern, err)
		}
		return func(name string) bool {
			ok, _ := filepath.Match(p, strings.ToLower(name))
			return ok
		}, nil
	}
	return func(name string) bool { return strings.Contains(strings.ToLower(name), p) }, nil
}

func mcpFindFilesHandler(cfg *Config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("find_files", args, mcpFindFilesParams); res != nil {
			return res, nil
		}
		pattern := strings.TrimSpace(mcpStringArg(args, "pattern"))
		if pattern == "" {
			return mcp.NewToolResultError("missing required parameter: pattern"), nil
		}
		match, err := mcpFilenameMatcher(pattern)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		root, _, err := resolveSearchRoot(cfg, mcpStringArg(args, "path"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		limit := mcpIntArg(args, "limit", 100, 1, 1000)

		resp := mcpFindFilesResponse{SearchedDirectory: root, Pattern: pattern, Files: []string{}}
		mcpWalkFiles(ctx, cfg, root, func(f *gocodewalker.File) bool {
			if !match(f.Filename) {
				return true
			}
			if len(resp.Files) == limit {
				resp.Truncated = true
				return false
			}
			resp.Files = append(resp.Files, mcpRelPath(root, f.Location))
			return true
		})
		if ctx.Err() != nil {
			return mcp.NewToolResultError("find_files cancelled before the walk finished"), nil
		}
		sort.Strings(resp.Files)
		if resp.Truncated {
			resp.Message = fmt.Sprintf("Stopped after %d files. Raise 'limit' or narrow the pattern or 'path'.", limit)
		}
		return mcpJSONResult(resp)
	}
}

// code_stats

var mcpCodeStatsParams = []string{"path", "top"}

// mcpCodeStatsMaxFiles bounds a code_stats walk so an unscoped call on a huge
// tree returns partial totals instead of running for minutes.
const mcpCodeStatsMaxFiles = 100_000

type mcpLanguageStats struct {
	Language   string `json:"language"`
	Files      int    `json:"files"`
	Lines      int64  `json:"lines"`
	Code       int64  `json:"code"`
	Comment    int64  `json:"comment"`
	Blank      int64  `json:"blank"`
	Complexity int64  `json:"complexity"`
}

type mcpComplexFile struct {
	Path       string `json:"path"`
	Language   string `json:"language"`
	Code       int64  `json:"code"`
	Complexity int64  `json:"complexity"`
}

type mcpCodeStatsResponse struct {
	Path        string             `json:"path"`
	Files       int                `json:"files"`
	Lines       int64              `json:"lines"`
	Code        int64              `json:"code"`
	Comment     int64              `json:"comment"`
	Blank       int64              `json:"blank"`
	Complexity  int64              `json:"complexity"`
	Languages   []mcpLanguageStats `json:"languages"`
	MostComplex []mcpComplexFile   `json:"most_complex"`
	Truncated   bool               `json:"truncated"`
	Message     string             `json:"message,omitempty"`
}

func newMCPCodeStatsTool() mcp.Tool {
	return mcp.NewTool("code_stats",
		mcp.WithDescription("Profile a directory or file with scc: lines, code, comments, blanks and cyclomatic complexity per language, plus the most complex files. "+
			"Use it to size up an unfamiliar repository (what it is written in, how big, where the complicated code is) before reading it. "+
			"Point 'path' at one repository: every file is read, so a very large tree is slow (and stops at "+fmt.Sprint(mcpCodeStatsMaxFiles)+" files)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("path", mcp.Description("Directory or file to profile. Defaults to the server's default directory.")),
		mcp.WithNumber("top", mcp.Description("How many of the most complex files to list, 0-50. Default 10.")),
	)
}

func mcpCodeStatsHandler(cfg *Config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("code_stats", args, mcpCodeStatsParams); res != nil {
			return res, nil
		}
		top := mcpIntArg(args, "top", 10, 0, 50)
		raw := mcpStringArg(args, "path")

		// A file profiles just itself; anything else must be a directory.
		var root string
		var single string
		if raw != "" {
			if abs, err := resolveMCPFilePath(cfg, raw); err == nil {
				if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
					single = abs
					root = filepath.Dir(abs)
				}
			}
		}
		if single == "" {
			dir, _, err := resolveSearchRoot(cfg, raw)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			root = dir
		}

		type fileStats struct {
			path, lang                              string
			lines, code, comment, blank, complexity int64
		}
		results := make(chan fileStats, 256)
		paths := make(chan string, 256)

		var wg sync.WaitGroup
		for range runtime.NumCPU() {
			wg.Go(func() {
				for p := range paths {
					content, err := readFileContent(p, cfg.MaxReadSizeBytes)
					if err != nil || len(content) == 0 || mcpIsBinary(content) {
						continue
					}
					lang, lines, code, comment, blank, complexity, _ := fileCodeStats(filepath.Base(p), content)
					if lang == "" {
						continue
					}
					results <- fileStats{p, lang, lines, code, comment, blank, complexity}
				}
			})
		}

		resp := mcpCodeStatsResponse{Path: root, Languages: []mcpLanguageStats{}, MostComplex: []mcpComplexFile{}}
		go func() {
			if single != "" {
				resp.Path = single
				paths <- single
			} else {
				walked := 0
				mcpWalkFiles(ctx, cfg, root, func(f *gocodewalker.File) bool {
					if walked == mcpCodeStatsMaxFiles {
						resp.Truncated = true
						return false
					}
					walked++
					paths <- f.Location
					return true
				})
			}
			close(paths)
			wg.Wait()
			close(results)
		}()

		byLang := map[string]*mcpLanguageStats{}
		var files []mcpComplexFile
		for r := range results {
			s, ok := byLang[r.lang]
			if !ok {
				s = &mcpLanguageStats{Language: r.lang}
				byLang[r.lang] = s
			}
			s.Files++
			s.Lines += r.lines
			s.Code += r.code
			s.Comment += r.comment
			s.Blank += r.blank
			s.Complexity += r.complexity
			resp.Files++
			resp.Lines += r.lines
			resp.Code += r.code
			resp.Comment += r.comment
			resp.Blank += r.blank
			resp.Complexity += r.complexity
			if top > 0 && r.complexity > 0 {
				files = append(files, mcpComplexFile{Path: mcpRelPath(root, r.path), Language: r.lang, Code: r.code, Complexity: r.complexity})
			}
		}
		if ctx.Err() != nil {
			return mcp.NewToolResultError("code_stats cancelled before the walk finished"), nil
		}

		for _, s := range byLang {
			resp.Languages = append(resp.Languages, *s)
		}
		sort.Slice(resp.Languages, func(i, j int) bool {
			if resp.Languages[i].Code != resp.Languages[j].Code {
				return resp.Languages[i].Code > resp.Languages[j].Code
			}
			return resp.Languages[i].Language < resp.Languages[j].Language
		})
		sort.Slice(files, func(i, j int) bool {
			if files[i].Complexity != files[j].Complexity {
				return files[i].Complexity > files[j].Complexity
			}
			return files[i].Path < files[j].Path
		})
		if len(files) > top {
			files = files[:top]
		}
		resp.MostComplex = append(resp.MostComplex, files...)
		if resp.Truncated {
			resp.Message = fmt.Sprintf("Stopped after %d files; totals are partial. Point 'path' at a single repository.", mcpCodeStatsMaxFiles)
		}
		return mcpJSONResult(resp)
	}
}
