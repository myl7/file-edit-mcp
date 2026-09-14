// Package lineio implements the rendering half of the read tool
// (ARCHITECTURE §1 and §5.1): splitting file content into lines on \n,
// 1-based absolute line numbers, the three output limits (line count,
// per-line characters, total bytes), UTF-8 sanitization, and the
// continuation footer.
//
// Everything here is pure and in-memory; this package never touches the
// filesystem (reading, retrying, and error cataloging are the tools/
// cifsops/errmsg layers' business). Footers are normal output, not errors,
// so no errmsg entries are involved.
package lineio

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Output limits of the read tool (ARCHITECTURE §5.1). They constrain the
// rendered payload, not the file size. They are vars (not consts) purely so
// tests can override them; production code must only read them.
var (
	// DefaultLines is the default limit of the read tool and at the same
	// time the hard per-call line cap: a request for more lines than this
	// still stops here (reason ReasonLineLimit).
	DefaultLines = 2000
	// MaxLineChars is the per-line character budget. Longer lines are cut
	// to MaxLineChars runes and suffixed with TruncationMarker.
	MaxLineChars = 2000
	// MaxTotalBytes is the per-call output byte budget. Rendering stops at
	// line granularity: a line that would push the total past the budget
	// is dropped whole, never emitted half-cut.
	MaxTotalBytes = 256 * 1024
)

// TruncationMarker is appended to every line cut at MaxLineChars.
const TruncationMarker = "... [truncated]"

// Reason strings carried in RenderResult.Reasons and the footer; "which
// limit fired" is part of the §5.1 footer contract.
const (
	// ReasonLineLimit: the hard DefaultLines cap was the binding line
	// count constraint (the request asked for even more lines).
	ReasonLineLimit = "line limit reached"
	// ReasonRequestedLimit: the caller's own limit was exhausted before
	// the end of the file.
	ReasonRequestedLimit = "requested limit reached"
	// ReasonLineLength: at least one emitted line was cut at MaxLineChars.
	ReasonLineLength = "line length limit reached"
	// ReasonOffsetPastEnd: the requested offset lies beyond the last line.
	ReasonOffsetPastEnd = "offset beyond end of file"
)

// sizeLimitReason describes the total-bytes limit. It is derived from
// MaxTotalBytes so overridden budgets stay truthful; at the production
// value it reads "256 KiB size limit reached".
func sizeLimitReason() string {
	return fmt.Sprintf("%d KiB size limit reached", MaxTotalBytes/1024)
}

// RenderResult is the outcome of rendering one read call.
type RenderResult struct {
	// Text is the rendered payload: lines joined by \n, each line of the
	// form "<line number>\t<content>". Empty when no lines were emitted.
	Text string
	// FirstLine and LastLine are the 1-based absolute line numbers of the
	// first and last line present in Text; both are 0 when Text is empty.
	FirstLine int
	LastLine  int
	// TotalLines is the line count of the whole content.
	TotalLines int
	// Truncated is true iff at least one emitted line was cut at
	// MaxLineChars.
	Truncated bool
	// Reasons lists which limits fired, in detection order, deduplicated.
	Reasons []string
	// Footer is the §5.1 continuation notice: empty iff the render reached
	// the end of the file without any per-line truncation.
	Footer string
}

// ValidateArgs checks read's offset/limit (§5.1): offset is the 1-based
// first line and limit is a line count, so both must be >= 1. This is plain
// input validation, not a §6 catalog error, so a simple fmt.Errorf is
// deliberate here.
func ValidateArgs(offset, limit int) error {
	if offset < 1 {
		return fmt.Errorf("offset must be >= 1, got %d", offset)
	}
	if limit < 1 {
		return fmt.Errorf("limit must be >= 1, got %d", limit)
	}
	return nil
}

// Render renders content per §5.1. Lines are split on \n (a trailing \n
// ends the last line instead of opening an empty one; \r from CRLF stays
// inside the line content byte-for-byte) and numbered with 1-based absolute
// line numbers as "<line>\t<content>", starting at line offset and covering
// at most limit lines. The three limits — limit lines (capped at
// DefaultLines), MaxLineChars per line, MaxTotalBytes per call — stop
// rendering at line granularity, in that order of detection. Invalid UTF-8
// is replaced with U+FFFD before anything is counted, truncated, or billed
// against the byte budget.
func Render(content []byte, offset, limit int) (result RenderResult, err error) {
	if err := ValidateArgs(offset, limit); err != nil {
		return RenderResult{}, err
	}

	lines := splitLines(content)
	result.TotalLines = len(lines)

	// The requested range lies entirely past the last line: no text, but
	// still a footer (unless the file has no lines at all, which is a
	// complete read of an empty file) so the caller learns the real length
	// and a usable offset.
	if offset > result.TotalLines {
		if result.TotalLines > 0 {
			result.Reasons = []string{ReasonOffsetPastEnd}
			result.Footer = buildFooter(result)
		}
		return result, nil
	}

	effective, capped := limit, false
	if limit > DefaultLines {
		effective, capped = DefaultLines, true
	}

	var (
		out      []string
		used     int
		reasons  []string
		sizeStop bool
	)
	end := offset - 1 + effective
	if end > len(lines) {
		end = len(lines)
	}
	for i := offset - 1; i < end; i++ {
		display := sanitize(lines[i])
		if utf8.RuneCountInString(display) > MaxLineChars {
			display = truncateRunes(display, MaxLineChars) + TruncationMarker
			result.Truncated = true
			reasons = appendReason(reasons, ReasonLineLength)
		}
		rendered := strconv.Itoa(i+1) + "\t" + display
		if used+len(rendered)+1 > MaxTotalBytes { // +1 for the joining \n
			sizeStop = true
			reasons = appendReason(reasons, sizeLimitReason())
			break
		}
		used += len(rendered) + 1
		out = append(out, rendered)
	}

	if len(out) > 0 {
		result.FirstLine = offset
		result.LastLine = offset + len(out) - 1
	}

	// The line count ran out before EOF. If the caller asked for more
	// than DefaultLines, the hard cap fired rather than the request.
	if !sizeStop && len(out) == effective && result.LastLine < result.TotalLines {
		reason := ReasonRequestedLimit
		if capped {
			reason = ReasonLineLimit
		}
		reasons = appendReason(reasons, reason)
	}

	result.Text = strings.Join(out, "\n")
	result.Reasons = reasons
	result.Footer = buildFooter(result)
	return result, nil
}

// buildFooter renders the §5.1 continuation footer, or "" when the render
// reached EOF with no per-line truncation.
func buildFooter(r RenderResult) string {
	if r.LastLine >= r.TotalLines && !r.Truncated {
		return ""
	}
	return fmt.Sprintf("lines %d-%d of %d; %s; continue with offset=%d",
		r.FirstLine, r.LastLine, r.TotalLines,
		strings.Join(r.Reasons, "; "), r.LastLine+1)
}

// splitLines splits content on '\n'. A trailing newline terminates the last
// line instead of opening a new empty one; "\r" from CRLF stays inside the
// line content, byte-exact (§5.1). Empty content is zero lines.
func splitLines(content []byte) [][]byte {
	if len(content) == 0 {
		return nil
	}
	if content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}
	return bytes.Split(content, []byte{'\n'})
}

// sanitize returns the display form of a raw line: each run of invalid
// UTF-8 bytes collapses to one U+FFFD (strings.ToValidUTF8 semantics,
// §5.1). Truncation and byte accounting operate on this display form.
func sanitize(line []byte) string {
	return strings.ToValidUTF8(string(line), "\uFFFD")
}

// truncateRunes returns the first n runes of s; callers guarantee s has
// more than n runes.
func truncateRunes(s string, n int) string {
	kept := 0
	for i := range s {
		if kept == n {
			return s[:i]
		}
		kept++
	}
	return s
}

// appendReason appends r unless already present, keeping detection order.
func appendReason(reasons []string, r string) []string {
	for _, have := range reasons {
		if have == r {
			return reasons
		}
	}
	return append(reasons, r)
}
