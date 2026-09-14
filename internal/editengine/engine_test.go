package editengine

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// Error-message assertions below assume errmsg renders the §6 templates,
// with EAmbiguous joining line numbers as a ", "-separated decimal list.
// Only stable fragments of the templates are asserted.

func TestApplyOnceUniqueReplace(t *testing.T) {
	content := []byte("func main() {\n\tfmt.Println(\"hi\")\n}\n")
	old := []byte("\tfmt.Println(\"hi\")\n")
	got, matches, err := ApplyOnce(content, old, []byte("\tfmt.Println(\"bye\")\n"), false)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if matches != 1 {
		t.Errorf("matches = %d, want 1", matches)
	}
	want := "func main() {\n\tfmt.Println(\"bye\")\n}\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if string(content) != "func main() {\n\tfmt.Println(\"hi\")\n}\n" {
		t.Errorf("input content was modified: %q", content)
	}
}

func TestApplyOnceWhitespaceIsSignificant(t *testing.T) {
	content := []byte("if x {\n\t\treturn\n\t}\n")
	// Spaces instead of tabs must not match tab-indented source.
	_, _, err := ApplyOnce(content, []byte("    return\n"), []byte("    pass\n"), false)
	if err == nil || !strings.Contains(err.Error(), "old_string not found") {
		t.Errorf("err = %v, want ENoMatch for space-vs-tab indentation", err)
	}
	// Extra trailing whitespace in old_string must not match.
	_, _, err = ApplyOnce(content, []byte("\t\treturn \n"), []byte("\t\tpass\n"), false)
	if err == nil || !strings.Contains(err.Error(), "old_string not found") {
		t.Errorf("err = %v, want ENoMatch for extra trailing whitespace", err)
	}
	// Different amount of interior whitespace must not match either.
	_, _, err = ApplyOnce(content, []byte("if  x"), []byte("if x"), false)
	if err == nil || !strings.Contains(err.Error(), "old_string not found") {
		t.Errorf("err = %v, want ENoMatch for doubled interior space", err)
	}
	// Exact bytes, indentation included, do match.
	got, _, err := ApplyOnce(content, []byte("\t\treturn\n"), []byte("\t\tpass\n"), false)
	if err != nil {
		t.Fatalf("err = %v, want nil for exact match", err)
	}
	if string(got) != "if x {\n\t\tpass\n\t}\n" {
		t.Errorf("got %q", got)
	}
}

func TestApplyOnceEmptyOld(t *testing.T) {
	_, _, err := ApplyOnce([]byte("abc"), nil, []byte("x"), false)
	if err == nil || !strings.Contains(err.Error(), "old_string is empty") {
		t.Fatalf("err = %v, want EEmptyOld", err)
	}
	// Empty old wins even when old == new (both empty) and when replaceAll.
	_, _, err = ApplyOnce([]byte("abc"), []byte{}, []byte{}, true)
	if err == nil || !strings.Contains(err.Error(), "old_string is empty") {
		t.Errorf("err = %v, want EEmptyOld to take precedence over ENoop", err)
	}
}

func TestApplyOnceNoop(t *testing.T) {
	_, matches, err := ApplyOnce([]byte("a b a"), []byte("a"), []byte("a"), false)
	if err == nil || !strings.Contains(err.Error(), "identical to new_string") {
		t.Fatalf("err = %v, want ENoop", err)
	}
	if matches != 0 {
		t.Errorf("matches = %d, want 0 on error", matches)
	}
	// replaceAll does not turn a noop into success.
	_, _, err = ApplyOnce([]byte("a b a"), []byte("a"), []byte("a"), true)
	if err == nil || !strings.Contains(err.Error(), "identical to new_string") {
		t.Errorf("err = %v, want ENoop even with replaceAll", err)
	}
}

func TestApplyOnceNoMatch(t *testing.T) {
	got, matches, err := ApplyOnce([]byte("hello\nworld\n"), []byte("goodbye"), []byte("x"), false)
	if err == nil || !strings.Contains(err.Error(), "old_string not found") {
		t.Fatalf("err = %v, want ENoMatch", err)
	}
	if got != nil || matches != 0 {
		t.Errorf("got = %v, matches = %d, want nil, 0", got, matches)
	}
}

func TestApplyOnceAmbiguous(t *testing.T) {
	content := []byte("target\nfiller\ntarget\nfiller\ntarget\n")
	_, matches, err := ApplyOnce(content, []byte("target"), []byte("x"), false)
	if err == nil {
		t.Fatal("err = nil, want EAmbiguous")
	}
	if matches != 0 {
		t.Errorf("matches = %d, want 0 on error", matches)
	}
	msg := err.Error()
	if !strings.Contains(msg, "matches 3 times") {
		t.Errorf("msg %q missing match count", msg)
	}
	if !strings.Contains(msg, "(first at line(s) 1, 3, 5)") {
		t.Errorf("msg %q missing line numbers 1, 3, 5", msg)
	}
}

func TestApplyOnceAmbiguousCapsLineNumbersAtFive(t *testing.T) {
	// Seven matches on lines 1..7: only the first five lines are reported.
	content := []byte(strings.Repeat("dup\n", 7))
	_, _, err := ApplyOnce(content, []byte("dup"), []byte("x"), false)
	if err == nil {
		t.Fatal("err = nil, want EAmbiguous")
	}
	msg := err.Error()
	if !strings.Contains(msg, "matches 7 times") {
		t.Errorf("msg %q missing match count 7", msg)
	}
	if !strings.Contains(msg, "(first at line(s) 1, 2, 3, 4, 5)") {
		t.Errorf("msg %q should list only first 5 lines", msg)
	}
	if strings.Contains(msg, "6") {
		t.Errorf("msg %q should not mention lines 6 or 7", msg)
	}
}

func TestApplyOnceAmbiguousSameLineMultipleMatches(t *testing.T) {
	// Two matches on one line: both map to that line number.
	_, _, err := ApplyOnce([]byte("a a\n"), []byte("a"), []byte("b"), false)
	if err == nil {
		t.Fatal("err = nil, want EAmbiguous")
	}
	if want := "(first at line(s) 1, 1)"; !strings.Contains(err.Error(), want) {
		t.Errorf("msg %q missing %q", err, want)
	}
}

func TestApplyOnceReplaceAll(t *testing.T) {
	content := []byte("x := 1\ny := x + x\nz := x\n")
	got, matches, err := ApplyOnce(content, []byte("x"), []byte("sum"), true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if matches != 4 {
		t.Errorf("matches = %d, want 4", matches)
	}
	want := "sum := 1\ny := sum + sum\nz := sum\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyOnceReplaceAllNewContainsOld(t *testing.T) {
	// new contains old: a single-pass replace must terminate and match
	// bytes.ReplaceAll exactly.
	content := []byte("ab-cd-ab")
	got, matches, err := ApplyOnce(content, []byte("ab"), []byte("ab+ab"), true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if matches != 2 {
		t.Errorf("matches = %d, want 2", matches)
	}
	if want := string(bytes.ReplaceAll(content, []byte("ab"), []byte("ab+ab"))); string(got) != want {
		t.Errorf("got %q, want %q (strings.ReplaceAll semantics)", got, want)
	}
	// Overlapping pattern "aa" in "aaa": one non-overlapping match, replaced once.
	got, matches, err = ApplyOnce([]byte("aaa"), []byte("aa"), []byte("b"), true)
	if err != nil || matches != 1 || string(got) != "ba" {
		t.Errorf("overlap: got %q, matches %d, err %v; want \"ba\", 1, nil", got, matches, err)
	}
}

func TestApplyOnceCRLF(t *testing.T) {
	content := []byte("line1\r\nline2\r\nline3\r\n")
	// \r is an ordinary byte: old with bare \n must not match CRLF content.
	_, _, err := ApplyOnce(content, []byte("line2\n"), []byte("x\n"), false)
	if err == nil || !strings.Contains(err.Error(), "old_string not found") {
		t.Fatalf("err = %v, want ENoMatch: \\r is significant", err)
	}
	// Exact CRLF anchor matches; \r bytes preserved in output.
	got, _, err := ApplyOnce(content, []byte("line2\r\n"), []byte("LINE2\r\n"), false)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != "line1\r\nLINE2\r\nline3\r\n" {
		t.Errorf("got %q", got)
	}
	// Line numbers count by \n, so \r before \n never shifts a number.
	_, _, err = ApplyOnce(content, []byte("line"), []byte("x"), false)
	if err == nil || !strings.Contains(err.Error(), "(first at line(s) 1, 2, 3)") {
		t.Errorf("err = %v, want EAmbiguous with lines 1, 2, 3", err)
	}
}

func TestApplyOnceBinaryContent(t *testing.T) {
	content := []byte{0x00, 0xFF, 0x01, 0xFF, 0xFE, 0xFF, 0x00}
	got, matches, err := ApplyOnce(content, []byte{0xFF, 0xFE}, []byte{0xEE}, false)
	if err != nil {
		t.Fatalf("err = %v, want nil for binary-exact match", err)
	}
	if matches != 1 {
		t.Errorf("matches = %d, want 1", matches)
	}
	if !bytes.Equal(got, []byte{0x00, 0xFF, 0x01, 0xEE, 0xFF, 0x00}) {
		t.Errorf("got % x", got)
	}
	// Raw 0xFF is not UTF-8 and must never be treated as U+FFFD (EF BF BD).
	_, _, err = ApplyOnce(content, []byte("\uFFFD"), []byte("x"), true)
	if err == nil || !strings.Contains(err.Error(), "old_string not found") {
		t.Errorf("err = %v, want ENoMatch: raw 0xFF is not U+FFFD", err)
	}
	// replaceAll over binary bytes, total count reported.
	got, matches, err = ApplyOnce(content, []byte{0xFF}, []byte{0x41, 0x42}, true)
	if err != nil || matches != 3 || !bytes.Equal(got, []byte{0x00, 0x41, 0x42, 0x01, 0x41, 0x42, 0xFE, 0x41, 0x42, 0x00}) {
		t.Errorf("got % x, matches %d, err %v", got, matches, err)
	}
}

func TestLineNumbers(t *testing.T) {
	content := []byte("a\nbb\nccc\ndd\ne\n")
	// 'b' at byte 2 (line 2), first 'd' at byte 9 (line 4), 'e' at byte 12 (line 5).
	if got := lineNumbers(content, []int{2, 9, 12}); !reflect.DeepEqual(got, []int{2, 4, 5}) {
		t.Errorf("lineNumbers = %v, want [2 4 5]", got)
	}
	if got := lineNumbers(content, nil); len(got) != 0 {
		t.Errorf("lineNumbers(nil) = %v, want empty", got)
	}
}

func TestApplyManySequentialAnchors(t *testing.T) {
	// Edit 2 anchors on text ("three two") that only exists after edit 1 ran.
	content := []byte("one two")
	edits := []Edit{
		{Old: []byte("one"), New: []byte("three")},
		{Old: []byte("three two"), New: []byte("done")},
	}
	got, err := ApplyMany(content, edits)
	if err != nil {
		t.Fatalf("err = %v, want nil: later edits must anchor on earlier output", err)
	}
	if string(got) != "done" {
		t.Errorf("got %q, want %q", got, "done")
	}
}

func TestApplyManyUniquenessAgainstCurrentContent(t *testing.T) {
	// After edit 1 collapses "dup\ndup\n" into "dup\n", edit 2's anchor is
	// unique in the current content even though it appeared twice in the
	// original.
	content := []byte("dup\ndup\nuniq\n")
	edits := []Edit{
		{Old: []byte("dup\ndup\n"), New: []byte("dup\n")},
		{Old: []byte("dup\n"), New: []byte("X\n")},
	}
	got, err := ApplyMany(content, edits)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != "X\nuniq\n" {
		t.Errorf("got %q, want %q", got, "X\nuniq\n")
	}
}

func TestApplyManyFailureRollsBackAndKeepsInput(t *testing.T) {
	content := []byte("keep\nanchor\n")
	backup := append([]byte(nil), content...)
	edits := []Edit{
		{Old: []byte("keep"), New: []byte("KEEP")},
		{Old: []byte("missing"), New: []byte("x")}, // fails: not present
	}
	got, err := ApplyMany(content, edits)
	if err == nil {
		t.Fatal("err = nil, want EEditIndex wrapping ENoMatch")
	}
	if got != nil {
		t.Errorf("got = %q, want nil on failure", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "edit #2 failed") {
		t.Errorf("msg %q missing 1-based edit number", msg)
	}
	if !strings.Contains(msg, "old_string not found") {
		t.Errorf("msg %q missing underlying cause", msg)
	}
	if !bytes.Equal(content, backup) {
		t.Errorf("input content modified: %q, want %q", content, backup)
	}
}

func TestApplyManyEditIndexNumbering(t *testing.T) {
	cases := []struct {
		name  string
		edits []Edit
		want  string
	}{
		{"first edit fails", []Edit{
			{Old: []byte("nope"), New: []byte("x")},
		}, "edit #1 failed"},
		{"second edit fails", []Edit{
			{Old: []byte("a"), New: []byte("b")},
			{Old: []byte(""), New: []byte("x")}, // EEmptyOld
		}, "edit #2 failed"},
		{"third edit fails", []Edit{
			{Old: []byte("a"), New: []byte("b")},
			{Old: []byte("b"), New: []byte("c")},
			{Old: []byte("a"), New: []byte("x"), ReplaceAll: true}, // "a" gone by now
		}, "edit #3 failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyMany([]byte("a"), tc.edits)
			if err == nil {
				t.Fatalf("err = nil, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("msg %q missing %q", err, tc.want)
			}
		})
	}
}

func TestApplyManyMixesReplaceAllAndUnique(t *testing.T) {
	content := []byte("TODO a\nTODO b\ndone\n")
	edits := []Edit{
		{Old: []byte("TODO"), New: []byte("DONE"), ReplaceAll: true},
		{Old: []byte("done\n"), New: []byte("finished\n")}, // lowercase, still unique after edit 1
	}
	got, err := ApplyMany(content, edits)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != "DONE a\nDONE b\nfinished\n" {
		t.Errorf("got %q", got)
	}
}

func TestApplyManyEmpty(t *testing.T) {
	content := []byte("unchanged\n")
	got, err := ApplyMany(content, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil for empty edit list", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("got %q, want %q", got, content)
	}
}
