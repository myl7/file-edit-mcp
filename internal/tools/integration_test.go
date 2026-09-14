// Integration-layer tests (ARCHITECTURE §9) for the T7/T8 remainder: glob
// and grep driven end-to-end through the InMemory MCP client (mtime
// ordering, the 200-result cap, doublestar depth, path narrowing, the three
// grep modes, engine fallback, and rg-vs-fallback consistency), plus the two
// read-tool matrix rows that need a client round-trip: the per-line
// truncation marker and offset continuation across the line-limit boundary.
//
// Tests that shrink package vars (lineio.DefaultLines, rgBin) restore them
// via t.Cleanup and rely on the package's serial execution (no t.Parallel
// anywhere in this package; mutating shared vars under parallel tests would
// race — lineio.DefaultLines is even another package's exported var).

package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myl7/file-edit-mcp/internal/lineio"
)

// globCall invokes glob through the client and decodes the structured
// GlobOutput; extra args (e.g. path) merge over the pattern.
func globCall(t *testing.T, cs *mcp.ClientSession, pattern string, extra map[string]any) GlobOutput {
	t.Helper()
	args := map[string]any{"pattern": pattern}
	for k, v := range extra {
		args[k] = v
	}
	var out GlobOutput
	if err := json.Unmarshal([]byte(wantOK(t, cs, "glob", args)), &out); err != nil {
		t.Fatalf("glob result is not GlobOutput JSON: %v", err)
	}
	return out
}

// grepCall invokes grep through the client and decodes the structured
// GrepOutput.
func grepCall(t *testing.T, cs *mcp.ClientSession, args map[string]any) GrepOutput {
	t.Helper()
	var out GrepOutput
	if err := json.Unmarshal([]byte(wantOK(t, cs, "grep", args)), &out); err != nil {
		t.Fatalf("grep result is not GrepOutput JSON: %v", err)
	}
	return out
}

// grepTreeFixture builds the E2E search tree (no matching binaries: rg would
// list them in -l/-c while the fallback skips them, so consistency fixtures
// keep binaries match-free; no hidden or ignored files either — rg's ignore
// semantics have no fallback counterpart):
//
//	one.txt      "alpha needle here\nplain line\nneedle again\n"
//	sub/two.py   "def x():\n    needle\n"
//	sub/three.py "nothing at all\n"
//	bin.dat      "x\x00x\n" (binary, no match)
func grepTreeFixture(t *testing.T, dir string) {
	t.Helper()
	writeDisk(t, filepath.Join(dir, "one.txt"), "alpha needle here\nplain line\nneedle again\n", 0o644)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "sub", "two.py"), "def x():\n    needle\n", 0o644)
	writeDisk(t, filepath.Join(dir, "sub", "three.py"), "nothing at all\n", 0o644)
	writeDisk(t, filepath.Join(dir, "bin.dat"), "x\x00x\n", 0o644)
}

// sortedCopy returns a sorted copy for order-insensitive comparisons (rg's
// file order is nondeterministic; the fallback walks lexically).
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// comparableGrepLines normalizes engine output for the consistency test:
// bare "--" context separators are dropped (their placement is structural
// and engine-order-dependent), every other line keeps its exact shape
// ("path" / "path:count" / "path:line:text" / "path-line-text").
func comparableGrepLines(results []string) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		if r != "--" {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// TestIntegrationGlobMtimeOrder: §9 matrix row 11 — newest first with
// os.Chtimes-controlled times, no spurious note (§5.5).
func TestIntegrationGlobMtimeOrder(t *testing.T) {
	cs, _, dir := startTestServer(t)
	names := []string{"old.txt", "mid.txt", "new.txt"}
	for i, n := range names {
		p := filepath.Join(dir, n)
		writeDisk(t, p, "x\n", 0o644)
		setMtime(t, p, globBaseTime.Add(time.Duration(i)*time.Hour))
	}

	out := globCall(t, cs, "*.txt", nil)
	if len(out.Files) != 3 {
		t.Fatalf("Files = %v, want 3", out.Files)
	}
	wantDesc := []string{"new.txt", "mid.txt", "old.txt"}
	for i, w := range wantDesc {
		if !strings.HasSuffix(out.Files[i], w) {
			t.Errorf("Files[%d] = %s, want suffix %s (mtime desc)", i, out.Files[i], w)
		}
	}
	if out.Note != "" {
		t.Errorf("Note = %q, want empty below the cap", out.Note)
	}
}

// TestIntegrationGlobLimitTruncation: 205 matches against the real 200 cap —
// exactly 200 paths come back, newest-first (the five oldest are cut), with
// the exact showing-X-of-Y note.
func TestIntegrationGlobLimitTruncation(t *testing.T) {
	cs, _, dir := startTestServer(t)
	const total = 205
	for i := 0; i < total; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		writeDisk(t, p, "x\n", 0o644)
		setMtime(t, p, globBaseTime.Add(time.Duration(i)*time.Minute))
	}

	out := globCall(t, cs, "*.txt", nil)
	if len(out.Files) != globMaxResults {
		t.Fatalf("got %d files, want the %d cap", len(out.Files), globMaxResults)
	}
	if !strings.HasSuffix(out.Files[0], "f204.txt") {
		t.Errorf("Files[0] = %s, want the newest match (f204.txt) first", out.Files[0])
	}
	for _, f := range out.Files {
		for _, old := range []string{"f000.txt", "f001.txt", "f002.txt", "f003.txt", "f004.txt"} {
			if strings.HasSuffix(f, old) {
				t.Errorf("Files contains %s: the oldest matches must be cut by the cap", f)
			}
		}
	}
	want := fmt.Sprintf("showing %d of %d matches (limit %d); narrow the pattern or set path to a subdirectory",
		globMaxResults, total, globMaxResults)
	if out.Note != want {
		t.Errorf("Note = %q, want %q", out.Note, want)
	}
}

// TestIntegrationGlobDoublestarDepth: "**" crosses directories at any depth
// including zero levels, a bare "*" stays at the root, and "**/" may consume
// zero directories (§5.5).
func TestIntegrationGlobDoublestarDepth(t *testing.T) {
	cs, _, dir := startTestServer(t)
	writeDisk(t, filepath.Join(dir, "top.txt"), "x\n", 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "sub", "mid.txt"), "x\n", 0o644)
	writeDisk(t, filepath.Join(dir, "sub", "deep", "low.txt"), "x\n", 0o644)

	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{"**", []string{"top.txt", "sub/mid.txt", "sub/deep/low.txt"}},
		{"*.txt", []string{"top.txt"}},
		{"**/*.txt", []string{"top.txt", "sub/mid.txt", "sub/deep/low.txt"}},
		{"sub/**/*.txt", []string{"sub/mid.txt", "sub/deep/low.txt"}},
	} {
		out := globCall(t, cs, tc.pattern, nil)
		if len(out.Files) != len(tc.want) {
			t.Errorf("pattern %q: Files = %v, want %d entries", tc.pattern, out.Files, len(tc.want))
			continue
		}
		got := make(map[string]bool, len(out.Files))
		for _, f := range out.Files {
			rel, err := filepath.Rel(dir, f)
			if err != nil {
				t.Fatal(err)
			}
			got[filepath.ToSlash(rel)] = true
		}
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("pattern %q: missing %q in Files %v", tc.pattern, w, out.Files)
			}
		}
	}
}

// TestIntegrationGlobPathNarrows: a path argument narrows the search root and
// re-anchors the pattern's relative base (§5.5).
func TestIntegrationGlobPathNarrows(t *testing.T) {
	cs, _, dir := startTestServer(t)
	writeDisk(t, filepath.Join(dir, "root.txt"), "x\n", 0o644)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "sub", "inner.txt"), "x\n", 0o644)

	// "*.txt" is relative to the given root, so it only sees sub/inner.txt.
	out := globCall(t, cs, "*.txt", map[string]any{"path": filepath.Join(dir, "sub")})
	want := []string{filepath.Join(dir, "sub", "inner.txt")}
	if len(out.Files) != 1 || out.Files[0] != want[0] {
		t.Errorf("Files = %v, want %v (path narrows the root)", out.Files, want)
	}
	// Without path, the same pattern sees only the allowed root's top level;
	// "**/" spans zero and one level and sees both files.
	out = globCall(t, cs, "**/*.txt", nil)
	if len(out.Files) != 2 {
		t.Errorf("Files = %v, want root.txt and sub/inner.txt", out.Files)
	}
}

// TestIntegrationGrepFallbackModesE2E: the §9 row 12 shape tests against the
// forced fallback engine (rgBin injected empty), all through the client:
// three output modes, no-match-is-empty-not-error, case_insensitivity, glob
// filtering, and head_limit truncation with its note.
func TestIntegrationGrepFallbackModesE2E(t *testing.T) {
	forceFallback(t)
	cs, _, dir := startTestServer(t)
	grepTreeFixture(t, dir)
	one, two := filepath.Join(dir, "one.txt"), filepath.Join(dir, "sub", "two.py")

	// files_with_matches (default mode, omitted output_mode): sorted because
	// only the fallback's lexical order is deterministic; rg's is not.
	out := grepCall(t, cs, map[string]any{"pattern": "needle"})
	want := sortedCopy([]string{one, two})
	if !equalStrings(sortedCopy(out.Results), want) {
		t.Errorf("files_with_matches: Results = %v, want %v (bin.dat is binary-skipped)", out.Results, want)
	}
	if out.Note != grepFallbackNote {
		t.Errorf("Note = %q, want exactly the fallback notice %q", out.Note, grepFallbackNote)
	}

	// count: path:count per file.
	out = grepCall(t, cs, map[string]any{"pattern": "needle", "output_mode": "count"})
	want = sortedCopy([]string{one + ":2", two + ":1"})
	if !equalStrings(sortedCopy(out.Results), want) {
		t.Errorf("count: Results = %v, want %v", out.Results, want)
	}

	// content: path:line:text passthrough shape.
	out = grepCall(t, cs, map[string]any{"pattern": "needle", "output_mode": "content"})
	want = sortedCopy([]string{
		one + ":1:alpha needle here",
		one + ":3:needle again",
		two + ":2:    needle",
	})
	if !equalStrings(sortedCopy(out.Results), want) {
		t.Errorf("content: Results = %v, want %v", out.Results, want)
	}

	// No match is an empty success, never an error (§5.6 / §9 row 12).
	out = grepCall(t, cs, map[string]any{"pattern": "absent-pattern", "output_mode": "content"})
	if len(out.Results) != 0 {
		t.Errorf("Results = %v, want empty for no matches", out.Results)
	}

	// case_insensitive: "NEEDLE" matches only with the flag.
	out = grepCall(t, cs, map[string]any{"pattern": "NEEDLE"})
	if len(out.Results) != 0 {
		t.Errorf("Results = %v, want empty: default search is case-sensitive", out.Results)
	}
	out = grepCall(t, cs, map[string]any{"pattern": "NEEDLE", "case_insensitive": true, "output_mode": "content"})
	if len(out.Results) != 3 {
		t.Errorf("Results = %v, want the 3 case-folded matches", out.Results)
	}

	// glob filter: basename globs match at any depth (rg semantics).
	out = grepCall(t, cs, map[string]any{"pattern": "needle", "glob": "*.py"})
	if !equalStrings(sortedCopy(out.Results), []string{two}) {
		t.Errorf("glob *.py: Results = %v, want [%s]", out.Results, two)
	}

	// head_limit: server-side truncation of the fallback's deterministic
	// order (one.txt lines before sub/two.py) plus the note.
	out = grepCall(t, cs, map[string]any{"pattern": "needle", "output_mode": "content", "head_limit": 2})
	wantFirst2 := []string{one + ":1:alpha needle here", one + ":3:needle again"}
	if !equalStrings(out.Results, wantFirst2) {
		t.Errorf("head_limit: Results = %v, want %v", out.Results, wantFirst2)
	}
	if !strings.Contains(out.Note, "showing first 2 of 3 results") {
		t.Errorf("Note = %q, missing the head_limit truncation notice", out.Note)
	}
}

// TestIntegrationGrepEngineConsistency: with a real rg on PATH, run the same
// query matrix through the rg path and the forced fallback and require
// identical (order-normalized) result shapes — the §5.6 fallback contract.
// All rg runs happen before rgBin is switched to the fallback (the switch is
// restored by t.Cleanup). Skipped entirely when rg is unavailable.
func TestIntegrationGrepEngineConsistency(t *testing.T) {
	if rgBin == "" {
		t.Skip("rg not available: consistency needs both engines")
	}
	cs, _, dir := startTestServer(t)
	grepTreeFixture(t, dir)
	one, two := filepath.Join(dir, "one.txt"), filepath.Join(dir, "sub", "two.py")

	queries := []struct {
		name string
		args map[string]any
	}{
		{"files_with_matches", map[string]any{"pattern": "needle"}},
		{"count", map[string]any{"pattern": "needle", "output_mode": "count"}},
		{"content", map[string]any{"pattern": "needle", "output_mode": "content"}},
		{"case_insensitive content", map[string]any{
			"pattern": "NEEDLE", "output_mode": "content", "case_insensitive": true}},
		{"glob basename", map[string]any{"pattern": "needle", "glob": "*.py"}},
		{"glob separator absolute-path match", map[string]any{
			"pattern": "needle", "glob": "**/sub/*.py"}},
		{"glob negation", map[string]any{"pattern": "needle", "glob": "!*.py"}},
		{"content with context", map[string]any{
			"pattern": "^def", "output_mode": "content", "context": 1}},
	}

	rgOuts := make([]GrepOutput, len(queries))
	for i, q := range queries {
		rgOuts[i] = grepCall(t, cs, q.args)
		if len(rgOuts[i].Results) == 0 {
			t.Errorf("%s: rg path returned no results", q.name)
		}
		if rgOuts[i].Note != "" {
			t.Errorf("%s: rg path Note = %q, want empty (no fallback/timeout notes)", q.name, rgOuts[i].Note)
		}
	}

	forceFallback(t)
	for i, q := range queries {
		fbOut := grepCall(t, cs, q.args)
		if !strings.Contains(fbOut.Note, grepFallbackNote) {
			t.Errorf("%s: fallback Note = %q, missing the fallback notice", q.name, fbOut.Note)
		}
		rgNorm := comparableGrepLines(rgOuts[i].Results)
		fbNorm := comparableGrepLines(fbOut.Results)
		if !equalStrings(rgNorm, fbNorm) {
			t.Errorf("%s: engine results diverge\n rg: %v\n fb: %v", q.name, rgNorm, fbNorm)
		}
	}

	// Spot-check one fully expanded expectation so "both engines agree" can-
	// not pass on a shared mistake: the rg run for plain "needle" content must
	// have found exactly these lines.
	want := sortedCopy([]string{
		one + ":1:alpha needle here",
		one + ":3:needle again",
		two + ":2:    needle",
	})
	if got := comparableGrepLines(rgOuts[2].Results); !equalStrings(got, want) {
		t.Errorf("rg content spot-check: Results = %v, want %v", got, want)
	}
}

// TestIntegrationReadLongLineTruncated: §9 row 3 through the client — a
// single line longer than MaxLineChars comes back cut to the budget and
// suffixed with the truncation marker, with the line-length footer.
func TestIntegrationReadLongLineTruncated(t *testing.T) {
	cs, _, dir := startTestServer(t)
	p := filepath.Join(dir, "long.txt")
	writeDisk(t, p, strings.Repeat("a", lineio.MaxLineChars+500)+"\n", 0o644)

	out := readTool(t, cs, p, nil)
	want := "1\t" + strings.Repeat("a", lineio.MaxLineChars) + lineio.TruncationMarker
	if out.Content != want {
		t.Errorf("Content = %q..., want the first %d chars + %q",
			truncateForLog(out.Content, 80), lineio.MaxLineChars, lineio.TruncationMarker)
	}
	if !strings.Contains(out.Footer, lineio.ReasonLineLength) {
		t.Errorf("Footer = %q, want it to name %q", out.Footer, lineio.ReasonLineLength)
	}
}

// TestIntegrationReadOffsetContinuation: §9 row 2 through the client — past
// the line limit, the plain read carries a continuation footer and the
// follow-up offset read resumes with continuous absolute line numbers.
//
// lineio.DefaultLines (2000 in production) is a var; this test shrinks it to
// 3 so the boundary is crossed without a 2000-line fixture, restoring it via
// t.Cleanup. This is only safe because this package's tests never use
// t.Parallel — the var is process-global.
func TestIntegrationReadOffsetContinuation(t *testing.T) {
	oldLines := lineio.DefaultLines
	lineio.DefaultLines = 3
	t.Cleanup(func() { lineio.DefaultLines = oldLines })

	cs, _, dir := startTestServer(t)
	var b strings.Builder
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "l%d\n", i)
	}
	p := filepath.Join(dir, "lines.txt")
	writeDisk(t, p, b.String(), 0o644)

	out := readTool(t, cs, p, nil)
	if out.Content != "1\tl1\n2\tl2\n3\tl3" {
		t.Errorf("first read Content = %q, want lines 1-3", out.Content)
	}
	if !strings.Contains(out.Footer, "lines 1-3 of 8") ||
		!strings.Contains(out.Footer, "continue with offset=4") {
		t.Errorf("first read Footer = %q, want range 1-3 of 8 and offset=4", out.Footer)
	}

	// Continuation: numbering picks up at 4 with no gaps or restarts.
	out = readTool(t, cs, p, map[string]any{"offset": 4})
	if out.Content != "4\tl4\n5\tl5\n6\tl6" {
		t.Errorf("offset=4 Content = %q, want lines 4-6 (continuous numbering)", out.Content)
	}
	if !strings.Contains(out.Footer, "continue with offset=7") {
		t.Errorf("offset=4 Footer = %q, want continue with offset=7", out.Footer)
	}

	// Final page reaches EOF: lines 7-8 and no footer.
	out = readTool(t, cs, p, map[string]any{"offset": 7})
	if out.Content != "7\tl7\n8\tl8" {
		t.Errorf("offset=7 Content = %q, want lines 7-8", out.Content)
	}
	if out.Footer != "" {
		t.Errorf("offset=7 Footer = %q, want empty at EOF", out.Footer)
	}
}

// equalStrings is a nil-tolerant slice equality (JSON round-trips make
// "empty" both nil and []).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// truncateForLog shortens s for failure messages.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
