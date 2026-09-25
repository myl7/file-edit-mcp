package contentwarn

import (
	"reflect"
	"strings"
	"testing"
)

// wantTail is the fixed tail of the warning sentence, from the " — " after
// the position list to the end (single line, no trailing newline). The
// backslashes in "(e.g. \b, \v)" are literal text shown to the model.
const wantTail = ` — these often result from escape sequences (e.g. \b, \v) being corrupted upstream before this tool received them; the file was written exactly as received; if this is unintended, read the file back and rewrite the affected text with corrected escapes`

// wantSentence composes the full expected warning from its variable LIST
// segment; the one-finding case additionally pins the complete literal.
func wantSentence(what, list string) string {
	return "content warning: " + what + " contains suspicious control characters: " + list + wantTail
}

// TestCheckClean asserts that text without suspicious control characters —
// including the three legitimate C0 characters, non-ASCII runes, real
// backslashes, and C1 controls above the suspicious range — yields nil.
func TestCheckClean(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"empty text", ""},
		{"plain ASCII", "hello, world"},
		{"tab, newline, CRLF, lone CR", "a\tb\nc\r\nd\re"},
		{"non-ASCII runes", "αβγ — εἷς θεός"},
		{"LaTeX with real backslashes", "$$\n\\beta_i + \\varepsilon_i\n$$"},
		{"C1 control above the suspicious range", "à\u0080b"},
	}
	for _, tc := range cases {
		if got := Check(tc.text); got != nil {
			t.Errorf("%s: Check(%q) = %v, want nil", tc.name, tc.text, got)
		}
	}
}

// TestCheckDetectsEachCodePoint asserts each representative suspicious
// code point is detected alone in the text with its exact Unicode name.
func TestCheckDetectsEachCodePoint(t *testing.T) {
	cases := []struct {
		name     string
		r        rune
		wantName string
	}{
		{"NUL", 0x00, "NULL"},
		{"SOH", 0x01, "START OF HEADING"},
		{"BACKSPACE", 0x08, "BACKSPACE"},
		{"VT", 0x0B, "LINE TABULATION"},
		{"FF", 0x0C, "FORM FEED"},
		{"ESC", 0x1B, "ESCAPE"},
		{"US", 0x1F, "INFORMATION SEPARATOR ONE"},
		{"DEL", 0x7F, "DELETE"},
	}
	for _, tc := range cases {
		got := Check(string(tc.r))
		want := []Finding{{Rune: tc.r, Name: tc.wantName, Line: 1, Col: 1}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Check(%q) = %v, want %v", tc.name, string(tc.r), got, want)
		}
	}
}

// TestCheckPositions asserts exact line/col accounting: lines split on
// '\n' only, columns count runes rather than bytes, and '\r' advances the
// column without ever starting a line. The last case is the observed
// upstream corruption shape (a LaTeX \beta and \varepsilon mangled into
// U+0008+"eta" and U+000B+"epsilon").
func TestCheckPositions(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Finding
	}{
		{
			name: "control char after a newline",
			text: "ab\nc\x08 d",
			want: []Finding{{Rune: 0x08, Name: "BACKSPACE", Line: 2, Col: 2}},
		},
		{
			name: "column counts runes, not bytes",
			text: "αβ\x08",
			want: []Finding{{Rune: 0x08, Name: "BACKSPACE", Line: 1, Col: 3}},
		},
		{
			name: "CRLF puts the next rune on a new line",
			text: "a\r\n\x0c",
			want: []Finding{{Rune: 0x0C, Name: "FORM FEED", Line: 2, Col: 1}},
		},
		{
			name: "CR advances the column but stays in its line",
			text: "x\r\x07y",
			want: []Finding{{Rune: 0x07, Name: "ALERT", Line: 1, Col: 3}},
		},
		{
			name: "observed upstream corruption shape",
			text: "R_i - R_f = \x08eta_i (R_m - R_f) + \x0bepsilon_i",
			want: []Finding{
				{Rune: 0x08, Name: "BACKSPACE", Line: 1, Col: 13},
				{Rune: 0x0B, Name: "LINE TABULATION", Line: 1, Col: 34},
			},
		},
	}
	for _, tc := range cases {
		if got := Check(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Check(%q) = %v, want %v", tc.name, tc.text, got, tc.want)
		}
	}
}

// TestCheckOrdering asserts findings come out sorted by (line, col) across
// several lines and columns, even though multiple code points interleave.
func TestCheckOrdering(t *testing.T) {
	got := Check("\x1f\x00\nab\x0b\nc\x08\x0c")
	want := []Finding{
		{Rune: 0x1F, Name: "INFORMATION SEPARATOR ONE", Line: 1, Col: 1},
		{Rune: 0x00, Name: "NULL", Line: 1, Col: 2},
		{Rune: 0x0B, Name: "LINE TABULATION", Line: 2, Col: 3},
		{Rune: 0x08, Name: "BACKSPACE", Line: 3, Col: 2},
		{Rune: 0x0C, Name: "FORM FEED", Line: 3, Col: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Check() = %v, want %v", got, want)
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if prev.Line > cur.Line || (prev.Line == cur.Line && prev.Col >= cur.Col) {
			t.Errorf("finding %d (%v) not ordered after finding %d (%v)", i, cur, i-1, prev)
		}
	}
}

// TestRender asserts the exact warning wording: group prefixes, shared
// ", line L col C" continuations, the "; " group separator, the
// five-position cap with "+N more", and the empty case.
func TestRender(t *testing.T) {
	cases := []struct {
		name     string
		what     string
		findings []Finding
		want     string
	}{
		{
			name:     "empty findings render empty",
			what:     "content",
			findings: nil,
			want:     "",
		},
		{
			name:     "one finding, whole sentence pinned",
			what:     "new_string",
			findings: []Finding{{Rune: 0x08, Name: "BACKSPACE", Line: 13, Col: 18}},
			want: "content warning: new_string contains suspicious control characters: U+0008 (BACKSPACE) at line 13 col 18" +
				` — these often result from escape sequences (e.g. \b, \v) being corrupted upstream before this tool received them; the file was written exactly as received; if this is unintended, read the file back and rewrite the affected text with corrected escapes`,
		},
		{
			name: "same code point shares one group prefix",
			what: "content",
			findings: []Finding{
				{Rune: 0x08, Name: "BACKSPACE", Line: 2, Col: 5},
				{Rune: 0x08, Name: "BACKSPACE", Line: 5, Col: 1},
				{Rune: 0x08, Name: "BACKSPACE", Line: 9, Col: 14},
			},
			want: wantSentence("content", "U+0008 (BACKSPACE) at line 2 col 5, line 5 col 1, line 9 col 14"),
		},
		{
			name: "two code points join with semicolon",
			what: "edit #2 new_string",
			findings: []Finding{
				{Rune: 0x08, Name: "BACKSPACE", Line: 1, Col: 1},
				{Rune: 0x0B, Name: "LINE TABULATION", Line: 2, Col: 3},
			},
			want: wantSentence("edit #2 new_string",
				"U+0008 (BACKSPACE) at line 1 col 1; U+000B (LINE TABULATION) at line 2 col 3"),
		},
		{
			name: "seven occurrences collapse to five plus more",
			what: "content",
			findings: []Finding{
				{Rune: 0x07, Name: "ALERT", Line: 1, Col: 1},
				{Rune: 0x07, Name: "ALERT", Line: 2, Col: 1},
				{Rune: 0x07, Name: "ALERT", Line: 3, Col: 1},
				{Rune: 0x07, Name: "ALERT", Line: 4, Col: 1},
				{Rune: 0x07, Name: "ALERT", Line: 5, Col: 1},
				{Rune: 0x07, Name: "ALERT", Line: 6, Col: 1},
				{Rune: 0x07, Name: "ALERT", Line: 7, Col: 1},
			},
			want: wantSentence("content",
				"U+0007 (ALERT) at line 1 col 1, line 2 col 1, line 3 col 1, line 4 col 1, line 5 col 1 +2 more"),
		},
		{
			name: "position cap cut across groups",
			what: "content",
			findings: []Finding{
				{Rune: 0x08, Name: "BACKSPACE", Line: 1, Col: 1},
				{Rune: 0x08, Name: "BACKSPACE", Line: 2, Col: 2},
				{Rune: 0x0B, Name: "LINE TABULATION", Line: 3, Col: 1},
				{Rune: 0x0B, Name: "LINE TABULATION", Line: 4, Col: 2},
				{Rune: 0x0B, Name: "LINE TABULATION", Line: 5, Col: 3},
				{Rune: 0x0B, Name: "LINE TABULATION", Line: 6, Col: 4},
				{Rune: 0x0B, Name: "LINE TABULATION", Line: 7, Col: 5},
			},
			want: wantSentence("content",
				"U+0008 (BACKSPACE) at line 1 col 1, line 2 col 2; U+000B (LINE TABULATION) at line 3 col 1, line 4 col 2, line 5 col 3 +2 more"),
		},
		{
			name:     "uppercase hex code point",
			what:     "new_string",
			findings: []Finding{{Rune: 0x1B, Name: "ESCAPE", Line: 3, Col: 7}},
			want:     wantSentence("new_string", "U+001B (ESCAPE) at line 3 col 7"),
		},
	}
	for _, tc := range cases {
		got := Render(tc.what, tc.findings)
		if got != tc.want {
			t.Errorf("%s: Render(%q, ...) =\n%s\nwant\n%s", tc.name, tc.what, got, tc.want)
		}
		if len(tc.findings) == 0 {
			continue
		}
		// Wording invariants that must hold for every non-empty sentence.
		if !strings.Contains(got, "written exactly as received") {
			t.Errorf("%s: sentence lacks \"written exactly as received\": %s", tc.name, got)
		}
		if !strings.Contains(got, "corrupted upstream before this tool received them") {
			t.Errorf("%s: sentence lacks the upstream-cause clause: %s", tc.name, got)
		}
		if strings.Contains(got, "server") {
			t.Errorf("%s: sentence must never claim server-side corruption: %s", tc.name, got)
		}
		if strings.Contains(got, "\n") || strings.HasSuffix(got, "\n") {
			t.Errorf("%s: sentence must be a single line: %q", tc.name, got)
		}
	}
}
