// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mcpGitTimeout bounds each git call a tool makes.
const mcpGitTimeout = 30 * time.Second

// mcpRevPattern is what a "rev" may contain: ref names, commit ids and the
// usual suffixes (v1.2.0, main, origin/main, abc123, HEAD~3, v2^{commit}).
// Leading "-" is rejected separately so a rev can never be read as an option.
var mcpRevPattern = regexp.MustCompile(`^[A-Za-z0-9._/@{}~^+-]{1,200}$`)

// validateGitRev rejects a rev that could be taken as a git option or that
// contains characters no ref or commit id uses.
func validateGitRev(rev string) error {
	if strings.HasPrefix(rev, "-") || !mcpRevPattern.MatchString(rev) {
		return fmt.Errorf("invalid rev %q: expected a tag, branch or commit id", rev)
	}
	return nil
}

// mcpGit runs git in dir and returns stdout, with stderr folded into the
// error. Output beyond maxBytes is cut off (maxBytes <= 0 means no limit).
func mcpGit(ctx context.Context, dir string, maxBytes int64, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot run git: %w", err)
	}
	var reader io.Reader = stdout
	if maxBytes > 0 {
		reader = io.LimitReader(stdout, maxBytes)
	}
	out, readErr := io.ReadAll(reader)
	_, _ = io.Copy(io.Discard, stdout) // let git finish writing past the limit
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out, readErr
}

// gitRepoRoot returns the top level of the git repository containing path,
// starting from the nearest directory that exists (a file read at an old
// revision may live in a directory deleted since).
func gitRepoRoot(ctx context.Context, path string) (string, error) {
	dir := path
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no existing directory above %s", path)
		}
		dir = parent
	}
	out, err := mcpGit(ctx, dir, 0, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not in a git repository", path)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitReadFileAt returns the contents of the file at abs as of rev, reading
// the blob directly (`git cat-file blob rev:path`), so no textconv, filter or
// pager runs.
func gitReadFileAt(ctx context.Context, abs, rev string, maxBytes int64) ([]byte, error) {
	if err := validateGitRev(rev); err != nil {
		return nil, err
	}
	root, err := gitRepoRoot(ctx, abs)
	if err != nil {
		return nil, err
	}
	// git reports the toplevel with symlinks resolved; resolve abs the same way
	// so the relative path is right on systems where /tmp or $HOME is a link.
	realAbs := abs
	if r, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		realAbs = filepath.Join(r, filepath.Base(abs))
	}
	rel, err := filepath.Rel(root, realAbs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("%s is not inside repository %s", abs, root)
	}
	out, err := mcpGit(ctx, root, maxBytes, "cat-file", "blob", rev+":"+filepath.ToSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s at %s: %v", filepath.ToSlash(rel), rev, err)
	}
	return out, nil
}

// mcpReadFile reads abs from disk, or from git as of rev when rev is set.
func mcpReadFile(ctx context.Context, cfg *Config, abs, rev string) ([]byte, error) {
	if rev != "" {
		return gitReadFileAt(ctx, abs, rev, cfg.MaxReadSizeBytes)
	}
	content, err := readFileContent(abs, cfg.MaxReadSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}
	return content, nil
}

// list_refs

var mcpListRefsParams = []string{"path", "kind", "limit"}

type mcpRef struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Commit string `json:"commit"`
	Date   string `json:"date"`
}

type mcpListRefsResponse struct {
	Repository string   `json:"repository"`
	Head       string   `json:"head"`
	HeadCommit string   `json:"head_commit"`
	Refs       []mcpRef `json:"refs"`
	Truncated  bool     `json:"truncated"`
}

func newMCPListRefsTool() mcp.Tool {
	return mcp.NewTool("list_refs",
		mcp.WithDescription("List a git repository's tags and branches, newest first, with commit ids and dates, plus the checked-out HEAD. "+
			"Use it to find the right 'rev' for get_file or file_outline, e.g. to read a file as of a released version instead of the current default branch."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("path", mcp.Description("Any directory inside the repository. Defaults to the server's default directory.")),
		mcp.WithString("kind", mcp.Description("'tags', 'branches' (local and remote-tracking) or 'all'. Default 'all'.")),
		mcp.WithNumber("limit", mcp.Description("Maximum refs, 1-500. Default 50.")),
	)
}

func mcpListRefsHandler(cfg *Config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		if res := mcpRejectUnknownArgs("list_refs", args, mcpListRefsParams); res != nil {
			return res, nil
		}
		dir, _, err := resolveSearchRoot(cfg, mcpStringArg(args, "path"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		limit := mcpIntArg(args, "limit", 50, 1, 500)

		var patterns []string
		switch kind := mcpStringArg(args, "kind"); kind {
		case "", "all":
			patterns = []string{"refs/tags", "refs/heads", "refs/remotes"}
		case "tags":
			patterns = []string{"refs/tags"}
		case "branches":
			patterns = []string{"refs/heads", "refs/remotes"}
		default:
			return mcp.NewToolResultError(fmt.Sprintf("invalid kind %q: use 'tags', 'branches' or 'all'", kind)), nil
		}

		root, err := gitRepoRoot(ctx, dir)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resp := mcpListRefsResponse{Repository: root, Refs: []mcpRef{}}
		if out, err := mcpGit(ctx, root, 0, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			resp.Head = strings.TrimSpace(string(out))
		}
		if out, err := mcpGit(ctx, root, 0, "rev-parse", "--short", "HEAD"); err == nil {
			resp.HeadCommit = strings.TrimSpace(string(out))
		}

		// Ask for one more than the limit to know whether there are more.
		forEachArgs := append([]string{"for-each-ref", "--sort=-creatordate", fmt.Sprintf("--count=%d", limit+1),
			"--format=%(refname)%09%(objectname:short)%09%(creatordate:short)"}, patterns...)
		out, err := mcpGit(ctx, root, 0, forEachArgs...)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("cannot list refs: %v", err)), nil
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) != 3 || strings.HasSuffix(fields[0], "/HEAD") {
				continue
			}
			if len(resp.Refs) == limit {
				resp.Truncated = true
				break
			}
			ref := mcpRef{Commit: fields[1], Date: fields[2]}
			switch {
			case strings.HasPrefix(fields[0], "refs/tags/"):
				ref.Kind, ref.Name = "tag", strings.TrimPrefix(fields[0], "refs/tags/")
			case strings.HasPrefix(fields[0], "refs/heads/"):
				ref.Kind, ref.Name = "branch", strings.TrimPrefix(fields[0], "refs/heads/")
			default:
				ref.Kind, ref.Name = "remote_branch", strings.TrimPrefix(fields[0], "refs/remotes/")
			}
			resp.Refs = append(resp.Refs, ref)
		}
		return mcpJSONResult(resp)
	}
}
