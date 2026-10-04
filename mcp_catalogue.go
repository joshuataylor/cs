// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// list_repos answers "which repositories are there, and where?" from a
// catalogue instead of a walk, for a --dir that is a tree of repositories laid
// out as <org>/<repo>. The catalogue is two CSVs with header rows:
//
//   - repos.csv: host, org, repo, default_branch, tags (comma-separated).
//   - repos-meta.csv (optional): host, org, repo, description,
//     primary_language, topics (separated by ';' or ','), is_archived,
//     is_fork, plus any other columns, which are ignored.
//
// Both are re-read when their modification time changes, so a long-running
// server follows catalogue refreshes without a restart.

const (
	catalogueReposFile = "repos.csv"
	catalogueMetaFile  = "repos-meta.csv"
)

var mcpListReposParams = []string{"query", "org", "tag", "language", "include_archived", "limit"}

type catalogueRepo struct {
	Host          string
	Org           string
	Repo          string
	DefaultBranch string
	Tags          []string
	Description   string
	Language      string
	Topics        []string
	Archived      bool
	Fork          bool
}

// repoCatalogue loads the catalogue lazily and reloads it when either file
// changes on disk.
type repoCatalogue struct {
	dir string

	mu       sync.Mutex
	repos    []catalogueRepo
	reposMod time.Time
	metaMod  time.Time
}

func newRepoCatalogue(dir string) *repoCatalogue {
	return &repoCatalogue{dir: dir}
}

// get returns the current catalogue, reloading it if a file has changed.
func (c *repoCatalogue) get() ([]catalogueRepo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	reposInfo, err := os.Stat(filepath.Join(c.dir, catalogueReposFile))
	if err != nil {
		return nil, fmt.Errorf("catalogue: %v", err)
	}
	var metaMod time.Time
	if info, err := os.Stat(filepath.Join(c.dir, catalogueMetaFile)); err == nil {
		metaMod = info.ModTime()
	}
	if c.repos != nil && reposInfo.ModTime().Equal(c.reposMod) && metaMod.Equal(c.metaMod) {
		return c.repos, nil
	}

	repos, err := loadCatalogue(c.dir)
	if err != nil {
		return nil, err
	}
	c.repos, c.reposMod, c.metaMod = repos, reposInfo.ModTime(), metaMod
	return repos, nil
}

// readCSVByHeader reads a CSV file into rows keyed by its header names.
func readCSVByHeader(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: reading header: %v", filepath.Base(path), err)
	}
	var rows []map[string]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", filepath.Base(path), err)
		}
		row := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(rec) {
				row[name] = rec[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// splitList splits a cell on any of seps into trimmed, non-empty values.
func splitList(s, seps string) []string {
	var out []string
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func loadCatalogue(dir string) ([]catalogueRepo, error) {
	rows, err := readCSVByHeader(filepath.Join(dir, catalogueReposFile))
	if err != nil {
		return nil, fmt.Errorf("catalogue: %v", err)
	}
	key := func(row map[string]string) string {
		return strings.ToLower(row["host"] + "/" + row["org"] + "/" + row["repo"])
	}

	meta := map[string]map[string]string{}
	if metaRows, err := readCSVByHeader(filepath.Join(dir, catalogueMetaFile)); err == nil {
		for _, row := range metaRows {
			meta[key(row)] = row
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("catalogue: %v", err)
	}

	repos := make([]catalogueRepo, 0, len(rows))
	for _, row := range rows {
		if row["org"] == "" || row["repo"] == "" {
			continue
		}
		r := catalogueRepo{
			Host:          row["host"],
			Org:           row["org"],
			Repo:          row["repo"],
			DefaultBranch: row["default_branch"],
			Tags:          splitList(row["tags"], ","),
		}
		if m, ok := meta[key(row)]; ok {
			r.Description = m["description"]
			r.Language = m["primary_language"]
			r.Topics = splitList(m["topics"], ";,")
			r.Archived = strings.EqualFold(m["is_archived"], "true")
			r.Fork = strings.EqualFold(m["is_fork"], "true")
		}
		repos = append(repos, r)
	}
	return repos, nil
}

type mcpRepoEntry struct {
	Path          string   `json:"path"`
	Host          string   `json:"host"`
	Description   string   `json:"description,omitempty"`
	Language      string   `json:"language,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Topics        []string `json:"topics,omitempty"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	Archived      bool     `json:"archived,omitempty"`
	Fork          bool     `json:"fork,omitempty"`
	OnDisk        bool     `json:"on_disk"`
}

type mcpListReposResponse struct {
	Total     int            `json:"total"`
	Returned  int            `json:"returned"`
	Truncated bool           `json:"truncated"`
	Repos     []mcpRepoEntry `json:"repos"`
	Message   string         `json:"message,omitempty"`
}

func newMCPListReposTool() mcp.Tool {
	return mcp.NewTool("list_repos",
		mcp.WithDescription("Look up repositories in the catalogue of the tree this server searches, by name, description, topic, org, tag or language. "+
			"Returns each repository's 'path' (<org>/<repo>, relative to the default directory) to pass as 'path' to search, search_facets, find_files or code_stats, "+
			"plus its description, primary language, tags and whether it is on disk. Use it first when you know roughly what you want "+
			"('the MCP Go SDK', 'dbt adapters') but not where it lives: it is instant, while an unscoped search walks every repository."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("query", mcp.Description("Case-insensitive substring matched against org/repo, description and topics. Several words must all match.")),
		mcp.WithString("org", mcp.Description("Exact org (owner) name, case-insensitive.")),
		mcp.WithString("tag", mcp.Description("Catalogue tag, e.g. a language ('rust'), domain ('mcp') or curated set.")),
		mcp.WithString("language", mcp.Description("Primary language, case-insensitive (e.g. 'Go').")),
		mcp.WithBoolean("include_archived", mcp.Description("Include archived repositories. Default true.")),
		mcp.WithNumber("limit", mcp.Description("Maximum repositories, 1-500. Default 30.")),
	)
}

func mcpListReposHandler(cfg *Config, catalogue *repoCatalogue) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("list_repos", args, mcpListReposParams); res != nil {
			return res, nil
		}
		repos, err := catalogue.get()
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		base, err := defaultSearchRoot(cfg)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		words := strings.Fields(strings.ToLower(mcpStringArg(args, "query")))
		org := strings.ToLower(strings.TrimSpace(mcpStringArg(args, "org")))
		tag := strings.ToLower(strings.TrimSpace(mcpStringArg(args, "tag")))
		lang := strings.ToLower(strings.TrimSpace(mcpStringArg(args, "language")))
		includeArchived := mcpBoolArg(args, "include_archived", true)
		limit := mcpIntArg(args, "limit", 30, 1, 500)
		if len(words) == 0 && org == "" && tag == "" && lang == "" {
			return mcp.NewToolResultError("give at least one of query, org, tag or language"), nil
		}

		type hit struct {
			repo catalogueRepo
			rank int
		}
		var hits []hit
		for _, r := range repos {
			if org != "" && strings.ToLower(r.Org) != org {
				continue
			}
			if lang != "" && strings.ToLower(r.Language) != lang {
				continue
			}
			if !includeArchived && r.Archived {
				continue
			}
			if tag != "" && !containsFold(r.Tags, tag) {
				continue
			}
			name := strings.ToLower(r.Org + "/" + r.Repo)
			text := name + " " + strings.ToLower(r.Description+" "+strings.Join(r.Topics, " "))
			matched := true
			for _, w := range words {
				if !strings.Contains(text, w) {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			// Name matches before description-only matches; an exact repo name first.
			rank := 2
			if len(words) == 1 && strings.ToLower(r.Repo) == words[0] {
				rank = 0
			} else if len(words) > 0 && allIn(words, name) {
				rank = 1
			}
			hits = append(hits, hit{r, rank})
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].rank != hits[j].rank {
				return hits[i].rank < hits[j].rank
			}
			return strings.ToLower(hits[i].repo.Org+"/"+hits[i].repo.Repo) < strings.ToLower(hits[j].repo.Org+"/"+hits[j].repo.Repo)
		})

		resp := mcpListReposResponse{Total: len(hits), Repos: []mcpRepoEntry{}}
		if len(hits) > limit {
			hits = hits[:limit]
			resp.Truncated = true
			resp.Message = fmt.Sprintf("Showing %d of %d. Narrow with more query words, org, tag or language, or raise 'limit'.", limit, resp.Total)
		}
		for _, h := range hits {
			r := h.repo
			path := r.Org + "/" + r.Repo
			_, statErr := os.Stat(filepath.Join(base, r.Org, r.Repo))
			resp.Repos = append(resp.Repos, mcpRepoEntry{
				Path: path, Host: r.Host, Description: r.Description, Language: r.Language,
				Tags: r.Tags, Topics: r.Topics, DefaultBranch: r.DefaultBranch,
				Archived: r.Archived, Fork: r.Fork, OnDisk: statErr == nil,
			})
		}
		resp.Returned = len(resp.Repos)
		return mcpJSONResult(resp)
	}
}

// tagRoots returns the directories of the catalogue's repositories that
// carry tag and exist on disk under base, keeping only those inside within
// when it is set. It is what the "tag" search parameter walks.
func (c *repoCatalogue) tagRoots(base, within, tag string) ([]string, error) {
	repos, err := c.get()
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, r := range repos {
		if !containsFold(r.Tags, tag) {
			continue
		}
		dir := filepath.Join(base, r.Org, r.Repo)
		if within != "" && !withinRoot(within, dir) {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			roots = append(roots, dir)
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("no repositories with tag %q on disk under %s", tag, base)
	}
	sort.Strings(roots)
	return roots, nil
}

// resolveSearchScope turns a search's directory and tag arguments into where
// to walk: root is the directory (as resolveSearchRoot), and roots, set only
// for a tag, are that tag's repositories inside root.
func resolveSearchScope(cfg *Config, catalogue *repoCatalogue, dir, tag string) (root string, explicit bool, roots []string, err error) {
	root, explicit, err = resolveSearchRoot(cfg, dir)
	if err != nil || strings.TrimSpace(tag) == "" {
		return root, explicit, nil, err
	}
	if catalogue == nil {
		return "", false, nil, fmt.Errorf("tag needs a repository catalogue, and this server was started without --catalogue")
	}
	base, err := defaultSearchRoot(cfg)
	if err != nil {
		return "", false, nil, err
	}
	within := ""
	if explicit {
		within = root
	}
	roots, err = catalogue.tagRoots(base, within, strings.TrimSpace(tag))
	return root, explicit, roots, err
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func allIn(words []string, s string) bool {
	for _, w := range words {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}
