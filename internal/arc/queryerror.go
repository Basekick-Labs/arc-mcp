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
var duckdbErrorClass = regexp.MustCompile(`^(?:Binder|Parser|Catalog|Conversion|Invalid Input|Out of Range|Not implemented|Syntax|Divide by Zero|Mismatch Type|Invalid type|Decimal|Expression|Parameter Not Resolved|Parameter Not Allowed) Error: `)

// arrowQueryFailed is the prefix Arc puts on a DuckDB error from the query
// path.
const arrowQueryFailed = "arrow query failed: "

// streamFailurePrefix is how Arc introduces an error it hit after the first
// rows were sent (query_arrow_json.go, query_json_writer.go), in a result's
// truncation_reason.
var streamFailurePrefix = regexp.MustCompile(`^(?:arrow reader error after \d+ rows?|iterator error after \d+ rows?|scan failed at row \d+): `)

// storageFault reports a message that a fixable class carries for a problem
// in the stored files rather than in the statement: a damaged Parquet file
// (the prefixes Arc's corruptParquetMessage recognises) or files whose
// schemas disagree. No change to the SQL fixes those, and their advice
// (union_by_name, COPY) is not the model's to follow.
func storageFault(msg string) bool {
	switch {
	case strings.HasPrefix(msg, "Invalid Input Error: No magic bytes found at end of file "),
		strings.HasPrefix(msg, "Invalid Input Error: Footer length error in file "),
		strings.HasPrefix(msg, "Invalid Input Error: File ") && strings.Contains(msg, " too small to be a Parquet file"):
		return true
	}
	lower := strings.ToLower(msg)
	for _, s := range []string{`in file "`, "in file '", "schema mismatch", "union_by_name", "parquet file", "parquet schema"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// queryErrorText returns the part of a DuckDB error a model can act on, with
// anything that names storage removed, or "" when the error is not one of
// the fixable classes, is about the stored files, or says nothing.
func queryErrorText(raw string) string {
	s := strings.TrimSpace(raw)
	s = streamFailurePrefix.ReplaceAllString(s, "")
	s = strings.TrimPrefix(s, arrowQueryFailed)
	loc := duckdbErrorClass.FindStringIndex(s)
	if loc == nil || storageFault(s) {
		return ""
	}
	// What follows a blank line is DuckDB's LINE n: context: Arc's rewritten
	// statement, storage paths included.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "LINE ") {
			break
		}
		lines = append(lines, l)
		if len(lines) == 5 {
			break
		}
	}
	s = strings.Join(lines, "\n")
	if strings.TrimSpace(s[loc[1]:]) == "" {
		return "" // a class with no message
	}
	s = redactPaths(s)
	if r := []rune(s); len(r) > 600 {
		s = string(r[:600]) + "…"
	}
	return strings.TrimSpace(s)
}

// FixableTruncation returns, for a result Arc stopped partway through, the
// reason when it is a fixable query error (redacted, as for a failed query),
// or "" when it is not or there is none. Running the query again fails the
// same way for those, so the model should change it instead.
func (r *QueryResponse) FixableTruncation() string {
	if !r.Truncated {
		return ""
	}
	return queryErrorText(r.TruncationReason)
}

// queryErrorFromBody is queryErrorText for Arc's JSON error body.
func queryErrorFromBody(body []byte) string {
	return queryErrorText(errorField(body))
}

// isOutOfMemory reports an error body whose DuckDB error is an out-of-memory
// failure, which the model can avoid by asking for less.
func isOutOfMemory(body []byte) bool {
	return strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(errorField(body)), arrowQueryFailed), "Out of Memory Error: ")
}

// errorField is the "error" string of Arc's JSON error body, "" for any
// other body.
func errorField(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Error
}

var (
	quotedSpan      = regexp.MustCompile("'[^']*'|\"[^\"]*\"|`[^`]*`")
	readParquetCall = regexp.MustCompile(`(?i)read_parquet\s*\((?:\s*\[[^\]]*\]|[^)]*)\)?`)
	ipv4            = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?$`)
	hostPort        = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+:\d+$`)
	driveLetter     = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	leadingSep      = regexp.MustCompile(`^(?:\.{0,2}[/\\])[A-Za-z0-9_.-]`)
	allNumericParts = regexp.MustCompile(`^[0-9]+(?:[/\\-][0-9]+)+$`)
)

// pathLike reports whether one token, or the inside of one quoted span,
// names a file or a storage location. Judged by content, not by a list of
// prefixes. A lone separator (`/(VARCHAR, INTEGER)`, `usage/idle`) and an
// all-numeric date (`2026/11/26`) are kept: they are the model's own SQL or
// data, and it needs them to fix its query.
func pathLike(tok string) bool {
	t := strings.Trim(tok, "()[]{},;:'\"`")
	switch {
	case t == "":
		return false
	case allNumericParts.MatchString(t):
		return false
	case strings.Contains(t, "://"), strings.HasPrefix(strings.ToLower(t), "file:"),
		strings.HasPrefix(t, `\\`), strings.Contains(strings.ToLower(t), "%2f"),
		strings.HasSuffix(strings.ToLower(t), ".parquet"), driveLetter.MatchString(t),
		ipv4.MatchString(t), hostPort.MatchString(t):
		return true
	}
	seps := strings.Count(t, "/") + strings.Count(t, `\`)
	return seps >= 2 || seps >= 1 && (leadingSep.MatchString(t) || strings.Contains(t, "="))
}

// redactPaths replaces anything in s that names a file or a storage location
// with <path>: Arc's read_parquet(...) calls, quoted spans whose content is a
// path, and unquoted tokens that are. A quoted span is judged whole, so a
// path with spaces in it goes as one.
func redactPaths(s string) string {
	s = readParquetCall.ReplaceAllString(s, "read_parquet(<path>)")
	s = quotedSpan.ReplaceAllStringFunc(s, func(q string) string {
		inner := q[1 : len(q)-1]
		for _, part := range strings.Fields(inner) {
			if pathLike(part) {
				return "<path>"
			}
		}
		if pathLike(inner) {
			return "<path>"
		}
		return q
	})
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		words := strings.Split(line, " ")
		for j, w := range words {
			if w != "<path>" && !strings.Contains(w, "<path>") && pathLike(w) {
				words[j] = "<path>"
			}
		}
		b.WriteString(strings.Join(words, " "))
	}
	return b.String()
}
