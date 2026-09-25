// Package contentwarn detects suspicious control characters in the text
// the modifying tools (write/edit/multi_edit) are about to write and
// renders the model-visible warning text. Such characters are the
// signature of escape sequences corrupted upstream before this server
// received them — e.g. a LaTeX \beta that arrives as U+0008 (BACKSPACE)
// followed by "eta", or \varepsilon as U+000B (LINE TABULATION) followed
// by "epsilon". Detection is informational only: it never blocks a write;
// the file is written exactly as received, and the warning rides along on
// the success result.
package contentwarn

import (
	"fmt"
	"strings"
)

// Finding is one detected suspicious control character occurrence.
type Finding struct {
	Rune rune   // the control code point, e.g. 0x0008
	Name string // Unicode name, e.g. "BACKSPACE"
	Line int    // 1-based line number ('\n' splits lines)
	Col  int    // 1-based rune column within the line
}

// names gives the Unicode name of every code point Check can report. The
// stdlib carries no C0 control names, so the table is hand-rolled; the
// three C0 code points excluded from detection (0x09 tab, 0x0A newline,
// 0x0D carriage return) need no entry because they can never be reported.
var names = map[rune]string{
	0x00: "NULL",
	0x01: "START OF HEADING",
	0x02: "START OF TEXT",
	0x03: "END OF TEXT",
	0x04: "END OF TRANSMISSION",
	0x05: "ENQUIRY",
	0x06: "ACKNOWLEDGE",
	0x07: "ALERT",
	0x08: "BACKSPACE",
	0x0B: "LINE TABULATION",
	0x0C: "FORM FEED",
	0x0E: "SHIFT OUT",
	0x0F: "SHIFT IN",
	0x10: "DATA LINK ESCAPE",
	0x11: "DEVICE CONTROL ONE",
	0x12: "DEVICE CONTROL TWO",
	0x13: "DEVICE CONTROL THREE",
	0x14: "DEVICE CONTROL FOUR",
	0x15: "NEGATIVE ACKNOWLEDGE",
	0x16: "SYNCHRONOUS IDLE",
	0x17: "END OF TRANSMISSION BLOCK",
	0x18: "CANCEL",
	0x19: "END OF MEDIUM",
	0x1A: "SUBSTITUTE",
	0x1B: "ESCAPE",
	0x1C: "INFORMATION SEPARATOR FOUR",
	0x1D: "INFORMATION SEPARATOR THREE",
	0x1E: "INFORMATION SEPARATOR TWO",
	0x1F: "INFORMATION SEPARATOR ONE",
	0x7F: "DELETE",
}

// maxListedPositions is how many occurrence positions Render lists across
// all code-point groups before collapsing the rest into "+N more" (the
// same first-5-locations house convention as errmsg's maxAmbiguousLines).
const maxListedPositions = 5

// Check returns every suspicious control character occurrence in text,
// ordered by position (line, then col — a single left-to-right scan, so
// scan order is position order). Clean text returns nil.
//
// A rune is suspicious iff it is a C0 control (<= 0x1F) other than tab,
// newline, and carriage return, or 0x7F (DELETE). Positions are 1-based;
// lines split on '\n' and columns count runes rather than bytes. '\r' is
// excluded from detection but not from accounting: it stays in its line
// and advances the column like any ordinary rune, so a "\r\n" starts a
// new line only because of the '\n'.
func Check(text string) []Finding {
	var findings []Finding
	line, col := 1, 1
	for _, r := range text {
		if suspicious(r) {
			findings = append(findings, Finding{Rune: r, Name: names[r], Line: line, Col: col})
		}
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return findings
}

// suspicious reports whether r is a code point Check reports: a C0 control
// other than the three line-handling characters, or DELETE.
func suspicious(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return false
	}
	return r <= 0x1F || r == 0x7F
}

// Render composes the model-visible warning sentence for the findings
// from one scanned text. what names the scanned subject (e.g. "content",
// "new_string", "edit #2 new_string"). Findings are grouped by code point
// in first-occurrence order; at most maxListedPositions positions are
// listed in total, the rest collapsed into a trailing "+N more". Empty
// findings render "".
//
// The sentence follows the errmsg catalog style: it states the likely
// cause (escape sequences corrupted upstream before this tool received
// them — never anything this server did) and an actionable next step
// (read the file back and rewrite with corrected escapes), and it never
// echoes surrounding file content — only code point, name, line, and col.
func Render(what string, findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}

	// Group by code point, preserving first-occurrence order (map
	// iteration order is not usable for the segment order).
	var order []rune
	byRune := make(map[rune][]Finding)
	for _, f := range findings {
		if _, seen := byRune[f.Rune]; !seen {
			order = append(order, f.Rune)
		}
		byRune[f.Rune] = append(byRune[f.Rune], f)
	}

	// Render group segments under the shared position budget: the first
	// occurrence of a group carries the "U+XXXX (NAME) at" prefix, later
	// ones append bare ", line L col C".
	var segs []string
	listed := 0
	for _, r := range order {
		if listed >= maxListedPositions {
			break
		}
		first := byRune[r][0]
		seg := fmt.Sprintf("U+%04X (%s) at line %d col %d", r, first.Name, first.Line, first.Col)
		listed++
		for _, f := range byRune[r][1:] {
			if listed >= maxListedPositions {
				break
			}
			seg += fmt.Sprintf(", line %d col %d", f.Line, f.Col)
			listed++
		}
		segs = append(segs, seg)
	}
	if listed < len(findings) {
		segs[len(segs)-1] += fmt.Sprintf(" +%d more", len(findings)-listed)
	}

	return fmt.Sprintf(
		"content warning: %s contains suspicious control characters: %s — these often result from escape sequences (e.g. \\b, \\v) being corrupted upstream before this tool received them; the file was written exactly as received; if this is unintended, read the file back and rewrite the affected text with corrected escapes",
		what, strings.Join(segs, "; "))
}
