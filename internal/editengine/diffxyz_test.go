package editengine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// Diff-XYZ layer-1 replay (ARCHITECTURE.md §9, docs/spec.md §5): each
// sample carries old_code / new_code / search-replace from the public
// JetBrains-Research/diff-xyz dataset (test split). The test parses the
// search-replace string into ordered edits, replays them through ApplyMany
// over old_code, and compares the result byte-for-byte with new_code.
//
// Scope: this only checks that the apply logic (block parsing, exact match,
// ordered application, comparison) is not wrong. It is not a product test
// and draws no conclusion about edit formats (spec.md §5: single-turn text
// in/out, no tool call, no filesystem, no retry — none of the behavior this
// engine's error wording targets is exercised).
//
// Data: testdata/diffxyz-30.jsonl is generated offline by
// scripts/fetch_diffxyz.py (100 rows from the HF datasets-server rows API,
// config "default", fixed seed 20260913, 6 rows per language). The test
// never touches the network.

const (
	// diffxyzPath is relative to the package directory.
	diffxyzPath = "../../testdata/diffxyz-30.jsonl"
	// diffxyzTotal is the expected number of samples (5 languages x 6).
	diffxyzTotal = 30
	// diffxyzMinPass is the pass threshold: measured 30/30 when the file
	// was generated; one failure is tolerated for dataset noise (a sample
	// whose search-replace is inherently inconsistent with its new_code,
	// or whose payload collides with a marker line).
	diffxyzMinPass = 29
)

// diffxyzSample is one JSONL record of testdata/diffxyz-30.jsonl.
type diffxyzSample struct {
	Language      string `json:"language"`
	OldCode       string `json:"old_code"`
	NewCode       string `json:"new_code"`
	SearchReplace string `json:"search_replace"`
}

// Diff-xyz search-replace marker lines (as confirmed against the dataset).
const (
	srSearch  = "<<<<<<< SEARCH"
	srDivide  = "======="
	srReplace = ">>>>>>> REPLACE"
)

// parseSearchReplace parses the dataset's search-replace string into
// ordered Edits. The format is one or more blocks, separated by blank
// lines:
//
//	<<<<<<< SEARCH
//	old lines...
//	=======
//	new lines...
//	>>>>>>> REPLACE
//
// Section text is the lines strictly between the marker lines joined with
// "\n". That reproduces the generator's bytes exactly: it emits
// "<marker>\n" + text + "\n" + "<marker>", so a text ending in a newline
// appears as one trailing empty line before the marker. A payload line
// equal to a marker would truncate the section early; such dataset noise
// surfaces as an ordinary failure below.
func parseSearchReplace(s string) ([]Edit, error) {
	var (
		edits []Edit
		lines []string
		old   string
		state int // 0 = between blocks, 1 = in SEARCH, 2 = in REPLACE
	)
	for _, ln := range strings.Split(s, "\n") {
		switch state {
		case 0:
			if ln == srSearch {
				state, lines = 1, nil
			}
		case 1:
			if ln == srDivide {
				old, state, lines = strings.Join(lines, "\n"), 2, nil
			} else {
				lines = append(lines, ln)
			}
		case 2:
			if ln == srReplace {
				edits = append(edits, Edit{
					Old: []byte(old),
					New: []byte(strings.Join(lines, "\n")),
				})
				state = 0
			} else {
				lines = append(lines, ln)
			}
		}
	}
	if state != 0 {
		return nil, fmt.Errorf("malformed search-replace: unterminated block after %d complete block(s)", len(edits))
	}
	if len(edits) == 0 {
		return nil, fmt.Errorf("malformed search-replace: no %s block", srSearch)
	}
	return edits, nil
}

// TestDiffXYZLayer1Apply replays all samples and requires at least
// diffxyzMinPass exact matches. Failures are logged (not fatal) so the
// per-sample score stays visible in -v output.
func TestDiffXYZLayer1Apply(t *testing.T) {
	data, err := os.ReadFile(diffxyzPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with scripts/fetch_diffxyz.py)", diffxyzPath, err)
	}

	type tally struct{ pass, total int }
	perLang := map[string]*tally{}
	total, passed := 0, 0

	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
		var s diffxyzSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("%s line %d: bad JSON: %v", diffxyzPath, i+1, err)
		}
		h := perLang[s.Language]
		if h == nil {
			h = &tally{}
			perLang[s.Language] = h
		}
		h.total++
		where := fmt.Sprintf("%s (sample %d)", s.Language, i+1)

		edits, perr := parseSearchReplace(s.SearchReplace)
		if perr != nil {
			t.Logf("FAIL %s: %v", where, perr)
			continue
		}
		got, aerr := ApplyMany([]byte(s.OldCode), edits)
		if aerr != nil {
			// aerr is EEditIndex-wrapped ("edit #N failed: <cause>") and
			// names the 1-based SEARCH block that failed.
			t.Logf("FAIL %s: %d block(s), error: %v", where, len(edits), aerr)
			continue
		}
		if !bytes.Equal(got, []byte(s.NewCode)) {
			t.Logf("FAIL %s: all %d block(s) applied but result != new_code (got %d bytes, want %d, first diff at byte %d)",
				where, len(edits), len(got), len(s.NewCode), firstDiff(got, []byte(s.NewCode)))
			continue
		}
		passed++
		h.pass++
	}

	if total != diffxyzTotal {
		t.Errorf("total samples = %d, want %d (regenerate testdata with scripts/fetch_diffxyz.py)", total, diffxyzTotal)
	}
	langs := make([]string, 0, len(perLang))
	for l := range perLang {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, l := range langs {
		h := perLang[l]
		t.Logf("summary: %-10s %d/%d", l, h.pass, h.total)
	}
	t.Logf("summary: total %d/%d passed, threshold >= %d", passed, total, diffxyzMinPass)
	if passed < diffxyzMinPass {
		t.Errorf("passed %d of %d, want >= %d; see FAIL lines above (apply logic bug or dataset noise)", passed, total, diffxyzMinPass)
	}
}

// firstDiff returns the offset of the first differing byte, or the length
// of the shorter input when one is a prefix of the other.
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
