// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// toolFixture is a small tree laid out like a corpus of repositories:
//
//	acme/widgets/main.go        WidgetRegistry, registerWidget, lookupWidget
//	acme/widgets/main_test.go   uses WidgetRegistry and registerWidget
//	acme/gadgets/lib.py         unrelated Python
//	other/thing/x.go            mentions WidgetRegistry once
//	acme/widgets/.hidden        dot-file
const fixtureMainGo = `package widgets

// WidgetRegistry holds widgets by name.
type WidgetRegistry struct {
	widgets map[string]int
}

func NewWidgetRegistry() *WidgetRegistry {
	return &WidgetRegistry{widgets: map[string]int{}}
}

func (r *WidgetRegistry) registerWidget(name string) {
	r.widgets[name]++
}

func (r *WidgetRegistry) lookupWidget(name string) int {
	return r.widgets[name]
}
`

const fixtureTestGo = `package widgets

import "testing"

func TestRegisterWidget(t *testing.T) {
	reg := NewWidgetRegistry()
	reg.registerWidget("gear")
	if reg.lookupWidget("gear") != 1 {
		t.Fatal("want 1")
	}
}
`

func writeToolFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"acme/widgets/main.go":      fixtureMainGo,
		"acme/widgets/main_test.go": fixtureTestGo,
		"acme/widgets/.hidden":      "secret\n",
		"acme/gadgets/lib.py":       "def spin_gadget(speed):\n    return speed * 2\n",
		"other/thing/x.go":          "package thing\n\n// See WidgetRegistry in acme.\nvar unrelatedValue = 1\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func toolConfig(root string) *Config {
	cfg := DefaultConfig()
	cfg.Directory = root
	cfg.MCPLockDir = true
	return &cfg
}

// callTool runs a handler and returns its text, failing on a Go error. It
// does not fail on an error result: wantErr says which one is expected.
func callTool(t *testing.T, h server.ToolHandlerFunc, args map[string]any, wantErr bool) string {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	text := mcpResultText(res)
	if res.IsError != wantErr {
		t.Fatalf("IsError=%v, want %v: %s", res.IsError, wantErr, text)
	}
	return text
}

func decodeTool[T any](t *testing.T, text string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return v
}

func TestSearchFacets(t *testing.T) {
	root := writeToolFixture(t)
	h := mcpFacetsHandler(toolConfig(root), NewSearchCache())

	resp := decodeTool[mcpFacetsResponse](t, callTool(t, h, map[string]any{"query": "WidgetRegistry"}, false))
	if resp.TotalFiles != 3 {
		t.Fatalf("total_files %d, want 3: %+v", resp.TotalFiles, resp)
	}
	want := map[string]int{"acme/widgets": 2, "other/thing": 1}
	for _, d := range resp.Directories {
		if want[d.Name] != d.Files {
			t.Errorf("by_directory %s: %d files, want %d", d.Name, d.Files, want[d.Name])
		}
		delete(want, d.Name)
	}
	if len(want) != 0 {
		t.Errorf("by_directory missing %v: %+v", want, resp.Directories)
	}
	if len(resp.Languages) != 1 || resp.Languages[0].Name != "Go" || resp.Languages[0].Files != 3 {
		t.Errorf("by_language %+v, want Go x3", resp.Languages)
	}
	if resp.Directories[0].Name != "acme/widgets" || resp.Directories[0].Matches <= resp.Directories[0].Files {
		t.Errorf("acme/widgets should lead with more matches than files: %+v", resp.Directories)
	}

	shallow := decodeTool[mcpFacetsResponse](t, callTool(t, h, map[string]any{"query": "WidgetRegistry", "depth": 1}, false))
	if len(shallow.Directories) != 2 || shallow.Directories[0].Name != "acme" {
		t.Errorf("depth 1 by_directory %+v, want acme then other", shallow.Directories)
	}

	callTool(t, h, map[string]any{}, true)
	callTool(t, h, map[string]any{"query": "x", "ext": "go"}, true)
}

func TestFileOutline(t *testing.T) {
	root := writeToolFixture(t)
	h := mcpOutlineHandler(toolConfig(root))

	resp := decodeTool[mcpOutlineResponse](t, callTool(t, h, map[string]any{"path": "acme/widgets/main.go"}, false))
	if resp.Language != "Go" {
		t.Fatalf("language %q, want Go", resp.Language)
	}
	got := map[int]string{}
	for _, d := range resp.Declarations {
		got[d.Line] = d.Text
	}
	for line, prefix := range map[int]string{4: "type WidgetRegistry struct", 8: "func NewWidgetRegistry()", 12: "func (r *WidgetRegistry) registerWidget", 16: "func (r *WidgetRegistry) lookupWidget"} {
		if !strings.HasPrefix(got[line], prefix) {
			t.Errorf("line %d: %q, want prefix %q (all: %v)", line, got[line], prefix, got)
		}
	}

	callTool(t, h, map[string]any{"path": "acme/widgets/.hidden"}, true)
	callTool(t, h, map[string]any{"path": "../outside.go"}, true)
}

func TestListDir(t *testing.T) {
	root := writeToolFixture(t)
	h := mcpListDirHandler(toolConfig(root))

	top := decodeTool[mcpListDirResponse](t, callTool(t, h, map[string]any{}, false))
	if len(top.Entries) != 2 || top.Entries[0].Path != "acme" || top.Entries[0].Type != "dir" || top.Entries[1].Path != "other" {
		t.Errorf("depth 1 entries %+v, want acme, other", top.Entries)
	}

	deep := decodeTool[mcpListDirResponse](t, callTool(t, h, map[string]any{"path": "acme", "depth": 2}, false))
	paths := map[string]mcpDirEntry{}
	for _, e := range deep.Entries {
		paths[e.Path] = e
	}
	if e, ok := paths["widgets/main.go"]; !ok || e.Type != "file" || e.Size == 0 {
		t.Errorf("want widgets/main.go with a size, got %+v", deep.Entries)
	}
	if _, ok := paths["widgets/.hidden"]; ok {
		t.Error("dot-file listed without include_hidden")
	}

	hidden := decodeTool[mcpListDirResponse](t, callTool(t, h, map[string]any{"path": "acme/widgets", "include_hidden": true}, false))
	found := false
	for _, e := range hidden.Entries {
		found = found || e.Path == ".hidden"
	}
	if !found {
		t.Errorf("include_hidden did not list .hidden: %+v", hidden.Entries)
	}

	limited := decodeTool[mcpListDirResponse](t, callTool(t, h, map[string]any{"depth": 3, "limit": 2}, false))
	if len(limited.Entries) != 2 || !limited.Truncated {
		t.Errorf("limit 2: %d entries, truncated=%v", len(limited.Entries), limited.Truncated)
	}
	callTool(t, h, map[string]any{"path": "acme/widgets/main.go"}, true)
}

func TestFindFiles(t *testing.T) {
	root := writeToolFixture(t)
	h := mcpFindFilesHandler(toolConfig(root))

	glob := decodeTool[mcpFindFilesResponse](t, callTool(t, h, map[string]any{"pattern": "*_TEST.go"}, false))
	if len(glob.Files) != 1 || glob.Files[0] != "acme/widgets/main_test.go" {
		t.Errorf("glob files %v", glob.Files)
	}
	sub := decodeTool[mcpFindFilesResponse](t, callTool(t, h, map[string]any{"pattern": "lib", "path": "acme"}, false))
	if len(sub.Files) != 1 || sub.Files[0] != "gadgets/lib.py" {
		t.Errorf("substring files %v", sub.Files)
	}
	limited := decodeTool[mcpFindFilesResponse](t, callTool(t, h, map[string]any{"pattern": ".go", "limit": 1}, false))
	if len(limited.Files) != 1 || !limited.Truncated {
		t.Errorf("limit 1: %v truncated=%v", limited.Files, limited.Truncated)
	}
	callTool(t, h, map[string]any{"pattern": "[bad"}, true)
	callTool(t, h, map[string]any{}, true)
}

func TestCodeStats(t *testing.T) {
	root := writeToolFixture(t)
	h := mcpCodeStatsHandler(toolConfig(root))

	dir := decodeTool[mcpCodeStatsResponse](t, callTool(t, h, map[string]any{"path": "acme"}, false))
	langs := map[string]mcpLanguageStats{}
	for _, l := range dir.Languages {
		langs[l.Language] = l
	}
	if langs["Go"].Files != 2 || langs["Python"].Files != 1 || dir.Files != 3 {
		t.Errorf("languages %+v, files %d; want Go x2, Python x1", dir.Languages, dir.Files)
	}
	if dir.Code == 0 || len(dir.MostComplex) == 0 {
		t.Errorf("want code lines and complex files: %+v", dir)
	}

	single := decodeTool[mcpCodeStatsResponse](t, callTool(t, h, map[string]any{"path": "acme/gadgets/lib.py"}, false))
	if single.Files != 1 || len(single.Languages) != 1 || single.Languages[0].Language != "Python" {
		t.Errorf("single file stats %+v", single)
	}
}

func TestRelatedFiles(t *testing.T) {
	root := writeToolFixture(t)
	h := mcpRelatedFilesHandler(toolConfig(root), NewSearchCache())

	resp := decodeTool[mcpRelatedResponse](t, callTool(t, h, map[string]any{"path": "acme/widgets/main.go", "scope": "."}, false))
	if len(resp.Terms) < 2 {
		t.Fatalf("terms %v", resp.Terms)
	}
	if !containsFold(resp.Terms, "WidgetRegistry") {
		t.Errorf("WidgetRegistry not among the distinctive terms %v", resp.Terms)
	}
	if len(resp.Results) == 0 || resp.Results[0].Path != "acme/widgets/main_test.go" {
		t.Fatalf("results %+v, want main_test.go first", resp.Results)
	}
	for _, r := range resp.Results {
		if strings.HasSuffix(r.Path, "main.go") && !strings.HasSuffix(r.Path, "_test.go") {
			t.Errorf("the file itself is in its own results: %+v", resp.Results)
		}
		if len(r.SharedTerms) < 2 {
			t.Errorf("%s shares fewer than two terms: %v", r.Path, r.SharedTerms)
		}
	}
}

func TestDistinctiveTermsPrefersCompoundIdentifiers(t *testing.T) {
	terms := distinctiveTerms([]byte(fixtureMainGo), "Go", 3)
	if len(terms) != 3 {
		t.Fatalf("terms %v", terms)
	}
	for _, term := range terms {
		if !isCompoundIdentifier(term) {
			t.Errorf("plain word %q outranked the compound identifiers: %v", term, terms)
		}
		if strings.EqualFold(term, "func") || strings.EqualFold(term, "return") {
			t.Errorf("keyword %q chosen: %v", term, terms)
		}
	}
}

// gitFixture makes a repository with main.go at tag v1.0.0, then changes it in
// a second commit, so the working tree and the tag differ.
func gitFixture(t *testing.T) (root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("package main\n\nfunc oldName() {}\n")
	run("add", "main.go")
	run("commit", "-q", "-m", "first")
	run("tag", "v1.0.0")
	write("package main\n\nfunc newName() {}\n")
	run("commit", "-q", "-am", "second")
	return root
}

func TestGetFileAtRev(t *testing.T) {
	root := gitFixture(t)
	cfg := toolConfig(root)
	h := mcpGetFileHandler(cfg)

	old := decodeTool[mcpFileResult](t, callTool(t, h, map[string]any{"path": "main.go", "rev": "v1.0.0"}, false))
	if !strings.Contains(old.Content, "oldName") || old.Rev != "v1.0.0" {
		t.Errorf("at v1.0.0: %+v", old)
	}
	cur := decodeTool[mcpFileResult](t, callTool(t, h, map[string]any{"path": "main.go"}, false))
	if !strings.Contains(cur.Content, "newName") || cur.Rev != "" {
		t.Errorf("working tree: %+v", cur)
	}

	for _, bad := range []string{"--output=/tmp/x", "-p", "v1.0.0:other", "no such rev", "doesnotexist"} {
		callTool(t, h, map[string]any{"path": "main.go", "rev": bad}, true)
	}

	outline := decodeTool[mcpOutlineResponse](t, callTool(t, mcpOutlineHandler(cfg), map[string]any{"path": "main.go", "rev": "v1.0.0"}, false))
	if len(outline.Declarations) != 1 || !strings.Contains(outline.Declarations[0].Text, "oldName") {
		t.Errorf("outline at v1.0.0: %+v", outline)
	}
}

func TestListRefs(t *testing.T) {
	root := gitFixture(t)
	h := mcpListRefsHandler(toolConfig(root))

	resp := decodeTool[mcpListRefsResponse](t, callTool(t, h, map[string]any{}, false))
	if resp.Head != "main" || resp.HeadCommit == "" {
		t.Errorf("head %q %q", resp.Head, resp.HeadCommit)
	}
	kinds := map[string]string{}
	for _, r := range resp.Refs {
		kinds[r.Name] = r.Kind
		if r.Commit == "" || r.Date == "" {
			t.Errorf("ref without commit or date: %+v", r)
		}
	}
	if kinds["v1.0.0"] != "tag" || kinds["main"] != "branch" {
		t.Errorf("refs %+v", resp.Refs)
	}

	tags := decodeTool[mcpListRefsResponse](t, callTool(t, h, map[string]any{"kind": "tags"}, false))
	if len(tags.Refs) != 1 || tags.Refs[0].Name != "v1.0.0" {
		t.Errorf("tags only: %+v", tags.Refs)
	}
	callTool(t, h, map[string]any{"kind": "everything"}, true)

	notRepo := t.TempDir()
	callTool(t, mcpListRefsHandler(toolConfig(notRepo)), map[string]any{}, true)
}

func writeCatalogue(t *testing.T, dir, repos string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, catalogueReposFile), []byte(repos), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListRepos(t *testing.T) {
	root := writeToolFixture(t)
	catDir := t.TempDir()
	writeCatalogue(t, catDir, `"host","org","repo","default_branch","tags"
"github.com","acme","widgets","main","go,mcp"
"github.com","acme","gadgets","main","python"
"codeberg.org","other","thing","main","go"
"github.com","acme","retired","master","go"
`)
	meta := "host,org,repo,description,primary_language,languages,topics,license,homepage,is_archived,created_at,canonical,is_fork,fork_parent\n" +
		"github.com,acme,widgets,A registry of widgets,Go,Go,\"registry,widgets\",MIT,,false,2020-01-01,,false,\n" +
		"github.com,acme,gadgets,Spinning gadgets,Python,Python,,MIT,,false,2020-01-01,,false,\n" +
		"github.com,acme,retired,Old widget code,Go,Go,,MIT,,true,2015-01-01,,true,upstream/retired\n"
	if err := os.WriteFile(filepath.Join(catDir, catalogueMetaFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := toolConfig(root)
	cfg.MCPCatalogueDir = catDir
	h := mcpListReposHandler(cfg, newRepoCatalogue(catDir))

	byQuery := decodeTool[mcpListReposResponse](t, callTool(t, h, map[string]any{"query": "widget"}, false))
	if byQuery.Total != 2 || byQuery.Repos[0].Path != "acme/widgets" || byQuery.Repos[1].Path != "acme/retired" {
		t.Fatalf("query widget: %+v", byQuery.Repos)
	}
	w := byQuery.Repos[0]
	if !w.OnDisk || w.Language != "Go" || w.Description != "A registry of widgets" || len(w.Topics) != 2 {
		t.Errorf("acme/widgets entry %+v", w)
	}
	if r := byQuery.Repos[1]; r.OnDisk || !r.Archived || !r.Fork {
		t.Errorf("acme/retired entry %+v", r)
	}

	active := decodeTool[mcpListReposResponse](t, callTool(t, h, map[string]any{"query": "widget", "include_archived": false}, false))
	if active.Total != 1 {
		t.Errorf("include_archived=false: %+v", active.Repos)
	}
	byTag := decodeTool[mcpListReposResponse](t, callTool(t, h, map[string]any{"tag": "MCP"}, false))
	if byTag.Total != 1 || byTag.Repos[0].Path != "acme/widgets" {
		t.Errorf("tag mcp: %+v", byTag.Repos)
	}
	byLang := decodeTool[mcpListReposResponse](t, callTool(t, h, map[string]any{"org": "ACME", "language": "python"}, false))
	if byLang.Total != 1 || byLang.Repos[0].Path != "acme/gadgets" {
		t.Errorf("org+language: %+v", byLang.Repos)
	}
	limited := decodeTool[mcpListReposResponse](t, callTool(t, h, map[string]any{"tag": "go", "limit": 1}, false))
	if limited.Total != 3 || limited.Returned != 1 || !limited.Truncated {
		t.Errorf("limit 1: %+v", limited)
	}
	callTool(t, h, map[string]any{}, true)

	// A refreshed catalogue is picked up without a restart.
	writeCatalogue(t, catDir, "\"host\",\"org\",\"repo\",\"default_branch\",\"tags\"\n\"github.com\",\"acme\",\"sprockets\",\"main\",\"go\"\n")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(catDir, catalogueReposFile), later, later); err != nil {
		t.Fatal(err)
	}
	reloaded := decodeTool[mcpListReposResponse](t, callTool(t, h, map[string]any{"org": "acme"}, false))
	if reloaded.Total != 1 || reloaded.Repos[0].Path != "acme/sprockets" {
		t.Errorf("after reload: %+v", reloaded.Repos)
	}
}

func TestValidateMCPFlagsCatalogue(t *testing.T) {
	catDir := t.TempDir()
	writeCatalogue(t, catDir, "\"host\",\"org\",\"repo\",\"default_branch\",\"tags\"\n")

	cfg := DefaultConfig()
	cfg.MCPCatalogueDir = catDir
	if err := validateMCPFlags(&cfg); err == nil || !strings.Contains(err.Error(), "requires --mcp") {
		t.Errorf("catalogue without an MCP mode: %v", err)
	}
	cfg.MCPServer = true
	if err := validateMCPFlags(&cfg); err != nil {
		t.Errorf("catalogue with --mcp: %v", err)
	}
	cfg.MCPCatalogueDir = t.TempDir()
	if err := validateMCPFlags(&cfg); err == nil {
		t.Error("catalogue dir without repos.csv accepted")
	}
}

// TestNewMCPServerTools checks every tool is registered, and list_repos only
// when a catalogue is configured.
func TestNewMCPServerTools(t *testing.T) {
	list := func(cfg *Config) map[string]mcp.Tool {
		t.Helper()
		c, err := client.NewInProcessClient(newMCPServer(cfg))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		ctx := context.Background()
		if err := c.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
			t.Fatal(err)
		}
		res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		tools := map[string]mcp.Tool{}
		for _, tool := range res.Tools {
			tools[tool.Name] = tool
		}
		return tools
	}

	cfg := toolConfig(t.TempDir())
	tools := list(cfg)
	for _, name := range []string{"search", "get_file", "search_facets", "file_outline", "list_dir", "find_files", "code_stats", "list_refs", "related_files"} {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("tool %s not registered", name)
			continue
		}
		a := tool.Annotations
		if a.ReadOnlyHint == nil || !*a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s: want readOnlyHint true, destructiveHint false", name)
		}
	}
	if _, ok := tools["list_repos"]; ok {
		t.Error("list_repos registered without --mcp-catalogue")
	}

	catDir := t.TempDir()
	writeCatalogue(t, catDir, "\"host\",\"org\",\"repo\",\"default_branch\",\"tags\"\n")
	cfg.MCPCatalogueDir = catDir
	if _, ok := list(cfg)["list_repos"]; !ok {
		t.Error("list_repos missing with --mcp-catalogue")
	}
}
