package tools

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RecoverToolPanic catches any panic in a tool handler and converts it into a
// safe MCP error result. Without this, a single nil-dereference or bounds
// violation crashes the entire MCP server process.
//
// Usage (at top of every tool handler closure):
//
//	var panicResult *mcp.CallToolResult
//	defer RecoverToolPanic(&panicResult)
//	if panicResult != nil { return panicResult, nil, nil }
//
// Because defer runs after the return statement is evaluated, callers must
// check panicResult at the top of the function body after the defer line.
// The idiomatic pattern is a named return:
//
//	func handler(...) (result *mcp.CallToolResult, _ any, _ error) {
//	    defer RecoverToolPanic(&result)
//	    ...
//	}
func RecoverToolPanic(result **mcp.CallToolResult) {
	if r := recover(); r != nil {
		log.Printf("tool handler panic: %v\n%s", r, debug.Stack())
		*result = &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Internal error: the tool handler encountered an unexpected problem. See server logs."},
			},
			IsError: true,
		}
	}
}

// identifierPattern matches safe SQL identifiers (database and measurement names).
// Arc identifiers are ASCII alphanumeric plus underscore, starting with a letter
// or underscore. Anything else is rejected to prevent injection via interpolation.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ValidateIdentifier returns an error if s is not a safe SQL identifier.
// The returned error never echoes s — callers should log the offending value
// to stderr separately for operator debugging.
func ValidateIdentifier(s string) error {
	if s == "" {
		return errors.New("identifier is empty")
	}
	if !identifierPattern.MatchString(s) {
		return errors.New("invalid identifier: only letters, digits, and underscores are allowed")
	}
	return nil
}

// Words Arc's query validator refuses wherever they appear as code, even
// as a column name or alias, and the two-word shapes it refuses (one of
// the second words directly after the first). INSERT [OR x] INTO,
// UPDATE x SET and CREATE/DROP ... SECRET are handled in refusedPhrases.
// This is a subset of Arc's checks, kept so the model learns why and what
// to change; Arc stays the authority and refuses more (file functions,
// backticks, Unicode escapes), and its message is passed through when it
// does. When Arc's list changes, change this one.
var (
	refusedWords = map[string]bool{
		"attach": true, "detach": true, "copy": true, "pragma": true, "set": true, "reset": true,
		"load": true, "install": true, "call": true, "prepare": true, "execute": true,
	}
	refusedPairs = map[string][]string{
		"drop":     {"table", "database", "index", "view"},
		"create":   {"table", "database", "index"},
		"alter":    {"table"},
		"truncate": {"table"},
		"delete":   {"from"},
		"merge":    {"into"},
		"export":   {"database"},
		"import":   {"database"},
	}
)

// refusedPhrases returns each word or phrase in toks that Arc refuses, in
// order, without repeats (for example "DELETE FROM", "LOAD").
func refusedPhrases(toks []token) []string {
	var found []string
	add := func(p string) {
		if !slices.Contains(found, p) {
			found = append(found, p)
		}
	}
	word := func(k int) string {
		if k < len(toks) && toks[k].kind == wordToken {
			return toks[k].text
		}
		return ""
	}
	for k, t := range toks {
		if t.kind != wordToken || gluedToLiteral(toks, k) {
			continue
		}
		w := t.text
		switch {
		case refusedWords[w]:
			add(strings.ToUpper(w))
		case asciiParts(w) != nil:
			// Arc matches with ASCII word boundaries, so in `set$x` or
			// `éset` it still finds SET.
			for _, part := range asciiParts(w) {
				if refusedWords[part] {
					add(strings.ToUpper(part))
				}
			}
		case slices.Contains(refusedPairs[w], word(k+1)):
			add(strings.ToUpper(w + " " + word(k+1)))
		case w == "insert" && (word(k+1) == "into" || word(k+1) == "or" && word(k+3) == "into"):
			add("INSERT INTO")
		case w == "update" && word(k+2) == "set":
			add("UPDATE ... SET")
		case w == "create" || w == "drop":
			if slices.ContainsFunc(toks[k+1:], func(t token) bool { return t.is("secret") }) {
				add(strings.ToUpper(w) + " ... SECRET")
			}
		}
	}
	return found
}

// asciiParts splits a word that contains $ or non-ASCII letters into its
// ASCII [a-z0-9_] runs; nil for a plain ASCII word.
func asciiParts(w string) []string {
	notASCIIWord := func(r rune) bool {
		return r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}
	if !strings.ContainsFunc(w, notASCIIWord) {
		return nil
	}
	return strings.FieldsFunc(w, notASCIIWord)
}

// gluedToLiteral reports whether the word at k touches a literal with no
// space between. Arc checks words after replacing literals with
// placeholder words, so there the two read as one word and the check
// does not fire; it must not fire here either.
func gluedToLiteral(toks []token, k int) bool {
	return k > 0 && toks[k-1].kind == literalToken && toks[k-1].end == toks[k].pos ||
		k+1 < len(toks) && toks[k+1].kind == literalToken && toks[k].end == toks[k+1].pos
}

// ReadQuery is a query that passed ValidateReadOnly, ready to send to Arc.
type ReadQuery struct {
	l lexed
}

// readStatementList is how refusals name the statements that run.
const readStatementList = "SELECT, FROM, VALUES, WITH, PIVOT, UNPIVOT, SHOW, DESCRIBE, SUMMARIZE, EXPLAIN"

// ValidateReadOnly checks that sql is one read-only statement and, when it
// is not, says what to change. The checks are a subset of Arc's own (Arc's
// server-side validation and a read-only token are the security boundary):
// arc-mcp's job is to answer early, with a reason the model can act on.
func ValidateReadOnly(sql string) (*ReadQuery, error) {
	if strings.TrimSpace(sql) == "" {
		return nil, errors.New("empty SQL query")
	}
	l, err := lex(sql)
	if err != nil {
		return nil, fmt.Errorf("query rejected: %w", err)
	}

	if !readsOnly(l.toks) {
		first, ok := firstToken(l.toks)
		switch {
		case !ok || first.is(";"):
			return nil, errors.New("query rejected: there is no statement to run")
		case first.is("with") || first.is("explain"):
			return nil, fmt.Errorf("query rejected: the statement after %s is not a read; arc-mcp only runs read-only queries", strings.ToUpper(first.text))
		case first.kind != wordToken:
			return nil, fmt.Errorf("query rejected: a query must start with a statement keyword; arc-mcp only runs read-only queries (%s)", readStatementList)
		default:
			return nil, fmt.Errorf("query rejected: arc-mcp only runs read-only queries (%s); this one starts with %s", readStatementList, strings.ToUpper(first.text))
		}
	}

	if phrases := refusedPhrases(l.toks); len(phrases) > 0 {
		quoted := make([]string, len(phrases))
		for i, p := range phrases {
			quoted[i] = `"` + strings.ToLower(strings.Fields(p)[0]) + `"`
		}
		return nil, fmt.Errorf("query rejected: Arc refuses %s in a query, even when the word is a column name or alias. "+
			"If it is a column or alias here, double-quote it: %s",
			strings.Join(phrases, ", "), strings.Join(quoted, ", "))
	}

	return &ReadQuery{l: l}, nil
}

// Row-limit bounds. Arc is asked for maxRows+1 rows; 100,000 rows is far
// beyond what a model can read.
const (
	MinRowLimit = 1
	MaxRowLimit = 100_000
)

// ClampRowLimit keeps a configured row limit inside [MinRowLimit, MaxRowLimit].
func ClampRowLimit(n int) int {
	return max(MinRowLimit, min(n, MaxRowLimit))
}

// Selectable reports whether the statement returns rows the limit applies
// to (SELECT, FROM, VALUES, WITH, PIVOT, UNPIVOT). SHOW, DESCRIBE,
// SUMMARIZE and EXPLAIN are sent unchanged.
func (q *ReadQuery) Selectable() bool {
	first, ok := firstToken(q.l.toks)
	return ok && first.kind == wordToken && selectStatements[first.text]
}

// WithRowLimit returns the SQL to send to Arc so that at most maxRows+1
// rows come back; one row more than will be shown is how the caller learns
// the result was cut. maxRows must already be clamped.
//
// The statement's own top-level LIMIT is what is changed, so the WHERE
// clause stays exactly where Arc's time-range pruning looks for it:
//   - no top-level LIMIT: append "\nLIMIT maxRows+1" (the newline ends any
//     trailing -- comment; trailing semicolons are dropped first). A LIMIT
//     inside a CTE or subquery is not the statement's and is left alone,
//     and neither is a column named limit (`AS limit`, `t.limit`).
//   - LIMIT n with n <= maxRows: unchanged.
//   - LIMIT n with n > maxRows: n becomes maxRows+1, OFFSET untouched.
//   - anything else (LIMIT expression, LIMIT n%, FETCH), and a statement
//     that is one parenthesised query, where DuckDB rejects a LIMIT after
//     the closing parenthesis: wrap the whole query as a subquery. Correct,
//     but Arc cannot prune by time through a subquery (it cannot for a
//     parenthesised statement either), so it is kept for these forms only.
//
// Statements that are not Selectable are returned unchanged; the caller
// still cuts their rows.
func (q *ReadQuery) WithRowLimit(maxRows int) string {
	sql, toks := q.l.sql, q.l.toks
	if !q.Selectable() {
		return sql
	}
	if toks[0].is("(") {
		if end := groupEnd(toks, 0); end == len(toks) || toks[end].is(";") {
			return q.wrapped(maxRows)
		}
	}

	depth, limitAt := 0, -1
	for k, t := range toks {
		switch {
		case t.is("("):
			depth++
		case t.is(")"):
			depth--
		case depth == 0 && (t.is("limit") || t.is("fetch")):
			if k == 0 || !toks[k-1].is("as") && !toks[k-1].is(".") {
				limitAt = k
			}
		}
	}

	if limitAt < 0 {
		return fmt.Sprintf("%s\nLIMIT %d", q.withoutSemicolons(), maxRows+1)
	}
	if toks[limitAt].is("limit") && limitAt+1 < len(toks) {
		num := toks[limitAt+1]
		next := limitAt + 2
		plainEnd := next == len(toks) || toks[next].is("offset") || toks[next].is(";")
		if num.kind == numberToken && isDigits(num.text) && plainEnd {
			// A number too large for int64 is larger than maxRows too.
			if n, err := strconv.ParseInt(num.text, 10, 64); err == nil && n <= int64(maxRows) {
				return sql
			}
			return sql[:num.pos] + strconv.Itoa(maxRows+1) + sql[num.end:]
		}
	}
	return q.wrapped(maxRows)
}

// wrapped is the query as a subquery under the row limit.
func (q *ReadQuery) wrapped(maxRows int) string {
	return fmt.Sprintf("SELECT * FROM (\n%s\n) AS arc_mcp_q LIMIT %d", q.withoutSemicolons(), maxRows+1)
}

// withoutSemicolons is the query with its trailing semicolons blanked and
// trailing whitespace trimmed (lex allows semicolons only at the end).
func (q *ReadQuery) withoutSemicolons() string {
	body := []byte(q.l.sql)
	for _, t := range q.l.toks {
		if t.is(";") {
			body[t.pos] = ' '
		}
	}
	return strings.TrimRight(string(body), " \t\r\n\f")
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// TruncateResponse caps the response string at approximately maxChars bytes,
// never splitting a UTF-8 rune. If truncated, appends a notice.
func TruncateResponse(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}

	// Walk runes to find the last rune boundary ≤ maxChars bytes.
	cut := 0
	for i := range s {
		if i > maxChars {
			break
		}
		cut = i
	}

	return s[:cut] + fmt.Sprintf("\n\n... [truncated — response exceeded %d characters]", maxChars)
}

// escapeMarkdownCell sanitizes a value for safe inclusion in a markdown table
// cell. Pipe characters would break the table structure; newlines would inject
// extra rows visible to the LLM. nil is rendered as NULL. Values longer than
// 512 runes are truncated with an ellipsis.
func escapeMarkdownCell(v any) string {
	var s string
	if v == nil {
		s = "NULL"
	} else {
		s = fmt.Sprintf("%v", v)
	}

	// Truncate very long cells (maxCellRunes, shared with the structured rows).
	runes := []rune(s)
	if len(runes) > maxCellRunes {
		s = string(runes[:maxCellRunes]) + "…"
	}

	// Escape characters that break markdown tables.
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	return s
}
