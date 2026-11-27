package arc

import (
	"encoding/json"
	"regexp"
	"strings"
)

// maxQueryErrorBodyBytes is how much of a failed query's response is read.
// Arc's error for a DuckDB failure carries the rewritten statement, with one
// storage path per partition read, after the part worth keeping; on a query
// over a wide time range that runs well past the 4 KiB logged, and a body cut
// short does not parse.
const maxQueryErrorBodyBytes = 256 << 10

// duckdbErrorClass matches the error classes a model can fix by changing its
// SQL: a misspelled column or function, a syntax mistake, a bad cast or
// comparison, an out-of-range value. Names as the DuckDB driver reports them.
// Anything else (IO, HTTP, internal, permission errors) can name storage and
// says nothing the model can act on, so it keeps the generic message.
var duckdbErrorClass = regexp.MustCompile(`^(?:Binder|Parser|Catalog|Conversion|Invalid Input|Out of Range|Not implemented|Syntax|Divide by Zero|Mismatch Type|Invalid type|Decimal) Error: `)

// arrowQueryFailed is the prefix Arc puts on a DuckDB error from the query
// path.
const arrowQueryFailed = "arrow query failed: "

// QueryErrorText returns the part of a DuckDB error a model can act on, with
// anything that names storage removed, or "" when the error is not one of
// the fixable classes.
func QueryErrorText(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, arrowQueryFailed)
	if !duckdbErrorClass.MatchString(s) {
		return ""
	}
	// What follows a blank line is DuckDB's LINE n: context: Arc's rewritten
	// statement, storage paths included.
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "LINE ") {
			break
		}
		lines = append(lines, l)
		if len(lines) == 5 {
			break
		}
	}
	s = redactPaths(strings.Join(lines, "\n"))
	if r := []rune(s); len(r) > 600 {
		s = string(r[:600]) + "…"
	}
	return strings.TrimSpace(s)
}

// queryErrorFromBody is QueryErrorText for Arc's JSON error body.
func queryErrorFromBody(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return QueryErrorText(parsed.Error)
}

// isOutOfMemory reports an error body whose DuckDB error is an out-of-memory
// failure, which the model can avoid by asking for less.
func isOutOfMemory(body []byte) bool {
	var parsed struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(body, &parsed) == nil &&
		strings.Contains(parsed.Error, "Out of Memory Error")
}

// Path-like text, found by content rather than by a list of prefixes:
// a quoted span holding a path separator, a URL scheme or a Parquet file
// name; any URL; any absolute or ./-relative path.
var (
	quotedPathLike  = regexp.MustCompile(`'[^']*(?:/|\\|://|\.parquet)[^']*'|"[^"]*(?:/|\\|://|\.parquet)[^"]*"`)
	urlLike         = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	absolutePath    = regexp.MustCompile(`(^|[\s(=,\[])(?:\.{0,2}/)[^\s'",)\]]+`)
	readParquetCall = regexp.MustCompile(`(?i)read_parquet\s*\([^)]*\)?`)
)

// redactPaths replaces anything in s that names a file or a storage location
// with <path>. It errs toward removing too much: a value that merely looks
// like a path is removed as well.
func redactPaths(s string) string {
	s = readParquetCall.ReplaceAllString(s, "read_parquet(<path>)")
	s = quotedPathLike.ReplaceAllString(s, "<path>")
	s = urlLike.ReplaceAllString(s, "<path>")
	s = absolutePath.ReplaceAllString(s, "${1}<path>")
	return s
}
