// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

const Version = "3.2.0"

// versionSuffix is appended to Version wherever it is reported (--version,
// the help header, MCP server info), so a build from a fork or branch can say
// what it is, e.g. go build -ldflags "-X main.versionSuffix=-josh.<commit>".
var versionSuffix = ""

// fullVersion is Version plus any suffix set at link time.
func fullVersion() string {
	return Version + versionSuffix
}

func main() {
	cfg := DefaultConfig()
	var cpuProfile string

	initLanguageDatabase()

	rootCmd := &cobra.Command{
		Use: "cs",
		Long: "code spelunker (cs) code search.\n" +
			"Version " + fullVersion() + "\n" +
			"Ben Boyter <ben@boyter.org>" +
			"\n\n" +
			"cs recursively searches the current directory using some boolean logic\n" +
			"optionally combined with regular expressions.\n" +
			"\n" +
			"Works via command line where passed in arguments are the search terms\n" +
			"or in a TUI mode with no arguments. Can also run in HTTP mode with\n" +
			"the -d or --http-server flag.\n" +
			"\n" +
			"Searches by default use AND boolean syntax for all terms\n" +
			" - use --default-operator=or to combine terms with OR\n" +
			" - exact match using quotes \"find this\"\n" +
			" - fuzzy match within 1 or 2 distance fuzzy~1 fuzzy~2\n" +
			" - negate using NOT such as pride NOT prejudice\n" +
			" - OR syntax such as catch OR throw\n" +
			" - group with parentheses (cat OR dog) NOT fish\n" +
			" - note: NOT binds to next term, use () with OR\n" +
			" - regex with toothpick syntax /pr[e-i]de/\n" +
			"\n" +
			"Searches can filter which files are searched by adding\n" +
			"the following syntax\n" +
			" - file:test              (substring match on filename)\n" +
			" - filename:.go           (substring match on filename)\n" +
			" - path:pkg/search        (substring match on full file path)\n" +
			"\n" +
			"Example search that uses all current functionality\n" +
			" - darcy NOT collins wickham~1 \"ten thousand a year\" /pr[e-i]de/ file:test path:pkg\n" +
			"\n" +
			"The default input field in tui mode supports some nano commands\n" +
			"- CTRL+a move to the beginning of the input\n" +
			"- CTRL+e move to the end of the input\n" +
			"- CTRL+k to clear from the cursor location forward\n" +
			"\n" +
			"- F1 cycle ranker (simple/tfidf/bm25/structural)\n" +
			"- F2 cycle code filter (default/only-code/only-comments/only-strings/only-declarations/only-usages)\n" +
			"- F3 cycle gravity (off/low/default/logic/brain)\n" +
			"- F4 cycle noise (silence/quiet/default/loud/raw)\n",
		Version: fullVersion(),
		Run: func(cmd *cobra.Command, args []string) {
			if cpuProfile != "" {
				f, err := os.Create(cpuProfile)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: could not create CPU profile: %v\n", err)
					os.Exit(1)
				}
				pprof.StartCPUProfile(f)
				defer func() {
					pprof.StopCPUProfile()
					f.Close()
				}()
			}

			cfg.SearchString = args

			// Validate --default-operator
			if _, err := cfg.ResolveDefaultOperator(); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}

			// Validate --profile
			switch cfg.Profile {
			case "", "balanced", "precise", "broad":
				// ok
			default:
				fmt.Fprintf(os.Stderr, "error: unknown --profile %q (valid: balanced, precise, broad)\n", cfg.Profile)
				os.Exit(1)
			}

			// Mutual exclusivity check
			count := 0
			if cfg.OnlyCode {
				count++
			}
			if cfg.OnlyComments {
				count++
			}
			if cfg.OnlyStrings {
				count++
			}
			if cfg.OnlyDeclarations {
				count++
			}
			if cfg.OnlyUsages {
				count++
			}
			if count > 1 {
				fmt.Fprintf(os.Stderr, "error: --only-code, --only-comments, --only-strings, --only-declarations, and --only-usages are mutually exclusive\n")
				os.Exit(1)
			}

			// Auto-select structural ranker when a content filter is set
			if cfg.HasContentFilter() && cfg.Ranker != "structural" {
				fmt.Fprintf(os.Stderr, "warning: --only-code/--only-comments/--only-strings requires structural ranker, setting --ranker=structural\n")
				cfg.Ranker = "structural"
			}

			// Validate git-sync flags before entering any mode
			if cfg.GitSync {
				if cfg.GitSyncInterval <= 0 {
					fmt.Fprintf(os.Stderr, "error: --git-sync-interval must be a positive duration\n")
					os.Exit(1)
				}
				if cfg.GitSyncWorkers < 1 {
					fmt.Fprintf(os.Stderr, "error: --git-sync-workers must be at least 1\n")
					os.Exit(1)
				}
			}

			if err := validateMCPFlags(&cfg); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}

			if cfg.MCPLockDir && !cfg.MCPServer && cfg.MCPHTTPAddress == "" {
				fmt.Fprintf(os.Stderr, "warning: --mcp-lock-dir has no effect without --mcp or --mcp-http\n")
			}

			if cfg.MCPHTTPAddress != "" {
				if cfg.GitSync {
					stopSync := startGitSync(&cfg)
					defer stopSync()
				}
				StartMCPHTTPServer(&cfg)
			} else if cfg.MCPServer {
				if cfg.GitSync {
					stopSync := startGitSync(&cfg)
					defer stopSync()
				}
				StartMCPServer(&cfg)
			} else if cfg.HttpServer {
				if cfg.GitSync {
					stopSync := startGitSync(&cfg)
					defer stopSync()
				}
				StartHttpServer(&cfg)
			} else if len(cfg.SearchString) != 0 {
				if cfg.GitSync {
					fmt.Fprintf(os.Stderr, "warning: --git-sync is ignored in console mode (not a long-running process)\n")
				}
				ConsoleSearch(&cfg)
			} else {
				if cfg.GitSync {
					stopSync := startGitSync(&cfg)
					defer stopSync()
				}
				// TUI default: show 5 snippets per result (vs. console's 1)
				// so the Left/Right cycling feature is discoverable out of the box.
				if !cmd.Flags().Changed("snippet-count") {
					cfg.SnippetCount = 5
				}
				p := tea.NewProgram(initialModel(&cfg), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithOutput(os.Stderr))
				m, err := p.Run()
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					os.Exit(1)
				}
				if fm, ok := m.(model); ok && fm.chosen != "" {
					fmt.Println(fm.chosen)
				}
			}
		},
	}

	flags := rootCmd.PersistentFlags()

	flags.BoolVar(
		&cfg.IncludeBinaryFiles,
		"binary",
		false,
		"set to disable binary file detection and search binary files",
	)
	flags.BoolVar(
		&cfg.IgnoreIgnoreFile,
		"no-ignore",
		false,
		"disables .ignore file logic",
	)
	flags.BoolVar(
		&cfg.IgnoreGitIgnore,
		"no-gitignore",
		false,
		"disables .gitignore file logic",
	)
	flags.IntVarP(
		&cfg.SnippetLength,
		"snippet-length",
		"n",
		300,
		"size of the snippet to display",
	)
	flags.IntVarP(
		&cfg.SnippetCount,
		"snippet-count",
		"s",
		1,
		"number of snippets to display",
	)
	flags.BoolVar(
		&cfg.IncludeHidden,
		"hidden",
		false,
		"include hidden files",
	)
	flags.StringSliceVarP(
		&cfg.AllowListExtensions,
		"include-ext",
		"i",
		[]string{},
		"limit to file extensions (N.B. case sensitive) [comma separated list: e.g. go,java,js,C,cpp]",
	)
	flags.StringSliceVarP(
		&cfg.LanguageTypes,
		"type",
		"t",
		[]string{},
		"limit to language types [comma separated list: e.g. Go,Java,Python]",
	)
	flags.BoolVarP(
		&cfg.FindRoot,
		"find-root",
		"r",
		false,
		"attempts to find the root of this repository by traversing in reverse looking for .git or .hg",
	)
	flags.StringSliceVar(
		&cfg.PathDenylist,
		"exclude-dir",
		[]string{".git", ".hg", ".svn"},
		"directories to exclude",
	)
	flags.BoolVarP(
		&cfg.CaseSensitive,
		"case-sensitive",
		"c",
		false,
		"make the search case sensitive",
	)
	flags.StringSliceVarP(
		&cfg.LocationExcludePattern,
		"exclude-pattern",
		"x",
		[]string{},
		"file and directory locations matching case sensitive patterns will be ignored [comma separated list: e.g. vendor,_test.go]",
	)
	flags.BoolVar(
		&cfg.IncludeMinified,
		"min",
		false,
		"include minified files",
	)
	flags.IntVar(
		&cfg.MinifiedLineByteLength,
		"min-line-length",
		255,
		"number of bytes per average line for file to be considered minified",
	)
	flags.Int64Var(
		&cfg.MaxReadSizeBytes,
		"max-read-size-bytes",
		1_000_000,
		"number of bytes to read into a file with the remaining content ignored",
	)
	flags.StringVarP(
		&cfg.Format,
		"format",
		"f",
		"text",
		"set output format [text, json, vimgrep]",
	)
	flags.StringVar(
		&cfg.Ranker,
		"ranker",
		"structural",
		"set ranking algorithm [simple, tfidf, bm25, structural]",
	)
	flags.StringVar(
		&cfg.Profile,
		"profile",
		"",
		"ranking profile [balanced, precise, broad] — overrides --gravity, --noise, and --test-penalty when set",
	)
	flags.StringVar(
		&cfg.DefaultOperator,
		"default-operator",
		"and",
		"how to combine adjacent terms with no explicit AND/OR [and, or]. 'or' is useful for broad multi-keyword searches",
	)
	flags.StringVar(
		&cfg.GravityIntent,
		"gravity",
		"default",
		"complexity gravity intent: brain (2.5), logic (1.5), default (1.0), low (0.2), off (0.0)",
	)
	flags.StringVar(
		&cfg.NoiseIntent,
		"noise",
		"default",
		"noise penalty intent: silence (0.1), quiet (0.5), default (1.0), loud (2.0), raw (off)",
	)
	flags.Float64Var(
		&cfg.TestPenalty,
		"test-penalty",
		0.4,
		"score multiplier for test files when query has no test intent (0.0-1.0, 1.0=disabled)",
	)
	flags.StringVarP(
		&cfg.FileOutput,
		"output",
		"o",
		"",
		"output filename (default stdout)",
	)
	flags.StringVar(
		&cfg.Directory,
		"dir",
		"",
		"directory to search, if not set defaults to current working directory",
	)
	flags.StringVar(
		&cfg.SnippetMode,
		"snippet-mode",
		"auto",
		"snippet extraction mode: auto, snippet, lines, or grep",
	)
	flags.IntVar(
		&cfg.ResultLimit,
		"result-limit",
		-1,
		"maximum number of results to return (-1 for unlimited)",
	)
	flags.IntVar(
		&cfg.LineLimit,
		"line-limit",
		-1,
		"max matching lines per file in grep mode (-1 = unlimited)",
	)
	flags.IntVarP(
		&cfg.ContextBefore,
		"before-context",
		"B",
		0,
		"lines of context before each match (grep mode)",
	)
	flags.IntVarP(
		&cfg.ContextAfter,
		"after-context",
		"A",
		0,
		"lines of context after each match (grep mode)",
	)
	flags.IntVarP(
		&cfg.ContextAround,
		"context",
		"C",
		0,
		"lines of context before and after each match (grep mode)",
	)
	flags.BoolVar(
		&cfg.Dedup,
		"dedup",
		false,
		"collapse byte-identical search matches, keeping the highest-scored representative",
	)
	flags.BoolVar(
		&cfg.MCPServer,
		"mcp",
		false,
		"start as an MCP (Model Context Protocol) server over stdio",
	)
	flags.BoolVar(
		&cfg.MCPLockDir,
		"mcp-lock-dir",
		false,
		"restrict the MCP server to --dir: reject searching or reading outside that tree",
	)
	flags.StringVar(
		&cfg.MCPHTTPAddress,
		"mcp-http",
		"",
		"start as an MCP server over Streamable HTTP on this address (e.g. 127.0.0.1:24134), serving /mcp; a non-loopback address requires --mcp-lock-dir or --mcp-http-token-file",
	)
	flags.StringVar(
		&cfg.MCPHTTPTokenFile,
		"mcp-http-token-file",
		"",
		"file holding a bearer token that --mcp-http clients must send as 'Authorization: Bearer <token>'",
	)
	flags.StringVar(
		&cfg.CatalogueDir,
		"catalogue",
		"",
		"directory with a repository catalogue (repos.csv, optional repos-meta.csv) for a --dir laid out as <org>/<repo>; enables the list_repos MCP tool and the 'tag' search parameter (web and MCP)",
	)
	flags.StringVar(
		&cfg.MCPInstructionsFile,
		"mcp-instructions-file",
		"",
		"text file sent to MCP clients as the server's instructions, e.g. how the tree under --dir is laid out and how best to search it",
	)
	// --mcp-catalogue was the flag's name before the web server could use it.
	flags.StringVar(&cfg.CatalogueDir, "mcp-catalogue", "", "deprecated: use --catalogue")
	_ = flags.MarkHidden("mcp-catalogue")
	flags.IntVar(
		&cfg.MaxResultFiles,
		"max-result-files",
		cfg.MaxResultFiles,
		"HTTP/MCP servers: stop collecting a search after this many matching files and mark the results partial (0 = no limit)",
	)
	flags.IntVar(
		&cfg.MaxResultMB,
		"max-result-mb",
		cfg.MaxResultMB,
		"HTTP/MCP servers: stop collecting a search once its matching files hold this many megabytes and mark the results partial (0 = no limit)",
	)
	flags.IntVar(
		&cfg.MaxConcurrentSearches,
		"max-concurrent-searches",
		cfg.MaxConcurrentSearches,
		"HTTP/MCP servers: run at most this many searches at once, queueing the rest (0 = no limit)",
	)
	flags.BoolVarP(
		&cfg.HttpServer,
		"http-server",
		"d",
		false,
		"start the HTTP server",
	)
	flags.StringVar(
		&cfg.Address,
		"address",
		":8080",
		"address and port to listen on",
	)
	flags.StringVar(
		&cfg.SearchTemplate,
		"template-search",
		"",
		"path to a custom search template",
	)
	flags.StringVar(
		&cfg.DisplayTemplate,
		"template-display",
		"",
		"path to a custom display template",
	)
	flags.StringVar(
		&cfg.TemplateStyle,
		"template-style",
		"dark",
		"built-in theme for the HTTP server UI [dark, light, bare]",
	)
	flags.BoolVar(
		&cfg.NoSyntax,
		"no-syntax",
		false,
		"disable syntax highlighting in output",
	)
	flags.StringVar(
		&cfg.Color,
		"color",
		"auto",
		"color output mode [auto, always, never]",
	)
	flags.BoolVar(
		&cfg.Reverse,
		"reverse",
		false,
		"reverse the result order",
	)
	flags.StringVar(
		&cpuProfile,
		"cpu-profile",
		"",
		"write CPU profile to file (for use with go tool pprof or PGO)",
	)
	flags.Float64Var(
		&cfg.WeightCode,
		"weight-code",
		1.0,
		"structural ranker: weight for matches in code (default 1.0)",
	)
	flags.Float64Var(
		&cfg.WeightComment,
		"weight-comment",
		0.2,
		"structural ranker: weight for matches in comments (default 0.2)",
	)
	flags.Float64Var(
		&cfg.WeightString,
		"weight-string",
		0.5,
		"structural ranker: weight for matches in strings (default 0.5)",
	)
	flags.BoolVar(
		&cfg.OnlyCode,
		"only-code",
		false,
		"only rank matches in code (auto-selects structural ranker)",
	)
	flags.BoolVar(
		&cfg.OnlyComments,
		"only-comments",
		false,
		"only rank matches in comments (auto-selects structural ranker)",
	)
	flags.BoolVar(
		&cfg.OnlyStrings,
		"only-strings",
		false,
		"only rank matches in string literals (auto-selects structural ranker)",
	)
	flags.BoolVar(
		&cfg.OnlyDeclarations,
		"only-declarations",
		false,
		"only show matches on declaration lines (func, type, var, const, class, def, etc.)",
	)
	flags.BoolVar(
		&cfg.OnlyUsages,
		"only-usages",
		false,
		"only show matches on usage lines (excludes declarations)",
	)
	flags.BoolVar(
		&cfg.GitSync,
		"git-sync",
		false,
		"periodically git pull repositories found in the search directory (TUI/HTTP/MCP only)",
	)
	flags.DurationVar(
		&cfg.GitSyncInterval,
		"git-sync-interval",
		5*time.Minute,
		"interval between git sync pulls (e.g. 5m, 30s, 1h)",
	)
	flags.IntVar(
		&cfg.GitSyncWorkers,
		"git-sync-workers",
		1,
		"number of concurrent git pull workers",
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
