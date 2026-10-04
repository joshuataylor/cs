// SPDX-License-Identifier: MIT

package main

import (
	"strings"

	"github.com/boyter/scc/v4/processor"
)

// initLanguageDatabase initialises the scc language database.
// Must be called before detectLanguage or languageExtensions.
func initLanguageDatabase() {
	processor.ProcessConstants()
}

// detectLanguage returns the language name for the given filename and content
// using the scc language database.
func detectLanguage(filename string, content []byte) string {
	detected, _ := processor.DetectLanguage(filename)
	if len(detected) >= 2 {
		return processor.DetermineLanguage(filename, detected[0], detected, content)
	}
	if len(detected) == 1 {
		return detected[0]
	}
	return ""
}

// fileCodeStats detects the language and computes SCC code stats for a file
// in a single call. Returns empty language and zero stats for unrecognised files.
func fileCodeStats(filename string, content []byte) (language string, lines, code, comment, blank, complexity int64, contentByteType []byte) {
	language = detectLanguage(filename, content)
	if language == "" {
		return
	}
	sccJob := &processor.FileJob{
		Filename:        filename,
		Language:        language,
		Content:         content,
		Bytes:           int64(len(content)),
		ClassifyContent: true,
	}
	if !countStatsSafe(sccJob) {
		// scc panicked on this file: report it as unrecognised (no stats, no
		// content types) rather than crash the search worker and the process.
		return "", 0, 0, 0, 0, 0, nil
	}
	return language, sccJob.Lines, sccJob.Code, sccJob.Comment, sccJob.Blank, sccJob.Complexity, sccJob.ContentByteType
}

// sccCountStats is processor.CountStats, held in a variable so a test can
// stand in a counter that panics.
var sccCountStats = processor.CountStats

// countStatsSafe runs scc's CountStats and reports whether it finished
// without panicking. scc runs on every file a search reads, including
// whatever odd input a large tree holds, and a panic there kills the whole
// process: boyter/cs#61 was scc's blankState indexing past the end of
// ContentByteType for a file ending exactly on a docstring marker (fixed in
// scc), which took down a long-running HTTP server on the first request that
// touched such a file. This keeps a future case to one file losing its stats.
func countStatsSafe(sccJob *processor.FileJob) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	sccCountStats(sccJob)
	return true
}

// languageExtensions resolves language names to file extensions using the scc
// language database. Lookup is case-insensitive. It uses the ExtensionToLanguage
// map (extension → []languageName) built by ProcessConstants to invert the mapping.
func languageExtensions(languageNames []string) []string {
	// Build set of desired language names (lowercased) for fast lookup
	wanted := make(map[string]struct{}, len(languageNames))
	for _, name := range languageNames {
		wanted[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}

	var exts []string
	for ext, langs := range processor.ExtensionToLanguage {
		for _, lang := range langs {
			if _, ok := wanted[strings.ToLower(lang)]; ok {
				exts = append(exts, ext)
				break
			}
		}
	}
	return exts
}
