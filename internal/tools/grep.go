// grep tool handler (ARCHITECTURE §5.6): pattern search over the (path-
// narrowed) allowed roots via a ripgrep subprocess when available, with a
// WalkDir + Go regexp fallback that mirrors the rg output shape. Server-side
// head_limit truncation and a 120s kill-timeout apply to both engines.

package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rgBin is where ripgrep was found at process start (exec.LookPath). Empty
// means rg is unavailable and every grep call uses the fallback engine. A
// package-level var so tests can inject "" to force the fallback or point it
// at a fake rg binary; tests that mutate it must restore it (t.Cleanup) and
// not run in parallel.
var rgBin = func() string {
	p, err := exec.LookPath("rg")
	if err != nil {
		return ""
	}
	return p
}()

// grepTimeout bounds one grep execution (§5.6: 120s). On expiry the rg
// process is killed (exec.CommandContext) or the fallback walk stops, and
// the partial results are returned with a timeout note. A var so tests can
// shrink it; same serial-test contract as rgBin.
var grepTimeout = 120 * time.Second

// Output mode names (§5.6). files_with_matches is the default.
const (
	grepModeFiles  = "files_with_matches"
	grepModeCount  = "count"
	grepModeOutput = "content"
)

// grepFallbackNote is attached whenever the WalkDir+regexp engine ran (§5.6:
// the fallback has no ignore-file semantics).
const grepFallbackNote = "fallback engine (rg unavailable); no ignore-file semantics"

// grepHeadLimitNoteFmt reports server-side truncation; the %d pairs are the
// number of lines kept, then the total line count before truncation.
const grepHeadLimitNoteFmt = "showing first %d of %d results"

// binarySniffLen is how many leading bytes the fallback inspects for a NUL
// byte to classify a file as binary and skip it (a rough approximation of
// rg's binary detection, §5.6 fallback).
const binarySniffLen = 8 * 1024

// grepScanLineMax is the fallback's per-line buffer cap (content of one line
// plus newline). Lines longer than this abort the file with a logged skip —
// bounded memory matters more than completeness in the no-rg path.
const grepScanLineMax = 1024 * 1024

// Grep implements the grep tool (§5.6). Validation order: pattern non-empty,
// output_mode/context/head_limit shape, then the shared search-root check
// (pathguard; a file is not a valid root). Execution prefers rg (flag
// mapping in buildRGArgs), falls back to the WalkDir engine with identical
// result-line shapes, and finally applies head_limit server-side. Notes
// (timeout, truncation, fallback) are joined with "; ".
func (s *Conn) Grep(ctx context.Context, _ *mcp.CallToolRequest, in GrepInput) (*mcp.CallToolResult, GrepOutput, error) {
	if in.Pattern == "" {
		return nil, GrepOutput{}, errors.New("pattern must not be empty")
	}
	mode := in.OutputMode
	if mode == "" {
		mode = grepModeFiles
	}
	if mode != grepModeFiles && mode != grepModeCount && mode != grepModeOutput {
		return nil, GrepOutput{}, fmt.Errorf("invalid output_mode %q: must be one of files_with_matches, content, count", mode)
	}
	if in.Context < 0 {
		return nil, GrepOutput{}, fmt.Errorf("context must be >= 0, got %d", in.Context)
	}
	// §5.6: context lines only exist in content mode; accepting the flag
	// silently elsewhere would be a no-op lie.
	if in.Context > 0 && mode != grepModeOutput {
		return nil, GrepOutput{}, fmt.Errorf("context is only valid with output_mode=%s", grepModeOutput)
	}
	if in.HeadLimit < 0 {
		return nil, GrepOutput{}, fmt.Errorf("head_limit must be >= 0, got %d", in.HeadLimit)
	}

	roots, err := s.searchRoots(in.Path)
	if err != nil {
		return nil, GrepOutput{}, err
	}

	var (
		lines    []string
		notes    []string
		timedOut bool
	)
	if rgBin != "" {
		lines, timedOut, err = s.grepRG(ctx, in, mode, roots)
	} else {
		lines, timedOut, err = s.grepFallback(ctx, in, mode, roots)
		notes = append(notes, grepFallbackNote)
	}
	if err != nil {
		return nil, GrepOutput{}, err
	}
	if timedOut {
		notes = append(notes, fmt.Sprintf("search timed out after %gs; results may be incomplete", grepTimeout.Seconds()))
	}

	results := lines
	if in.HeadLimit > 0 && len(results) > in.HeadLimit {
		total := len(results)
		results = results[:in.HeadLimit]
		notes = append(notes, fmt.Sprintf(grepHeadLimitNoteFmt, in.HeadLimit, total))
	}
	if results == nil {
		results = []string{}
	}
	return nil, GrepOutput{Results: results, Note: strings.Join(notes, "; ")}, nil
}

// buildRGArgs constructs the rg command line for one grep call. Base flags
// per the T7 contract: --with-filename --no-heading, then per-mode (-l for
// files_with_matches, -n for content, -c for count), -i for
// case_insensitive, -C N for context>0 in content mode, one --glob pair for
// the single glob filter, then the pattern and the search roots.
//
// The pattern rides on -e rather than as a bare positional: a pattern like
// "-foo" would otherwise be eaten by rg's flag parser, and without any
// positional pattern rg silently treats the first search root as the pattern
// and falls back to scanning its own working directory (a bug this -e form
// makes structurally impossible). head_limit is NOT an rg flag — it is
// applied server-side so both engines truncate identically.
func buildRGArgs(in GrepInput, mode string, roots []string) []string {
	args := []string{"--with-filename", "--no-heading"}
	switch mode {
	case grepModeFiles:
		args = append(args, "-l")
	case grepModeOutput:
		args = append(args, "-n")
	case grepModeCount:
		args = append(args, "-c")
	}
	if in.CaseInsensitive {
		args = append(args, "-i")
	}
	if in.Context > 0 && mode == grepModeOutput {
		args = append(args, "-C", strconv.Itoa(in.Context))
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "-e", in.Pattern)
	return append(args, roots...)
}

// grepRG runs the rg subprocess and maps its outcome per §5.6: exit 0 means
// matches (stdout passthrough), exit 1 means no matches (normal empty
// result), >=2 is an error. A context deadline kills the process and
// returns whatever stdout reached the buffer, with timedOut=true. Regex
// parse failures are mapped to the same "invalid regular expression"
// wording the fallback engine produces, so a bad pattern is a parameter
// error regardless of engine.
func (s *Conn) grepRG(ctx context.Context, in GrepInput, mode string, roots []string) (lines []string, timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, rgBin, buildRGArgs(in, mode, roots)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Bound the post-kill wait: if rg (or anything it spawned) still holds
	// the output pipe after the deadline kill, Wait gives up on the copy
	// after this delay and returns, so partial results are not delayed.
	cmd.WaitDelay = 2 * time.Second
	// Neutralize a possible RIPGREP_CONFIG_PATH on the host so the arg table
	// above is the complete, deterministic flag set.
	cmd.Env = append(os.Environ(), "RIPGREP_CONFIG_PATH=")

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		// Killed at the deadline: return the partial stdout (§5.6).
		return splitResultLines(stdout.String()), true, nil
	}
	if runErr == nil {
		return splitResultLines(stdout.String()), false, nil
	}
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		return nil, false, fmt.Errorf("rg failed to run: %w", runErr)
	}
	switch code := ee.ExitCode(); {
	case code == 1:
		// §5.6: no matches is a normal empty result, not an error.
		return []string{}, false, nil
	case code >= 2:
		msg := firstLine(stderr.String())
		if strings.Contains(msg, "regex parse error") {
			// Unify with the fallback: a bad regex is a parameter error.
			return nil, false, fmt.Errorf("invalid regular expression %q: %s", in.Pattern, msg)
		}
		return nil, false, fmt.Errorf("rg exited with code %d: %s", code, msg)
	default:
		// Negative codes: killed by a signal without our deadline (e.g. the
		// 64 MB cgroup OOM from §10). Treat as a hard error.
		return nil, false, fmt.Errorf("rg terminated abnormally (%v)", runErr)
	}
}

// grepFallback is the no-rg engine (§5.6): filepath.WalkDir + Go regexp
// (RE2), no ignore-file semantics, binary files skipped by the NUL-in-first-
// 8KiB heuristic, output lines shaped exactly like rg's. The 120s timeout
// becomes a walk deadline: once passed, traversal stops and the partial
// results return with timedOut=true. Traversal is read-only (§4 waives the
// os.Root requirement for glob/grep walks).
func (s *Conn) grepFallback(ctx context.Context, in GrepInput, mode string, roots []string) (lines []string, timedOut bool, err error) {
	// Compile before any walking so an invalid pattern is a parameter error
	// (mirroring the rg path's unified wording).
	expr := in.Pattern
	if in.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, cerr := regexp.Compile(expr)
	if cerr != nil {
		return nil, false, fmt.Errorf("invalid regular expression %q: %v", in.Pattern, cerr)
	}

	deadline := time.Now().Add(grepTimeout)
	col := &grepCollector{mode: mode, contextLines: in.Context}
	for _, root := range roots {
		if timedOut {
			break
		}
		werr := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				s.log().Warn("grep fallback: skipping unreadable entry", "path", p, "err", werr)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil || time.Now().After(deadline) {
				timedOut = true
				return fs.SkipAll
			}
			if d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			if !grepGlobIncludes(in.Glob, p, filepath.ToSlash(rel)) {
				return nil
			}
			if ferr := s.grepFallbackFile(re, p, col); ferr != nil {
				s.log().Warn("grep fallback: skipping file", "path", p, "err", ferr)
			}
			return nil
		})
		if werr != nil && !errors.Is(werr, fs.SkipAll) {
			return nil, false, fmt.Errorf("grep: walk %s: %w", root, werr)
		}
	}
	col.endFile() // flush the last file (no-op outside content mode)
	return col.lines, timedOut, nil
}

// grepFallbackFile scans one file: skips binaries (NUL in the first
// binarySniffLen bytes), then matches line by line and hands lines to the
// collector. Read errors skip the file (logged by the caller), never fail
// the call.
func (s *Conn) grepFallbackFile(re *regexp.Regexp, absPath string, col *grepCollector) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()

	head := make([]byte, binarySniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil // binary: roughly skipped, §5.6 fallback
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), grepScanLineMax)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		col.line(absPath, lineNo, sc.Text(), re.Match(sc.Bytes()))
	}
	if serr := sc.Err(); serr != nil {
		return serr
	}
	col.endFile()
	return nil
}

// grepCollector renders fallback results in the exact rg output shapes
// (verified against rg 14 with --no-heading and absolute search roots):
//
//   - files_with_matches: "path" per matching file;
//   - count: "path:count" (matching line count per file);
//   - content: "path:line:text" for matching lines, "path-line-text" for
//     context lines, and a bare "--" between disjoint context groups,
//     including across file boundaries.
type grepCollector struct {
	mode         string
	contextLines int
	lines        []string

	// content-mode state for the file being scanned.
	curPath string
	texts   []string
	matches map[int]bool
	emitted bool
}

// line records one scanned line (matched flag per mode).
func (c *grepCollector) line(absPath string, lineNo int, text string, matched bool) {
	switch c.mode {
	case grepModeOutput:
		if len(c.texts) == 0 {
			c.curPath, c.matches = absPath, make(map[int]bool)
		}
		c.texts = append(c.texts, text)
		if matched {
			c.matches[lineNo] = true
		}
	case grepModeCount:
		if matched {
			if len(c.lines) > 0 && strings.HasPrefix(c.lines[len(c.lines)-1], absPath+":") {
				prev := c.lines[len(c.lines)-1]
				if n, err := strconv.Atoi(prev[len(absPath)+1:]); err == nil {
					c.lines[len(c.lines)-1] = fmt.Sprintf("%s:%d", absPath, n+1)
					return
				}
			}
			c.lines = append(c.lines, fmt.Sprintf("%s:1", absPath))
		}
	default: // files_with_matches
		if matched && (len(c.lines) == 0 || c.lines[len(c.lines)-1] != absPath) {
			c.lines = append(c.lines, absPath)
		}
	}
}

// endFile flushes the content-mode context groups of the file just scanned
// (no-op for the other modes).
func (c *grepCollector) endFile() {
	if c.mode != grepModeOutput || len(c.texts) == 0 {
		return
	}
	defer func() { c.texts, c.matches, c.curPath = nil, nil, "" }()

	if len(c.matches) == 0 {
		return
	}
	// Expand matches to [n-C, n+C] and merge overlapping/adjacent ranges;
	// rg prints one "--" between disjoint groups.
	type span struct{ lo, hi int }
	var spans []span
	for ln := 1; ln <= len(c.texts); ln++ {
		if !c.matches[ln] {
			continue
		}
		lo, hi := ln-c.contextLines, ln+c.contextLines
		if lo < 1 {
			lo = 1
		}
		if hi > len(c.texts) {
			hi = len(c.texts)
		}
		if n := len(spans); n > 0 && lo <= spans[n-1].hi+1 {
			if hi > spans[n-1].hi {
				spans[n-1].hi = hi
			}
			continue
		}
		spans = append(spans, span{lo, hi})
	}
	for _, sp := range spans {
		// rg only prints "--" separators when a context flag (-C) is active;
		// a plain -n run emits bare match lines (verified against rg 14).
		if c.emitted && c.contextLines > 0 {
			c.lines = append(c.lines, "--")
		}
		for ln := sp.lo; ln <= sp.hi; ln++ {
			if c.matches[ln] {
				c.lines = append(c.lines, fmt.Sprintf("%s:%d:%s", c.curPath, ln, c.texts[ln-1]))
			} else {
				c.lines = append(c.lines, fmt.Sprintf("%s-%d-%s", c.curPath, ln, c.texts[ln-1]))
			}
		}
		c.emitted = true
	}
}

// grepGlobIncludes applies the single --glob filter with rg's semantics as
// observed with an absolute search root: a glob without "/" is matched
// against the file's base name, one with "/" against the full absolute
// path; a leading "!" excludes matching files instead of including them. An
// empty glob includes everything.
func grepGlobIncludes(glob string, absPath, rel string) bool {
	if glob == "" {
		return true
	}
	neg := strings.HasPrefix(glob, "!")
	pat := strings.TrimPrefix(glob, "!")
	target := absPath
	if !strings.Contains(pat, "/") {
		target = path.Base(rel)
	}
	if doublestar.MatchUnvalidated(filepath.ToSlash(pat), target) {
		return !neg
	}
	return neg
}

// splitResultLines splits engine output into result lines, dropping the
// trailing empty element after the final newline.
func splitResultLines(out string) []string {
	lines := strings.Split(out, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if lines == nil {
		lines = []string{}
	}
	return lines
}

// firstLine returns the first non-empty line of s (for rg stderr messages).
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}
