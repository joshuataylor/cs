// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/boyter/cs/v3/pkg/common"
	"github.com/boyter/cs/v3/pkg/ranker"
	"github.com/boyter/gocodewalker"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// related_files finds files that share a file's distinctive identifiers: its
// tests, callers, and sibling implementations. There is no index to give real
// document frequencies, so "distinctive" is approximated from the file alone:
// identifiers that recur, are long, and are compound (camelCase, snake_case)
// rank above plain words, and the language's keywords are dropped. The terms
// are ORed into one search ranked by BM25, which then supplies the rarity
// weighting across the searched tree.

var mcpRelatedParams = []string{"path", "scope", "limit"}

// mcpRelatedTerms is how many identifiers go into the query, within the MCP
// query limits (12 terms, 250 characters).
const mcpRelatedTerms = 8

var mcpIdentifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

type mcpRelatedFile struct {
	Path        string   `json:"path"`
	Score       float64  `json:"score"`
	SharedTerms []string `json:"shared_terms"`
}

type mcpRelatedResponse struct {
	File              string           `json:"file"`
	SearchedDirectory string           `json:"searched_directory"`
	Terms             []string         `json:"terms"`
	Results           []mcpRelatedFile `json:"results"`
	Message           string           `json:"message,omitempty"`
}

func newMCPRelatedFilesTool() mcp.Tool {
	return mcp.NewTool("related_files",
		mcp.WithDescription("Find files related to a given file: ones that share its distinctive identifiers, such as its tests, its callers and sibling implementations. "+
			"Picks the file's most distinctive identifiers, searches its repository for files sharing at least two of them, and ranks them with BM25. "+
			"Heuristic: good for 'what else touches this', not a precise call graph. The terms used are returned so you can refine with search."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("path", mcp.Required(), mcp.Description("The file to find relatives of, as for get_file.")),
		mcp.WithString("scope", mcp.Description("Directory to search. Defaults to the file's git repository root.")),
		mcp.WithNumber("limit", mcp.Description("Maximum related files, 1-50. Default 10.")),
	)
}

// distinctiveTerms returns up to n identifiers from content worth searching
// for, most distinctive first, deduplicated case-insensitively (search is
// case-insensitive by default).
func distinctiveTerms(content []byte, language string, n int) []string {
	counts := map[string]int{}
	for _, m := range mcpIdentifierPattern.FindAll(content, -1) {
		if len(m) < 4 {
			continue
		}
		counts[string(m)]++
	}

	type scored struct {
		term  string
		score float64
	}
	var candidates []scored
	for term, count := range counts {
		if ranker.IsStopword(language, term) || isAllDigitsOrUnderscore(term) {
			continue
		}
		score := math.Log2(1+float64(count)) * math.Min(float64(len(term)), 24) / 6
		if isCompoundIdentifier(term) {
			score *= 3
		}
		candidates = append(candidates, scored{term, score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].term < candidates[j].term
	})

	seen := map[string]bool{}
	var terms []string
	queryLen := 0
	for _, c := range candidates {
		key := strings.ToLower(c.term)
		if seen[key] {
			continue
		}
		// Keep the ORed query inside the MCP character limit.
		if queryLen+len(c.term)+4 > common.MaxQueryCharsMCP {
			continue
		}
		seen[key] = true
		terms = append(terms, c.term)
		queryLen += len(c.term) + 4
		if len(terms) == n {
			break
		}
	}
	return terms
}

// isCompoundIdentifier reports camelCase, PascalCase or snake_case names,
// which are far more often domain names than plain words are.
func isCompoundIdentifier(s string) bool {
	trimmed := strings.Trim(s, "_")
	if strings.Contains(trimmed, "_") {
		return true
	}
	for i, r := range trimmed {
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func isAllDigitsOrUnderscore(s string) bool {
	for _, r := range s {
		if r != '_' && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func mcpRelatedFilesHandler(cfg *Config, cache *SearchCache) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("related_files", args, mcpRelatedParams); res != nil {
			return res, nil
		}
		abs, err := resolveMCPFilePath(cfg, mcpStringArg(args, "path"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		content, err := mcpReadFile(ctx, cfg, abs, "")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if mcpIsBinary(content) {
			return mcp.NewToolResultError("file appears to be binary"), nil
		}
		limit := mcpIntArg(args, "limit", 10, 1, 50)

		scope := mcpStringArg(args, "scope")
		if scope == "" {
			scope = gocodewalker.FindRepositoryRoot(filepath.Dir(abs))
		}
		root, _, err := resolveSearchRoot(cfg, scope)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		lang := detectLanguage(filepath.Base(abs), content)
		terms := distinctiveTerms(content, lang, mcpRelatedTerms)
		resp := mcpRelatedResponse{File: abs, SearchedDirectory: root, Terms: terms, Results: []mcpRelatedFile{}}
		if len(terms) < 2 {
			resp.Message = "Not enough distinctive identifiers in this file to look for related files."
			return mcpJSONResult(resp)
		}

		searchCfg := *cfg
		searchCfg.Directory = root
		searchCfg.FindRoot = false
		searchCfg.Format = "json"
		searchCfg.MaxQueryChars = common.MaxQueryCharsMCP
		searchCfg.MaxQueryTerms = common.MaxQueryTermsMCP
		query := strings.Join(terms, " OR ")
		release, slotErr := acquireSearchSlot(ctx, cfg)
		if slotErr != nil {
			return mcp.NewToolResultError(slotErr.Error()), nil
		}
		defer release()
		searchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		ch, stats, searchErr := DoSearch(searchCtx, &searchCfg, query, cache)
		if searchErr != nil {
			return mcp.NewToolResultError(searchErr.Error()), nil
		}
		collected, partial := collectResults(&searchCfg, ch, cancel)

		// One shared identifier is usually a coincidence; ask for two.
		var results []*common.FileJob
		for _, fj := range collected {
			if fj.Location == abs || len(fj.MatchLocations) < 2 {
				continue
			}
			results = append(results, fj)
		}
		results = ranker.RankResults(searchCfg.Ranker, int(stats.TextFileCount.Load()), results,
			searchCfg.StructuralRankerConfig(), searchCfg.ResolveRankingProfile(), false)
		if len(results) > limit {
			results = results[:limit]
		}
		for _, fj := range results {
			shared := make([]string, 0, len(fj.MatchLocations))
			for term := range fj.MatchLocations {
				shared = append(shared, term)
			}
			sort.Strings(shared)
			resp.Results = append(resp.Results, mcpRelatedFile{
				Path:        mcpRelPath(root, fj.Location),
				Score:       math.Round(fj.Score*1000) / 1000,
				SharedTerms: shared,
			})
		}
		if len(resp.Results) == 0 {
			resp.Message = fmt.Sprintf("No other file in %s shares two or more of these identifiers.", root)
		}
		if partial {
			resp.Message = strings.TrimSpace(budgetMessage(&searchCfg, len(collected)) + " " + resp.Message)
		}
		return mcpJSONResult(resp)
	}
}
