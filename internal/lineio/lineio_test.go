package lineio

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// overrideLimits swaps the package limits for the duration of one test.
// Tests in this package never run in parallel, so this is race-free.
func overrideLimits(t *testing.T, f func()) {
	t.Helper()
	lines, chars, total := DefaultLines, MaxLineChars, MaxTotalBytes
	f()
	t.Cleanup(func() {
		DefaultLines, MaxLineChars, MaxTotalBytes = lines, chars, total
	})
}

// numberedContent builds an n-line file whose line i reads "line i".
func numberedContent(n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return []byte(b.String())
}

// lineNums extracts the leading line numbers from rendered text.
func lineNums(t *testing.T, text string) []int {
	t.Helper()
	var nums []int
	for _, ln := range strings.Split(text, "\n") {
		tab := strings.IndexByte(ln, '\t')
		if tab < 0 {
			t.Fatalf("rendered line lacks <num>\\t prefix: %q", ln)
		}
		num, err := strconv.Atoi(ln[:tab])
		if err != nil {
			t.Fatalf("bad line number %q in %q", ln[:tab], ln)
		}
		nums = append(nums, num)
	}
	return nums
}

func TestRenderPaginationNoOverlapNoGap(t *testing.T) {
	content := numberedContent(3000)

	// Page through with a limit that does not divide 3000, so the final
	// page is short and every other page exhausts the request.
	const limit = 700
	offset, pages := 1, 0
	var all []int
	for {
		res, err := Render(content, offset, limit)
		if err != nil {
			t.Fatalf("page %d: Render: %v", pages+1, err)
		}
		if res.Text == "" {
			t.Fatalf("page %d: unexpected empty text", pages+1)
		}
		if res.TotalLines != 3000 {
			t.Fatalf("page %d: TotalLines = %d, want 3000", pages+1, res.TotalLines)
		}
		if res.FirstLine != offset {
			t.Fatalf("page %d: FirstLine = %d, want %d", pages+1, res.FirstLine, offset)
		}
		for i, ln := range strings.Split(res.Text, "\n") {
			wantLine := fmt.Sprintf("%d\tline %d", offset+i, offset+i)
			if ln != wantLine {
				t.Fatalf("page %d rendered line %d = %q, want %q", pages+1, i+1, ln, wantLine)
			}
		}
		nums := lineNums(t, res.Text)
		if res.LastLine != nums[len(nums)-1] {
			t.Fatalf("page %d: LastLine = %d, want %d", pages+1, res.LastLine, nums[len(nums)-1])
		}
		all = append(all, nums...)
		pages++
		if res.Footer == "" {
			break // reached EOF: complete read
		}
		const cue = "continue with offset="
		i := strings.LastIndex(res.Footer, cue)
		if i < 0 {
			t.Fatalf("page %d: footer lacks continuation cue: %q", pages, res.Footer)
		}
		next, err := strconv.Atoi(res.Footer[i+len(cue):])
		if err != nil {
			t.Fatalf("page %d: bad continuation offset in %q", pages, res.Footer)
		}
		if next != res.LastLine+1 {
			t.Fatalf("page %d: footer says continue with offset=%d, want %d", pages, next, res.LastLine+1)
		}
		offset = next
		if pages > 8 {
			t.Fatal("pagination did not terminate")
		}
	}

	if pages != 5 {
		t.Fatalf("pages = %d, want 5 (700 x4 + 200)", pages)
	}
	if len(all) != 3000 {
		t.Fatalf("rendered %d line numbers in total, want 3000", len(all))
	}
	for i, n := range all {
		if n != i+1 {
			t.Fatalf("line number %d at position %d: overlap or gap", n, i+1)
		}
	}
}

func TestRenderPageFooters(t *testing.T) {
	content := numberedContent(3000)

	// First page at the default limit: request exhausted before EOF.
	res, err := Render(content, 1, DefaultLines)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("lines 1-%d of 3000; %s; continue with offset=%d",
		DefaultLines, ReasonRequestedLimit, DefaultLines+1)
	if res.Footer != want {
		t.Fatalf("footer = %q, want %q", res.Footer, want)
	}
	if res.LastLine != DefaultLines || res.Truncated {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Continuation page reads to EOF: no footer, absolute numbers intact.
	res, err = Render(content, DefaultLines+1, DefaultLines)
	if err != nil {
		t.Fatal(err)
	}
	if res.Footer != "" {
		t.Fatalf("continuation page footer = %q, want empty (complete read)", res.Footer)
	}
	if res.FirstLine != DefaultLines+1 || res.LastLine != 3000 {
		t.Fatalf("bounds = %d-%d, want %d-3000", res.FirstLine, res.LastLine, DefaultLines+1)
	}
	if first := strings.SplitN(res.Text, "\n", 2)[0]; first != fmt.Sprintf("%d\tline %d", DefaultLines+1, DefaultLines+1) {
		t.Fatalf("continuation starts with %q", first)
	}

	// Asking for more than the hard cap still stops at DefaultLines and
	// reports the cap, not the request.
	res, err = Render(content, 1, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if res.LastLine != DefaultLines {
		t.Fatalf("LastLine = %d, want %d (hard cap)", res.LastLine, DefaultLines)
	}
	want = fmt.Sprintf("lines 1-%d of 3000; %s; continue with offset=%d",
		DefaultLines, ReasonLineLimit, DefaultLines+1)
	if res.Footer != want {
		t.Fatalf("footer = %q, want %q", res.Footer, want)
	}

	// Mid-file page with a small request limit.
	res, err = Render(numberedContent(5), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "2\tline 2\n3\tline 3" {
		t.Fatalf("text = %q", res.Text)
	}
	want = fmt.Sprintf("lines 2-3 of 5; %s; continue with offset=4", ReasonRequestedLimit)
	if res.Footer != want {
		t.Fatalf("footer = %q, want %q", res.Footer, want)
	}
}

func TestRenderLongLineTruncation(t *testing.T) {
	// Exactly at the limit: no truncation, no footer.
	exact := strings.Repeat("a", MaxLineChars)
	res, err := Render([]byte(exact+"\n"), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.Footer != "" || res.Text != "1\t"+exact {
		t.Fatalf("line of exactly MaxLineChars must pass through: %+v", res)
	}

	// Over the limit: cut to MaxLineChars runes plus the marker. The read
	// reached EOF, but Truncated alone still forces a footer.
	over := strings.Repeat("a", MaxLineChars+500)
	res, err = Render([]byte(over+"\n"), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantText := "1\t" + exact + TruncationMarker
	if res.Text != wantText {
		t.Fatalf("text = %d bytes, want %d bytes", len(res.Text), len(wantText))
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	wantFooter := fmt.Sprintf("lines 1-1 of 1; %s; continue with offset=2", ReasonLineLength)
	if res.Footer != wantFooter {
		t.Fatalf("footer = %q, want %q", res.Footer, wantFooter)
	}

	// Multibyte content: the budget counts runes after sanitization, not
	// bytes, so MaxLineChars CJK chars (3 bytes each) survive intact.
	cjk := strings.Repeat("世", MaxLineChars+10)
	res, err = Render([]byte(cjk+"\n"), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantDisplay := strings.Repeat("世", MaxLineChars) + TruncationMarker
	if res.Text != "1\t"+wantDisplay {
		t.Fatalf("CJK text = %d bytes, want %d bytes", len(res.Text), len("1\t"+wantDisplay))
	}
	if got := utf8.RuneCountInString(strings.TrimPrefix(res.Text, "1\t")); got != MaxLineChars+len(TruncationMarker) {
		t.Fatalf("rune count after truncation = %d, want %d", got, MaxLineChars+len(TruncationMarker))
	}
}

func TestRenderTotalBytesLimitStopsAtWholeLines(t *testing.T) {
	// A tiny override keeps the fixture small; the logic under test is
	// identical to the production 256 KiB budget.
	overrideLimits(t, func() { MaxTotalBytes = 4 * 1024 })

	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "%0100d\n", i) // each line body exactly 100 bytes
	}
	content := []byte(b.String())

	res, err := Render(content, 1, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Independently derive the expected stop: accumulate whole rendered
	// lines (+1 for the joining \n) until the next one would not fit.
	used, wantLast := 0, 0
	for i := 1; i <= 100; i++ {
		rendered := fmt.Sprintf("%d\t%0100d", i, i)
		if used+len(rendered)+1 > MaxTotalBytes {
			break
		}
		used += len(rendered) + 1
		wantLast = i
	}
	if wantLast == 0 || wantLast == 100 {
		t.Fatalf("test setup failed to exercise a mid-file stop (stopped at %d)", wantLast)
	}
	if res.LastLine != wantLast {
		t.Fatalf("LastLine = %d, want %d (whole-line stop)", res.LastLine, wantLast)
	}
	if len(res.Text) > MaxTotalBytes {
		t.Fatalf("text is %d bytes, exceeds budget %d", len(res.Text), MaxTotalBytes)
	}
	wantFooter := fmt.Sprintf("lines 1-%d of 100; 4 KiB size limit reached; continue with offset=%d", wantLast, wantLast+1)
	if res.Footer != wantFooter {
		t.Fatalf("footer = %q, want %q", res.Footer, wantFooter)
	}
	if res.Truncated {
		t.Fatal("no line hit MaxLineChars; Truncated should be false")
	}
}

func TestRenderDefault256KiBBudget(t *testing.T) {
	// ~303 KB of output against the production 256 KiB budget.
	var b strings.Builder
	for i := 1; i <= 600; i++ {
		fmt.Fprintf(&b, "%0500d\n", i) // each line body exactly 500 bytes
	}
	content := []byte(b.String())

	res, err := Render(content, 1, DefaultLines)
	if err != nil {
		t.Fatal(err)
	}

	used, wantLast := 0, 0
	for i := 1; i <= 600; i++ {
		rendered := fmt.Sprintf("%d\t%0500d", i, i)
		if used+len(rendered)+1 > MaxTotalBytes {
			break
		}
		used += len(rendered) + 1
		wantLast = i
	}
	if wantLast == 0 || wantLast == 600 {
		t.Fatalf("test setup failed to exercise a mid-file stop (stopped at %d)", wantLast)
	}
	if res.LastLine != wantLast {
		t.Fatalf("LastLine = %d, want %d", res.LastLine, wantLast)
	}
	if len(res.Text) > MaxTotalBytes {
		t.Fatalf("text is %d bytes, exceeds budget %d", len(res.Text), MaxTotalBytes)
	}
	wantFooter := fmt.Sprintf("lines 1-%d of 600; 256 KiB size limit reached; continue with offset=%d", wantLast, wantLast+1)
	if res.Footer != wantFooter {
		t.Fatalf("footer = %q, want %q", res.Footer, wantFooter)
	}
}

func TestRenderCRLFPreservedByteExact(t *testing.T) {
	res, err := Render([]byte("one\r\ntwo\r\nthree\r\n"), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\tone\r\n2\ttwo\r\n3\tthree\r"
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	if res.TotalLines != 3 || res.FirstLine != 1 || res.LastLine != 3 {
		t.Fatalf("bounds = %+v", res)
	}
	if res.Footer != "" {
		t.Fatalf("complete read got footer %q", res.Footer)
	}

	// A lone \r inside a line is content too.
	res, err = Render([]byte("a\rb\nc\n"), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "1\ta\rb\n2\tc" {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestRenderInvalidUTF8Replaced(t *testing.T) {
	// \xff\xfe cannot start any valid sequence; \xc3 before \n is a
	// cut-off 2-byte sequence.
	content := []byte("ok\xff\xfe\nbad\xc3\nend\n")
	res, err := Render(content, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\tok\uFFFD\n2\tbad\uFFFD\n3\tend"
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	if !utf8.ValidString(res.Text) {
		t.Fatal("rendered text is not valid UTF-8")
	}
	for _, bad := range []byte{0xff, 0xfe, 0xc3} {
		if strings.IndexByte(res.Text, bad) >= 0 {
			t.Fatalf("raw invalid byte %#x survived into output", bad)
		}
	}
}

func TestRenderCompleteReadHasNoFooter(t *testing.T) {
	cases := []struct {
		name                           string
		content                        string
		offset, limit                  int
		wantText                       string
		wantFirst, wantLast, wantTotal int
	}{
		{"plain", "a\nb\nc\n", 1, 10, "1\ta\n2\tb\n3\tc", 1, 3, 3},
		{"from middle", "a\nb\nc\n", 2, 10, "2\tb\n3\tc", 2, 3, 3},
		{"limit exactly reaches EOF", "a\nb\nc\n", 1, 3, "1\ta\n2\tb\n3\tc", 1, 3, 3},
		{"no trailing newline", "a\nb", 1, 10, "1\ta\n2\tb", 1, 2, 2},
		{"empty line kept", "a\n\nb\n", 1, 10, "1\ta\n2\t\n3\tb", 1, 3, 3},
		{"single newline file", "\n", 1, 10, "1\t", 1, 1, 1},
		{"empty file", "", 1, 10, "", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Render([]byte(c.content), c.offset, c.limit)
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != c.wantText {
				t.Fatalf("text = %q, want %q", res.Text, c.wantText)
			}
			if res.FirstLine != c.wantFirst || res.LastLine != c.wantLast || res.TotalLines != c.wantTotal {
				t.Fatalf("bounds = %d/%d/%d, want %d/%d/%d",
					res.FirstLine, res.LastLine, res.TotalLines, c.wantFirst, c.wantLast, c.wantTotal)
			}
			if res.Footer != "" {
				t.Fatalf("complete read got footer %q", res.Footer)
			}
			if res.Truncated {
				t.Fatal("Truncated = true")
			}
			if len(res.Reasons) != 0 {
				t.Fatalf("Reasons = %v, want none", res.Reasons)
			}
		})
	}
}

func TestRenderOffsetPastEnd(t *testing.T) {
	content := numberedContent(5)

	res, err := Render(content, 6, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "" {
		t.Fatalf("text = %q, want empty", res.Text)
	}
	if res.FirstLine != 0 || res.LastLine != 0 || res.TotalLines != 5 {
		t.Fatalf("bounds = %+v", res)
	}
	if res.Truncated {
		t.Fatal("Truncated = true")
	}
	want := "lines 0-0 of 5; offset beyond end of file; continue with offset=1"
	if res.Footer != want {
		t.Fatalf("footer = %q, want %q", res.Footer, want)
	}

	// Far past the end behaves the same.
	res, err = Render(content, 100, 50)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "" || res.Footer != want {
		t.Fatalf("res = %+v", res)
	}
}

func TestRenderFooterCombinesReasons(t *testing.T) {
	// Ten over-length lines: every line is cut (reason deduplicated to one
	// entry) and the request limit also runs out before EOF.
	long := strings.Repeat("x", MaxLineChars+1)
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		b.WriteString(long)
		b.WriteByte('\n')
	}
	res, err := Render([]byte(b.String()), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("lines 1-10 of 30; %s; %s; continue with offset=11",
		ReasonLineLength, ReasonRequestedLimit)
	if res.Footer != want {
		t.Fatalf("footer = %q, want %q", res.Footer, want)
	}
	if len(res.Reasons) != 2 {
		t.Fatalf("Reasons = %v, want exactly two entries", res.Reasons)
	}
}

func TestValidateArgs(t *testing.T) {
	for _, c := range [][2]int{{0, 10}, {-1, 10}, {1, 0}, {1, -5}, {0, 0}} {
		if err := ValidateArgs(c[0], c[1]); err == nil {
			t.Fatalf("ValidateArgs(%d, %d) = nil, want error", c[0], c[1])
		}
	}
	for _, c := range [][2]int{{1, 1}, {1, 2000}, {5, 700}, {9999, 1}} {
		if err := ValidateArgs(c[0], c[1]); err != nil {
			t.Fatalf("ValidateArgs(%d, %d) = %v, want nil", c[0], c[1], err)
		}
	}
}

func TestRenderRejectsInvalidArgs(t *testing.T) {
	for _, c := range [][2]int{{0, 10}, {1, 0}, {-2, -2}} {
		res, err := Render([]byte("a\n"), c[0], c[1])
		if err == nil {
			t.Fatalf("Render(offset=%d, limit=%d) err = nil, want error", c[0], c[1])
		}
		if res.Text != "" || res.Footer != "" || res.TotalLines != 0 {
			t.Fatalf("Render(offset=%d, limit=%d) result = %+v, want zero value", c[0], c[1], res)
		}
	}
}
