// Tool descriptors (final descriptions from T1) and input/output parameter
// structs (§5) for all six tools.
//
// The handlers live in read.go/write.go/edit.go (read/write/edit/multi_edit)
// and glob.go/grep.go, all methods on *Conn (server.go): one Conn per MCP
// connection over the process-wide *Shared.

package tools

// Tool descriptions (English, final; every tool states absolute-paths-only
// and the --allow allowed roots; each is at most 5 sentences, ARCHITECTURE §5).

const toolDescRead = `Read a file and return lines prefixed with 1-based absolute line numbers in the form "<line>\t<content>". Absolute paths only; every path must stay within the allowed roots configured via --allow. Output is truncated at whichever limit hits first: 2000 lines per call (default limit), 2000 characters per line, or 256 KiB total, and a footer then states the returned line range, the total line count, which limit fired, and how to continue with offset. offset is the 1-based first line to read (default 1) and limit is the line count to read.`

const toolDescWrite = `Create a file or rewrite it wholesale with the given complete content; appending is not supported. Absolute paths only; every path must stay within the allowed roots configured via --allow. You must hold a current read of an existing target: read it, then write it — a file that has not been read, or that changed since your read, is rejected until re-read. Parent directories are never created, and the write is atomic (temp file + rename).`

const toolDescEdit = `Replace text in a file where old_string matches the file bytes exactly, including all whitespace and indentation; there is no trimming, indentation normalization, or fuzzy fallback. Absolute paths only; every path must stay within the allowed roots configured via --allow. With replace_all=false (the default), old_string must match exactly once: zero matches and multiple matches are both errors; set replace_all=true to replace every occurrence. You must hold a current read of the file: read it, then edit it — a file that has not been read, or that changed since your read, is rejected until re-read.`

const toolDescMultiEdit = `Apply an ordered list of edits to one file, each matched byte-for-byte against the content as modified by the preceding edits; there is no trimming or fuzzy fallback. Absolute paths only; every path must stay within the allowed roots configured via --allow. Each edit needs a unique old_string unless its replace_all is true, and if any edit fails nothing is written and the error names the failing edit index (1-based) and the reason. All edits are applied in memory and land on disk in a single atomic write. You must hold a current read of the file: read it, then edit it — a file that has not been read, or that changed since your read, is rejected until re-read.`

const toolDescGlob = `List files matching a doublestar glob pattern (** supported) as absolute paths sorted by modification time, newest first. Absolute paths only; the path search root must be within the allowed roots configured via --allow, and it defaults to all allowed roots. The pattern is matched against paths relative to the search root, so an absolute pattern matches nothing. At most 200 results are returned, truncated entries are reported as "showing X of Y", and the directory scan stops once 100,000 entries have been traversed, after which results are declared possibly incomplete.`

const toolDescGrep = `Search file contents with a regular expression and report matches via output_mode: files_with_matches (default), content, or count. Absolute paths only; the path search root must be within the allowed roots configured via --allow, and it defaults to all allowed roots. glob is a single filename filter ("!pattern" excludes). A search is killed after a 120 second timeout and returns the partial results with a timeout notice. Narrow the scope with path and glob before searching — scanning an entire allowed root can be slow on a large share.`

// Input/output parameter structs per ARCHITECTURE §5. JSON field names are
// snake_case. Parameters that §5 declares optional or defaulted carry
// `,omitempty` so the SDK-inferred schema does not mark them required —
// that is schema shape, not validation. Value validation (ranges, enums,
// required semantics) lives in the handlers.

// ReadInput is the input of the read tool (§5.1).
type ReadInput struct {
	FilePath string `json:"file_path"`
	// Offset is the 1-based first line to read; default 1.
	Offset int `json:"offset,omitempty"`
	// Limit is the number of lines to read; default 2000.
	Limit int `json:"limit,omitempty"`
}

// ReadOutput is the output of the read tool: numbered lines plus the
// truncation footer (present whenever the read did not reach EOF).
type ReadOutput struct {
	Content string `json:"content"`
	Footer  string `json:"footer,omitempty"`
}

// WriteInput is the input of the write tool (§5.2).
type WriteInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// EditInput is the input of the edit tool (§5.3).
type EditInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// MultiEditInput is the input of the multi_edit tool (§5.4).
type MultiEditInput struct {
	FilePath string     `json:"file_path"`
	Edits    []EditItem `json:"edits"`
}

// EditItem is one entry of multi_edit's edits array.
type EditItem struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// GlobInput is the input of the glob tool (§5.5).
type GlobInput struct {
	// Pattern is a doublestar pattern matched against paths relative to Path.
	Pattern string `json:"pattern"`
	// Path is the search root; empty means all allowed roots.
	Path string `json:"path,omitempty"`
}

// GlobOutput is the output of the glob tool: absolute paths, newest first,
// plus the "showing X of Y" / possibly-incomplete notice when applicable.
type GlobOutput struct {
	Files []string `json:"files"`
	Note  string   `json:"note,omitempty"`
}

// GrepInput is the input of the grep tool (§5.6).
type GrepInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	// Glob is a filename filter passed to rg as a single --glob flag (and
	// applied with the same semantics in the fallback engine). rg-style
	// "!glob" excludes.
	Glob            string `json:"glob,omitempty"`
	OutputMode      string `json:"output_mode,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	Context         int    `json:"context,omitempty"`
	HeadLimit       int    `json:"head_limit,omitempty"`
}

// GrepOutput is the output of the grep tool: rg-shaped result lines
// (path, path:line:text, or path:count depending on output_mode) plus
// notices (timeout, head_limit truncation, fallback engine) joined by "; ".
type GrepOutput struct {
	Results []string `json:"results"`
	Note    string   `json:"note,omitempty"`
}
