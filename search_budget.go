// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/boyter/cs/v3/pkg/common"
)

// The servers (web and MCP) collect every matching file from DoSearch before
// ranking, and each result holds the file's content plus a per-byte content
// type map, so a query matching a large share of a big tree (a common word
// over a corpus of repositories) can hold most of that tree in memory. The
// budget stops collection early, and the gate bounds how many searches run at
// once, so a server's worst case is roughly MaxConcurrentSearches x
// MaxResultMB.

// collectResults drains ch into a slice until cfg's result budget is spent,
// then calls cancel to stop the search and discards the rest. It reports
// whether the budget cut the results short. A zero limit is no limit.
func collectResults(cfg *Config, ch <-chan *common.FileJob, cancel context.CancelFunc) ([]*common.FileJob, bool) {
	maxBytes := int64(cfg.MaxResultMB) << 20
	var results []*common.FileJob
	var held int64
	exhausted := false
	for fj := range ch {
		if exhausted {
			continue // drain until the cancelled search closes the channel
		}
		results = append(results, fj)
		held += int64(len(fj.Content) + len(fj.ContentByteType))
		if (cfg.MaxResultFiles > 0 && len(results) >= cfg.MaxResultFiles) || (maxBytes > 0 && held >= maxBytes) {
			exhausted = true
			cancel()
		}
	}
	return results, exhausted
}

// budgetMessage explains a partial result set to the caller.
func budgetMessage(cfg *Config, collected int) string {
	return fmt.Sprintf("Search stopped after %d matching files (server limits: %d files, %d MB of matched content); "+
		"results and their ranking cover only those. Narrow the search with a directory, a tag or more specific terms.",
		collected, cfg.MaxResultFiles, cfg.MaxResultMB)
}

// searchGate admits a bounded number of searches at a time.
type searchGate struct {
	slots chan struct{} // nil: no limit
}

func newSearchGate(limit int) *searchGate {
	if limit <= 0 {
		return &searchGate{}
	}
	return &searchGate{slots: make(chan struct{}, limit)}
}

// acquire waits for a slot and returns its release function. It gives up with
// an error if ctx ends first, e.g. the client disconnected while queued.
func (g *searchGate) acquire(ctx context.Context) (func(), error) {
	if g.slots == nil {
		return func() {}, nil
	}
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("search cancelled while waiting for one of %d search slots: %w", cap(g.slots), ctx.Err())
	}
}

var (
	serverGateOnce sync.Once
	serverGate     *searchGate
)

// acquireSearchSlot takes a slot from the process-wide gate, sized from the
// first caller's cfg.MaxConcurrentSearches (a server runs with one config).
func acquireSearchSlot(ctx context.Context, cfg *Config) (func(), error) {
	serverGateOnce.Do(func() { serverGate = newSearchGate(cfg.MaxConcurrentSearches) })
	return serverGate.acquire(ctx)
}
