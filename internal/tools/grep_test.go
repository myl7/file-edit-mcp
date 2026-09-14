// grep tool tests (ARCHITECTURE §5.6 / §9 matrix row 12): the rg argument
// table, the exit-code/timeout contract (via a fake rg script injected into
// the rgBin var), and the WalkDir+regexp fallback engine's three output
// modes, binary skipping, glob filtering, and context-line shape.
//
// Several tests override the rgBin/grepTimeout package vars; they restore
// via t.Cleanup and must not use t.Parallel (the whole package runs
// serially, and no other test reads these vars concurrently).

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// forceFallback points rgBin at "" for the test's duration.
func forceFallback(t *testing.T) {
	t.Helper()
	old := rgBin
	rgBin = ""
	t.Cleanup(func() { rgBin = old })
}

// fakeRG points rgBin at a freshly written executable script. The path is
// warmed with one throwaway exec first: on macOS the first exec of a
// freshly written binary at a given path is gated for ~300ms (code-signing
// validation), which would eat a short test deadline before the script ever
// runs. Production rg is an installed binary and never pays this.
func fakeRG(t *testing.T, script string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-rg")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	warm := exec.Command(p)
	warm.Stdout = nil
	_ = warm.Run()
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := rgBin
	rgBin = p
	t.Cleanup(func() { rgBin = old })
}

// overrideDuration swaps *dst for a test duration and restores on cleanup.
func overrideDuration(t *testing.T, dst *time.Duration, v time.Duration) {
	t.Helper()
	old := *dst
	*dst = v
	t.Cleanup(func() { *dst = old })
}

// callGrep invokes the Grep handler directly and fails on error.
func callGrep(t *testing.T, s *Conn, in GrepInput) GrepOutput {
	t.Helper()
	_, out, err := s.Grep(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("grep %+v: %v", in, err)
	}
	return out
}

// grepErr invokes the Grep handler directly and asserts failure substrings.
func grepErr(t *testing.T, s *Conn, in GrepInput, substrs ...string) {
	t.Helper()
	_, _, err := s.Grep(context.Background(), nil, in)
	if err == nil {
		t.Fatalf("grep %+v: expected error, got success", in)
	}
	for _, sub := range substrs {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("grep error %q missing substring %q", err, sub)
		}
	}
}

// grepFixture builds the standard search tree:
//
//	one.txt        "alpha needle here\nplain line\nneedle again\n"
//	sub/two.py     "def x():\n    needle\n"
//	sub/three.py   "nothing at all\n"
//	bin.dat        "needle\x00binary\n" (NUL in the first 8KiB)
func grepFixture(t *testing.T, dir string) {
	t.Helper()
	writeDisk(t, filepath.Join(dir, "one.txt"), "alpha needle here\nplain line\nneedle again\n", 0o644)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(dir, "sub", "two.py"), "def x():\n    needle\n", 0o644)
	writeDisk(t, filepath.Join(dir, "sub", "three.py"), "nothing at all\n", 0o644)
	writeDisk(t, filepath.Join(dir, "bin.dat"), "needle\x00binary\n", 0o644)
}

func TestBuildRGArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   GrepInput
		mode string
		roots []string
		want []string
	}{
		{
			name: "defaults: files_with_matches",
			in:   GrepInput{Pattern: "x"},
			mode: grepModeFiles,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-l", "-e", "x", "/r"},
		},
		{
			name: "content adds -n",
			in:   GrepInput{Pattern: "x"},
			mode: grepModeOutput,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-n", "-e", "x", "/r"},
		},
		{
			name: "count adds -c",
			in:   GrepInput{Pattern: "x"},
			mode: grepModeCount,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-c", "-e", "x", "/r"},
		},
		{
			name: "case_insensitive adds -i",
			in:   GrepInput{Pattern: "x", CaseInsensitive: true},
			mode: grepModeFiles,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-l", "-i", "-e", "x", "/r"},
		},
		{
			name: "context in content mode adds -C N",
			in:   GrepInput{Pattern: "x", Context: 2},
			mode: grepModeOutput,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-n", "-C", "2", "-e", "x", "/r"},
		},
		{
			name: "context outside content mode adds nothing (rejected by the handler)",
			in:   GrepInput{Pattern: "x", Context: 1},
			mode: grepModeFiles,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-l", "-e", "x", "/r"},
		},
		{
			name: "glob adds one --glob pair",
			in:   GrepInput{Pattern: "x", Glob: "*.py"},
			mode: grepModeFiles,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-l", "--glob", "*.py", "-e", "x", "/r"},
		},
		{
			name: "exclusion glob is passed through as-is",
			in:   GrepInput{Pattern: "x", Glob: "!skip/**"},
			mode: grepModeFiles,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-l", "--glob", "!skip/**", "-e", "x", "/r"},
		},
		{
			name: "multiple roots become positional paths",
			in:   GrepInput{Pattern: "x"},
			mode: grepModeFiles,
			roots: []string{"/r1", "/r2"},
			want: []string{"--with-filename", "--no-heading", "-l", "-e", "x", "/r1", "/r2"},
		},
		{
			// A dash-leading pattern must stay a pattern: on -e it cannot be
			// mistaken for an rg flag (which would otherwise turn the search
			// root into the pattern and scan rg's own cwd).
			name: "dash-leading pattern is safe on -e",
			in:   GrepInput{Pattern: "-foo"},
			mode: grepModeFiles,
			roots: []string{"/r"},
			want: []string{"--with-filename", "--no-heading", "-l", "-e", "-foo", "/r"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildRGArgs(tc.in, tc.mode, tc.roots); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildRGArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGrepValidation(t *testing.T) {
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "f.txt"), "x\n", 0o644)

	grepErr(t, s, GrepInput{Pattern: ""}, "pattern must not be empty")
	grepErr(t, s, GrepInput{Pattern: "x", OutputMode: "loud"},
		`invalid output_mode "loud"`)
	grepErr(t, s, GrepInput{Pattern: "x", Context: -1}, "context must be >= 0")
	grepErr(t, s, GrepInput{Pattern: "x", Context: 2}, "context is only valid with output_mode=content")
	grepErr(t, s, GrepInput{Pattern: "x", HeadLimit: -2}, "head_limit must be >= 0")
	// path validation mirrors glob: file root, outside, missing.
	grepErr(t, s, GrepInput{Pattern: "x", Path: filepath.Join(dir, "f.txt")},
		"must be a directory, not a file")
	grepErr(t, s, GrepInput{Pattern: "x", Path: "/etc"}, "outside the allowed directories")
	grepErr(t, s, GrepInput{Pattern: "x", Path: filepath.Join(dir, "gone")},
		"file does not exist")
}

func TestGrepFallbackThreeModes(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	grepFixture(t, dir)

	out := callGrep(t, s, GrepInput{Pattern: "needle"})
	// bin.dat contains the pattern but is skipped by the binary heuristic.
	want := []string{filepath.Join(dir, "one.txt"), filepath.Join(dir, "sub", "two.py")}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("files_with_matches: Results = %v, want %v", out.Results, want)
	}
	if !strings.Contains(out.Note, grepFallbackNote) {
		t.Errorf("Note = %q, want the fallback-engine notice", out.Note)
	}

	out = callGrep(t, s, GrepInput{Pattern: "needle", OutputMode: "count"})
	want = []string{filepath.Join(dir, "one.txt") + ":2", filepath.Join(dir, "sub", "two.py") + ":1"}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("count: Results = %v, want %v", out.Results, want)
	}

	out = callGrep(t, s, GrepInput{Pattern: "needle", OutputMode: "content"})
	want = []string{
		filepath.Join(dir, "one.txt") + ":1:alpha needle here",
		filepath.Join(dir, "one.txt") + ":3:needle again",
		filepath.Join(dir, "sub", "two.py") + ":2:    needle",
	}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("content: Results = %v, want %v", out.Results, want)
	}
}

func TestGrepFallbackNoMatchIsEmptyNotError(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	grepFixture(t, dir)

	out := callGrep(t, s, GrepInput{Pattern: "absent-pattern"})
	if len(out.Results) != 0 {
		t.Errorf("Results = %v, want empty for no matches", out.Results)
	}
}

func TestGrepFallbackCaseInsensitive(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "mixed.txt"), "Hello World\nbye\n", 0o644)

	if out := callGrep(t, s, GrepInput{Pattern: "hello world"}); len(out.Results) != 0 {
		t.Errorf("case-sensitive matched unexpectedly: %v", out.Results)
	}
	out := callGrep(t, s, GrepInput{Pattern: "hello world", CaseInsensitive: true, OutputMode: "content"})
	want := []string{filepath.Join(dir, "mixed.txt") + ":1:Hello World"}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results = %v, want %v", out.Results, want)
	}
}

func TestGrepFallbackGlobFilter(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	grepFixture(t, dir)

	// Basename glob (no "/"): matches at any depth, rg-style.
	out := callGrep(t, s, GrepInput{Pattern: "needle", Glob: "*.py"})
	want := []string{filepath.Join(dir, "sub", "two.py")}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results = %v, want %v", out.Results, want)
	}

	// Separator glob: matched against the full absolute path, so it needs a
	// "**/" prefix to reach nested files (rg behaves identically with an
	// absolute search root).
	out = callGrep(t, s, GrepInput{Pattern: "needle", Glob: "**/sub/*.py"})
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results = %v, want %v", out.Results, want)
	}
	out = callGrep(t, s, GrepInput{Pattern: "needle", Glob: "sub/*.py"})
	if len(out.Results) != 0 {
		t.Errorf("Results = %v, want empty: non-**/ separator globs cannot match an absolute path", out.Results)
	}

	// A negation glob keeps everything not excluded.
	out = callGrep(t, s, GrepInput{Pattern: "needle", Glob: "!*.py"})
	want = []string{filepath.Join(dir, "one.txt")}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results = %v, want %v (bin.dat skipped as binary)", out.Results, want)
	}
}

func TestGrepFallbackBinarySkipped(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	grepFixture(t, dir)

	// bin.dat contains the pattern but also a NUL byte in its first 8KiB:
	// the fallback skips it entirely (§5.6), in every mode.
	for _, mode := range []string{"", "count", "content"} {
		out := callGrep(t, s, GrepInput{Pattern: "needle", OutputMode: mode})
		for _, r := range out.Results {
			if strings.HasSuffix(r, "bin.dat") || strings.Contains(r, "bin.dat:") {
				t.Errorf("mode %q: binary file leaked into results: %v", mode, out.Results)
			}
		}
	}
}

func TestGrepFallbackHeadLimit(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	grepFixture(t, dir)

	out := callGrep(t, s, GrepInput{Pattern: "needle", OutputMode: "content", HeadLimit: 2})
	want := []string{
		filepath.Join(dir, "one.txt") + ":1:alpha needle here",
		filepath.Join(dir, "one.txt") + ":3:needle again",
	}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results = %v, want the first 2 lines", out.Results)
	}
	wantNote := "showing first 2 of 3 results"
	if !strings.Contains(out.Note, wantNote) {
		t.Errorf("Note = %q, missing %q", out.Note, wantNote)
	}
	if !strings.Contains(out.Note, grepFallbackNote) {
		t.Errorf("Note = %q, missing the fallback notice", out.Note)
	}
}

func TestGrepFallbackContextShape(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	// a_ctx.txt: matches at lines 1 and 5 with C=1 form two disjoint groups
	// (1-2 and 4-6); b_solo.txt adds one cross-file group. Expect rg's exact
	// shape: matches "p:n:text", context "p-n-text", "--" between groups.
	writeDisk(t, filepath.Join(dir, "a_ctx.txt"), "m\nx\nx\nx\nm\nx\nx\nx\n", 0o644)
	writeDisk(t, filepath.Join(dir, "b_solo.txt"), "m\n", 0o644)

	out := callGrep(t, s, GrepInput{Pattern: "^m$", OutputMode: "content", Context: 1})
	a, b := filepath.Join(dir, "a_ctx.txt"), filepath.Join(dir, "b_solo.txt")
	want := []string{
		a + ":1:m",
		a + "-2-x",
		"--",
		a + "-4-x",
		a + ":5:m",
		a + "-6-x",
		"--",
		b + ":1:m",
	}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results =\n%v\nwant\n%v", out.Results, want)
	}

	// Adjacent groups merge: matches 1 and 3 with C=1 cover lines 1-4 with
	// no separator. Fresh server so only this file is in scope.
	s2, dir2 := newDirectServer(t)
	writeDisk(t, filepath.Join(dir2, "c_adj.txt"), "m\nx\nm\nx\n", 0o644)
	out = callGrep(t, s2, GrepInput{
		Pattern: "^m$", OutputMode: "content", Context: 1, Path: dir2,
	})
	c := filepath.Join(dir2, "c_adj.txt")
	want = []string{c + ":1:m", c + "-2-x", c + ":3:m", c + "-4-x"}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results = %v, want %v (merged adjacent groups)", out.Results, want)
	}
}

func TestGrepFallbackInvalidRegexIsParamError(t *testing.T) {
	forceFallback(t)
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "f.txt"), "x\n", 0o644)

	grepErr(t, s, GrepInput{Pattern: "("}, "invalid regular expression")
}

func TestGrepFallbackTimeoutNote(t *testing.T) {
	forceFallback(t)
	overrideDuration(t, &grepTimeout, time.Nanosecond)
	s, dir := newDirectServer(t)
	grepFixture(t, dir)

	out := callGrep(t, s, GrepInput{Pattern: "needle"})
	if len(out.Results) != 0 {
		t.Errorf("Results = %v, want empty (deadline passes before scanning)", out.Results)
	}
	if !strings.Contains(out.Note, "search timed out after") ||
		!strings.Contains(out.Note, "results may be incomplete") {
		t.Errorf("Note = %q, want the timeout notice", out.Note)
	}
}

func TestGrepRGExitCodes(t *testing.T) {
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "f.txt"), "x\n", 0o644)

	t.Run("exit 1 is empty success", func(t *testing.T) {
		fakeRG(t, "#!/bin/sh\nexit 1\n")
		out := callGrep(t, s, GrepInput{Pattern: "x"})
		if len(out.Results) != 0 || out.Note != "" {
			t.Errorf("Results = %v Note = %q, want both empty (no match is not an error)", out.Results, out.Note)
		}
	})

	t.Run("exit 2 is an error", func(t *testing.T) {
		fakeRG(t, "#!/bin/sh\necho 'rg: IO error for operation' >&2\nexit 2\n")
		grepErr(t, s, GrepInput{Pattern: "x"}, "rg exited with code 2", "IO error for operation")
	})

	t.Run("regex parse error is a parameter error", func(t *testing.T) {
		fakeRG(t, "#!/bin/sh\necho 'rg: regex parse error:' >&2\necho '    (?:(' >&2\nexit 2\n")
		grepErr(t, s, GrepInput{Pattern: "("}, "invalid regular expression")
	})
}

func TestGrepRGOutputPassthroughAndHeadLimit(t *testing.T) {
	fakeRG(t, "#!/bin/sh\ncat <<'EOF'\n/a.txt:1:one\n/a.txt:5:two\n/b.py:3:three\nEOF\n")
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "f.txt"), "x\n", 0o644)

	// rg output lines are passed through verbatim.
	out := callGrep(t, s, GrepInput{Pattern: "x", OutputMode: "content"})
	want := []string{"/a.txt:1:one", "/a.txt:5:two", "/b.py:3:three"}
	if !reflect.DeepEqual(out.Results, want) {
		t.Errorf("Results = %v, want verbatim passthrough %v", out.Results, want)
	}

	// head_limit truncates server-side with the note.
	out = callGrep(t, s, GrepInput{Pattern: "x", OutputMode: "content", HeadLimit: 2})
	if !reflect.DeepEqual(out.Results, want[:2]) {
		t.Errorf("Results = %v, want %v", out.Results, want[:2])
	}
	if !strings.Contains(out.Note, "showing first 2 of 3 results") {
		t.Errorf("Note = %q, want truncation note", out.Note)
	}
}

func TestGrepRGTimeoutReturnsPartial(t *testing.T) {
	// The fake emits one line every 50ms for ~5s; the 300ms deadline kills
	// it mid-stream. Streaming (rather than echo-then-sleep) makes the test
	// immune to process-startup jitter on the host — whatever number of
	// lines made it into the buffer by the kill must be returned.
	fakeRG(t, "#!/bin/sh\ni=0\nwhile [ $i -lt 100 ]; do\n  echo \"slow:$i\"\n  i=$((i+1))\n  sleep 0.05\ndone\n")
	overrideDuration(t, &grepTimeout, 300*time.Millisecond)
	s, dir := newDirectServer(t)
	writeDisk(t, filepath.Join(dir, "f.txt"), "x\n", 0o644)

	start := time.Now()
	out := callGrep(t, s, GrepInput{Pattern: "x", OutputMode: "content"})
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("grep took %v; the kill must not wait out the orphaned child", d)
	}
	if len(out.Results) == 0 || len(out.Results) >= 100 {
		t.Errorf("Results = %v, want a partial (non-empty, incomplete) set", out.Results)
	}
	for _, r := range out.Results {
		if !strings.HasPrefix(r, "slow:") {
			t.Errorf("unexpected result line %q", r)
		}
	}
	want := "search timed out after 0.3s; results may be incomplete"
	if !strings.Contains(out.Note, want) {
		t.Errorf("Note = %q, missing %q", out.Note, want)
	}
}
