// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boyter/cs/v3/pkg/common"
)

func jobsChannel(sizes ...int) <-chan *common.FileJob {
	ch := make(chan *common.FileJob, len(sizes))
	for i, n := range sizes {
		ch <- &common.FileJob{Filename: fmt.Sprint(i), Content: make([]byte, n), ContentByteType: make([]byte, n)}
	}
	close(ch)
	return ch
}

func TestCollectResults(t *testing.T) {
	cases := []struct {
		name        string
		files, mb   int
		sizes       []int
		wantKept    int
		wantPartial bool
	}{
		{"no limits", 0, 0, []int{10, 10, 10}, 3, false},
		{"under both", 5, 1, []int{10, 10, 10}, 3, false},
		{"file limit", 2, 0, []int{10, 10, 10, 10}, 2, true},
		// Each result counts content plus its byte-type map: 2 x 300KB per file.
		{"byte limit", 0, 1, []int{300 << 10, 300 << 10, 300 << 10}, 2, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.MaxResultFiles, cfg.MaxResultMB = c.files, c.mb
			cancelled := false
			results, partial := collectResults(&cfg, jobsChannel(c.sizes...), func() { cancelled = true })
			if len(results) != c.wantKept || partial != c.wantPartial || cancelled != c.wantPartial {
				t.Errorf("kept %d partial %v cancelled %v; want %d %v %v", len(results), partial, cancelled, c.wantKept, c.wantPartial, c.wantPartial)
			}
		})
	}
}

func TestSearchGate(t *testing.T) {
	g := newSearchGate(1)
	release, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The second caller waits; with a short deadline it gives up.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(ctx); err == nil || !strings.Contains(err.Error(), "waiting for one of 1 search slots") {
		t.Fatalf("second acquire: %v", err)
	}

	// Once released, the slot is free again.
	release()
	release2, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()

	unlimited := newSearchGate(0)
	for range 10 {
		if _, err := unlimited.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func searchLocations(t *testing.T, cfg *Config, query string, cache *SearchCache) []string {
	t.Helper()
	ch, _, err := DoSearch(context.Background(), cfg, query, cache)
	if err != nil {
		t.Fatal(err)
	}
	var locs []string
	for fj := range ch {
		locs = append(locs, mcpRelPath(cfg.Directory, fj.Location))
	}
	sort.Strings(locs)
	return locs
}

func TestDoSearchRoots(t *testing.T) {
	root := writeToolFixture(t)
	cfg := DefaultConfig()
	cfg.Directory = root
	cfg.SearchRoots = []string{filepath.Join(root, "acme", "widgets"), filepath.Join(root, "acme", "gadgets")}

	got := searchLocations(t, &cfg, "WidgetRegistry OR spin_gadget", NewSearchCache())
	want := []string{"acme/gadgets/lib.py", "acme/widgets/main.go", "acme/widgets/main_test.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roots search found %v, want %v (other/thing must be skipped)", got, want)
	}
}

// TestDoSearchSkipsCacheWhenCancelled checks a search stopped early (the
// result budget, or a client going away) does not leave its partial file list
// in the prefix cache, where later queries extending it would miss files.
func TestDoSearchSkipsCacheWhenCancelled(t *testing.T) {
	root := t.TempDir()
	for i := range 30 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.txt", i)), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.Directory = root
	cfg.MaxResultFiles = 1

	cache := NewSearchCache()
	ctx, cancel := context.WithCancel(context.Background())
	ch, _, err := DoSearch(ctx, &cfg, "needle", cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, partial := collectResults(&cfg, ch, cancel); !partial {
		t.Fatal("expected the one-file budget to cut the search short")
	}
	// DoSearch stores the cache just after closing its channel, so give a store
	// (which the fix should prevent) the time to land before checking.
	time.Sleep(100 * time.Millisecond)
	if _, ok := cache.FindPrefixFiles(root, cfg.AllowListExtensions, "needle"); ok {
		t.Error("partial search was stored in the prefix cache")
	}

	// A complete search is cached as before.
	full := DefaultConfig()
	full.Directory = root
	if n := len(searchLocations(t, &full, "needle", cache)); n != 30 {
		t.Fatalf("full search found %d files, want 30", n)
	}
	var files []string
	var ok bool
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if files, ok = cache.FindPrefixFiles(root, full.AllowListExtensions, "needle"); ok {
			break
		}
	}
	if !ok || len(files) != 30 {
		t.Errorf("complete search not cached: %d files, ok=%v", len(files), ok)
	}
}

func TestMCPSearchBudgetAndTag(t *testing.T) {
	root := writeToolFixture(t)
	catDir := t.TempDir()
	writeCatalogue(t, catDir, "\"host\",\"org\",\"repo\",\"default_branch\",\"tags\"\n"+
		"\"github.com\",\"acme\",\"widgets\",\"main\",\"go,mcp\"\n\"github.com\",\"other\",\"thing\",\"main\",\"go\"\n")
	cfg := toolConfig(root)
	cfg.CatalogueDir = catDir
	h := mcpSearchHandler(cfg, NewSearchCache(), newRepoCatalogue(catDir))

	all := decodeTool[mcpSearchResponse](t, callTool(t, h, map[string]any{"query": "WidgetRegistry"}, false))
	tagged := decodeTool[mcpSearchResponse](t, callTool(t, h, map[string]any{"query": "WidgetRegistry", "tag": "mcp"}, false))
	if all.TotalMatches != 3 || tagged.TotalMatches != 2 || tagged.TagRepositories != 1 || tagged.Tag != "mcp" {
		t.Errorf("all %d matches; tag mcp %d matches over %d repos (%q); want 3, 2, 1", all.TotalMatches, tagged.TotalMatches, tagged.TagRepositories, tagged.Tag)
	}
	callTool(t, h, map[string]any{"query": "WidgetRegistry", "tag": "nosuchtag"}, true)
	callTool(t, mcpSearchHandler(cfg, NewSearchCache(), nil), map[string]any{"query": "x", "tag": "mcp"}, true)

	capped := *cfg
	capped.MaxResultFiles = 1
	partial := decodeTool[mcpSearchResponse](t, callTool(t, mcpSearchHandler(&capped, NewSearchCache(), nil), map[string]any{"query": "WidgetRegistry"}, false))
	if !partial.Partial || partial.TotalMatches != 1 || !strings.Contains(partial.Message, "Search stopped after 1 matching files") {
		t.Errorf("budget: %+v", partial)
	}
}

// webSearch runs the web search handler for query string q (format=json).
func webSearch(t *testing.T, cfg *Config, catalogue *repoCatalogue, q string) httpSearch {
	t.Helper()
	tmpl, err := resolveSearchTemplate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	httpSearchHandler(cfg, NewSearchCache(), tmpl, catalogue)(rec, httptest.NewRequest(http.MethodGet, "/?format=json&"+q, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp httpSearch
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rec.Body.String())
	}
	return resp
}

func TestHTTPSearchScope(t *testing.T) {
	root := writeToolFixture(t)
	catDir := t.TempDir()
	writeCatalogue(t, catDir, "\"host\",\"org\",\"repo\",\"default_branch\",\"tags\"\n\"github.com\",\"acme\",\"widgets\",\"main\",\"mcp\"\n")
	cfg := DefaultConfig()
	cfg.Directory = root
	cfg.CatalogueDir = catDir
	catalogue := newRepoCatalogue(catDir)

	if got := webSearch(t, &cfg, catalogue, "q=WidgetRegistry").ResultsCount; got != 3 {
		t.Errorf("unscoped: %d results, want 3", got)
	}
	dir := webSearch(t, &cfg, catalogue, "q=WidgetRegistry&dir=other/thing")
	if dir.ResultsCount != 1 || dir.Dir != "other/thing" {
		t.Errorf("dir=other/thing: %d results, dir %q", dir.ResultsCount, dir.Dir)
	}
	tag := webSearch(t, &cfg, catalogue, "q=WidgetRegistry&tag=mcp")
	if tag.ResultsCount != 2 || tag.TagRepositories != 1 {
		t.Errorf("tag=mcp: %d results over %d repos", tag.ResultsCount, tag.TagRepositories)
	}
	for _, page := range tag.Pages {
		if page.Tag != "mcp" {
			t.Errorf("page link lost the tag: %+v", page)
		}
	}

	// The web server has no authentication: dir never leaves --dir.
	out := webSearch(t, &cfg, catalogue, "q=WidgetRegistry&dir=..")
	if out.ResultsCount != 0 || !strings.Contains(out.Message, "outside") {
		t.Errorf("dir=..: %d results, message %q", out.ResultsCount, out.Message)
	}
	if noCat := webSearch(t, &cfg, nil, "q=x&tag=mcp"); !strings.Contains(noCat.Message, "--catalogue") {
		t.Errorf("tag without a catalogue: message %q", noCat.Message)
	}

	capped := cfg
	capped.MaxResultFiles = 1
	partial := webSearch(t, &capped, catalogue, "q=WidgetRegistry")
	if !partial.Partial || partial.ResultsCount != 1 || !strings.Contains(partial.Message, "Search stopped after 1") {
		t.Errorf("budget: partial %v, %d results, message %q", partial.Partial, partial.ResultsCount, partial.Message)
	}
}

// TestHTTPSearchHTMLCarriesScope checks the rendered page keeps dir and tag
// in the form and shows the message.
func TestHTTPSearchHTMLCarriesScope(t *testing.T) {
	root := writeToolFixture(t)
	cfg := DefaultConfig()
	cfg.Directory = root
	for _, style := range []string{"dark", "light", "bare"} {
		cfg.TemplateStyle = style
		tmpl, err := resolveSearchTemplate(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		httpSearchHandler(&cfg, NewSearchCache(), tmpl, nil)(rec, httptest.NewRequest(http.MethodGet, "/?q=WidgetRegistry&dir=acme&tag=mcp", nil))
		body := rec.Body.String()
		for _, want := range []string{`name="dir" value="acme"`, `name="tag" value="mcp"`, "--catalogue"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s theme: page lacks %q", style, want)
			}
		}
	}
}

// fillSearchGate takes every slot of the process-wide search gate and returns
// a function that releases them, so a search started meanwhile waits in the
// gate until its context ends.
func fillSearchGate(t *testing.T) func() {
	t.Helper()
	cfg := DefaultConfig()
	var releases []func()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		release, err := acquireSearchSlot(ctx, &cfg)
		cancel()
		if err != nil {
			break
		}
		releases = append(releases, release)
	}
	if len(releases) == 0 {
		t.Fatal("could not take any search slot")
	}
	return func() {
		for _, r := range releases {
			r()
		}
	}
}

// TestMCPHTTPClientDisconnectCancelsSearch checks a client giving up on an
// MCP-over-HTTP search cancels it on the server: the handler, stuck waiting
// for a slot, returns and logs the call as cancelled.
func TestMCPHTTPClientDisconnectCancelsSearch(t *testing.T) {
	var buf safeBuffer
	url := startTestMCPHTTP(t, "", slog.New(slog.NewTextHandler(&buf, nil)))
	releaseAll := fillSearchGate(t)
	defer releaseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"greetSpelunker"},`+mcpMeta+`}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "search")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("request finished although every search slot was taken")
	}

	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(buf.String(), "outcome=cancelled") {
			return
		}
	}
	t.Fatalf("server did not log the search as cancelled after the client left:\n%s", buf.String())
}

// TestHTTPSearchClientDisconnectReturns checks the web handler gives up a
// search whose client has gone, rather than waiting (or searching) on.
func TestHTTPSearchClientDisconnectReturns(t *testing.T) {
	root := writeToolFixture(t)
	cfg := DefaultConfig()
	cfg.Directory = root
	tmpl, err := resolveSearchTemplate(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpSearchHandler(&cfg, NewSearchCache(), tmpl, nil)(w, r)
		close(done)
	}))
	defer srv.Close()
	releaseAll := fillSearchGate(t)
	defer releaseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/?q=WidgetRegistry&format=json", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("request finished although every search slot was taken")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("web handler still running 3s after its client disconnected")
	}
}

// safeBuffer is a bytes.Buffer safe for the server's logger and the test to
// use at once.
type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
