// Package editengine implements the pure in-memory engine behind the edit
// and multi_edit tools (ARCHITECTURE.md §5.3/§5.4): exact byte-level match
// and replace. Whitespace and indentation are significant; there is no
// trimming, no normalization, and no fuzzy fallback of any kind. All
// functions are pure: they never touch the file system and never modify
// their input. Error messages come from internal/errmsg, the single
// source of error wording (§6).
package editengine

import (
	"bytes"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
)

// Edit is a single replacement instruction for ApplyMany. Old and New are
// raw bytes compared exactly (whitespace included). ReplaceAll selects
// replace-everywhere semantics; otherwise Old must occur exactly once in
// the content the edit is applied to.
type Edit struct {
	Old        []byte
	New        []byte
	ReplaceAll bool
}

// maxAmbiguousLines is how many match line numbers the EAmbiguous error
// reports (§5.3: first 5 locations).
const maxAmbiguousLines = 5

// where describes the subject in error messages. The engine is a pure
// function over content and has no file path; higher layers may add path
// context on top.
const where = "the file"

// newline is the single-byte separator used for line numbering.
var newline = []byte{'\n'}

// ApplyOnce applies a single old→new replacement to content.
//
// Semantics (§5.3):
//   - Matching is byte-exact against the whole content, including all
//     whitespace and indentation; matches are non-overlapping and counted
//     the same way strings.Count / strings.ReplaceAll do.
//   - len(old) == 0 → EEmptyOld (checked before anything else).
//   - old equals new byte-for-byte → ENoop.
//   - No match → ENoMatch.
//   - !replaceAll and more than one match → EAmbiguous carrying the total
//     match count and the line numbers of the first maxAmbiguousLines
//     matches (1-based; line 1 is the start of content; lines are counted
//     by '\n').
//   - replaceAll replaces every non-overlapping occurrence in a single
//     pass; new containing old cannot loop (bytes.ReplaceAll semantics).
//
// On success it returns the new content (a fresh slice; content itself is
// never written to) and the number of matches that were replaced.
// On error the result is nil and matches is 0.
func ApplyOnce(content, old, new []byte, replaceAll bool) (result []byte, matches int, err error) {
	if len(old) == 0 {
		return nil, 0, errmsg.EEmptyOld()
	}
	if bytes.Equal(old, new) {
		return nil, 0, errmsg.ENoop()
	}

	matches, firstOffsets := countMatches(content, old)
	if matches == 0 {
		return nil, 0, errmsg.ENoMatch(where)
	}
	if !replaceAll && matches > 1 {
		return nil, 0, errmsg.EAmbiguous(matches, lineNumbers(content, firstOffsets), where)
	}

	if replaceAll {
		return bytes.ReplaceAll(content, old, new), matches, nil
	}
	return bytes.Replace(content, old, new, 1), matches, nil
}

// ApplyMany applies edits in array order onto progressively modified
// content (§5.4). The uniqueness/existence check of edit i is evaluated
// against the content as it stands when edit i is applied, so later edits
// may anchor on text produced by earlier ones.
//
// If any edit fails, ApplyMany stops and returns an EEditIndex-wrapped
// error identifying the failing edit by 1-based number plus the underlying
// cause; nothing is persisted (the function is pure, and the input content
// is never modified, so a failure leaves the caller's content untouched).
func ApplyMany(content []byte, edits []Edit) ([]byte, error) {
	cur := content
	for i, e := range edits {
		out, _, err := ApplyOnce(cur, e.Old, e.New, e.ReplaceAll)
		if err != nil {
			return nil, errmsg.EEditIndex(i+1, err)
		}
		cur = out
	}
	return cur, nil
}

// countMatches counts non-overlapping exact occurrences of old in content
// and records the byte offsets of the first maxAmbiguousLines of them.
func countMatches(content, old []byte) (count int, firstOffsets []int) {
	off := 0
	for off <= len(content)-len(old) {
		i := bytes.Index(content[off:], old)
		if i < 0 {
			return count, firstOffsets
		}
		i += off
		if len(firstOffsets) < maxAmbiguousLines {
			firstOffsets = append(firstOffsets, i)
		}
		count++
		off = i + len(old)
	}
	return count, firstOffsets
}

// lineNumbers converts byte offsets (ascending) into 1-based line numbers.
// The start of content is line 1; each '\n' starts a new line. '\r' is an
// ordinary byte and never affects numbering.
func lineNumbers(content []byte, offsets []int) []int {
	lines := make([]int, len(offsets))
	newlines := 0
	prev := 0
	for i, off := range offsets {
		newlines += bytes.Count(content[prev:off], newline)
		lines[i] = newlines + 1
		prev = off
	}
	return lines
}
