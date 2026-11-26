package tools

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// This file reads a query into tokens following DuckDB's lexer: one
// forward pass, each token read whole by its own rules, never by looking
// back. The read-only check and the row limit both work from that one
// reading, so they agree with each other about where the statement's
// words, literals, parentheses and semicolons are. Where this reading and
// DuckDB's could still differ, the difference must only ever make arc-mcp
// refuse a query, never accept one DuckDB would run as more than one
// statement; sqlscan_test.go pins the cases found so far.

// maxSQLBytes bounds the work spent on one query. Arc's own default limit
// on query text is far smaller; this only keeps a pathological input cheap.
const maxSQLBytes = 1 << 20

// Refusals from reading the query. DuckDB or Arc refuse the same queries;
// naming the reason lets the model fix the query instead of guessing.
var (
	errTooLong             = errors.New("the query is longer than 1 MiB")
	errNULByte             = errors.New("the query contains a NUL byte")
	errInvalidUTF8         = errors.New("the query is not valid UTF-8")
	errUnterminatedString  = errors.New("a string literal or quoted identifier is not closed")
	errUnterminatedComment = errors.New("a /* comment is not closed")
	errNestedComment       = errors.New("nested block comments are not allowed")
	errUnbalancedParens    = errors.New("parentheses are not balanced")
	errMultipleStatements  = errors.New("multi-statement queries are not allowed; send one statement")
)

type tokenKind int

const (
	wordToken    tokenKind = iota // keyword or unquoted identifier, lower-cased
	numberToken                   // numeric literal, as written
	literalToken                  // '...', E'...', $tag$...$tag$ or "..."
	paramToken                    // $1, $name
	symbolToken                   // any other single byte
)

// token is one token of the query, with its byte span in lexed.sql.
type token struct {
	kind     tokenKind
	text     string // lower-cased word, number as written, the symbol; "" for a literal
	pos, end int
}

// lexed is a query read by DuckDB's rules.
type lexed struct {
	// sql is what arc-mcp sends to Arc: the query with the Unicode spaces
	// DuckDB treats as whitespace folded to ASCII spaces of the same byte
	// length, except inside literals, whose bytes are part of the value
	// and are kept. Arc and DuckDB therefore read exactly the code that
	// was checked here.
	sql  string
	toks []token
}

// duckdbSpace reports whether DuckDB treats r as whitespace. The list is
// DuckDB's (measured by sweeping every code point above 0x7F through
// `SELECT<cp>42`); any other non-ASCII code point is part of a word.
func duckdbSpace(r rune) bool {
	switch {
	case r == 0x00A0, r == 0x200B, r == 0x202F, r == 0x205F, r == 0x2060, r == 0x3000, r == 0xFEFF:
		return true
	case r >= 0x2000 && r <= 0x200A:
		return true
	}
	return false
}

// dollarOpener matches $$ or $tag$ at the start of the input. A tag is an
// identifier that does not start with a digit ($1 is a parameter).
var dollarOpener = regexp.MustCompile(`^\$(?:[A-Za-z_\x{80}-\x{10FFFF}][A-Za-z0-9_\x{80}-\x{10FFFF}]*)?\$`)

// lex reads sql into tokens. It refuses what DuckDB or Arc refuse at this
// level: invalid UTF-8, NUL bytes, unclosed literals and comments, nested
// comments, unbalanced parentheses and more than one statement (trailing
// semicolons are fine).
func lex(sql string) (lexed, error) {
	switch {
	case len(sql) > maxSQLBytes:
		return lexed{}, errTooLong
	case strings.IndexByte(sql, 0) >= 0:
		return lexed{}, errNULByte
	case !utf8.ValidString(sql):
		return lexed{}, errInvalidUTF8
	}

	sent := []byte(sql)
	var toks []token
	emit := func(kind tokenKind, text string, pos, end int) {
		toks = append(toks, token{kind: kind, text: text, pos: pos, end: end})
	}

	for i := 0; i < len(sql); {
		c := sql[i]
		rest := sql[i:]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++

		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(rest)
			if duckdbSpace(r) {
				for k := i; k < i+size; k++ {
					sent[k] = ' '
				}
				i += size
				continue
			}
			n := wordLen(rest)
			emit(wordToken, strings.ToLower(rest[:n]), i, i+n)
			i += n

		case strings.HasPrefix(rest, "--"):
			// DuckDB ends a line comment at \n or \r.
			n := strings.IndexAny(rest, "\r\n")
			if n < 0 {
				n = len(rest)
			}
			i += n

		case strings.HasPrefix(rest, "/*"):
			n, err := blockCommentLen(rest)
			if err != nil {
				return lexed{}, err
			}
			i += n

		case (c == 'e' || c == 'E') && len(rest) > 1 && rest[1] == '\'':
			// Reached only at the start of a token: a word's letters are
			// consumed by the word case below, so this E begins one.
			n, ok := quotedLen(rest[1:], '\'', true)
			if !ok {
				return lexed{}, errUnterminatedString
			}
			emit(literalToken, "", i, i+1+n)
			i += 1 + n

		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			n := wordLen(rest)
			emit(wordToken, strings.ToLower(rest[:n]), i, i+n)
			i += n

		case c >= '0' && c <= '9' || c == '.' && len(rest) > 1 && rest[1] >= '0' && rest[1] <= '9':
			n := numberLen(rest)
			emit(numberToken, rest[:n], i, i+n)
			i += n

		case c == '\'' || c == '"':
			n, ok := quotedLen(rest, c, false)
			if !ok {
				return lexed{}, errUnterminatedString
			}
			emit(literalToken, "", i, i+n)
			i += n

		case c == '$':
			if opener := dollarOpener.FindString(rest); opener != "" {
				body := strings.Index(rest[len(opener):], opener)
				if body < 0 {
					return lexed{}, errUnterminatedString
				}
				n := 2*len(opener) + body
				emit(literalToken, "", i, i+n)
				i += n
				continue
			}
			n := 1 + wordLen(rest[1:])
			emit(paramToken, rest[:n], i, i+n)
			i += n

		default:
			emit(symbolToken, string(c), i, i+1)
			i++
		}
	}

	// Literal bytes are sent as written.
	for _, t := range toks {
		if t.kind == literalToken {
			copy(sent[t.pos:t.end], sql[t.pos:t.end])
		}
	}

	depth, ended := 0, false
	for _, t := range toks {
		switch {
		case t.is(";"):
			ended = true
			continue
		case ended:
			return lexed{}, errMultipleStatements
		case t.is("("):
			depth++
		case t.is(")"):
			if depth--; depth < 0 {
				return lexed{}, errUnbalancedParens
			}
		}
	}
	if depth != 0 {
		return lexed{}, errUnbalancedParens
	}
	return lexed{sql: string(sent), toks: toks}, nil
}

// wordLen is the length of the identifier run at the start of s: letters,
// digits, _, $ and any non-ASCII code point DuckDB does not treat as a
// space.
func wordLen(s string) int {
	n := 0
	for n < len(s) {
		c := s[n]
		if c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			n++
			continue
		}
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[n:])
			if !duckdbSpace(r) {
				n += size
				continue
			}
		}
		break
	}
	return n
}

// numberLen is the length of the numeric literal at the start of s:
// 0x.. / 0b.. integers, or digits with an optional fraction and exponent
// (underscores allowed between digits). Whatever follows, even a letter, is
// the next token: DuckDB reads `1abc` as 1 followed by abc.
func numberLen(s string) int {
	digits := func(k int, ok func(byte) bool) int {
		for k < len(s) && (ok(s[k]) || s[k] == '_' && k > 0 && ok(s[k-1])) {
			k++
		}
		return k
	}
	dec := func(c byte) bool { return c >= '0' && c <= '9' }
	hex := func(c byte) bool { return dec(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
	bin := func(c byte) bool { return c == '0' || c == '1' }

	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') && hex(s[2]) {
		return digits(2, hex)
	}
	if len(s) > 2 && s[0] == '0' && (s[1] == 'b' || s[1] == 'B') && bin(s[2]) {
		return digits(2, bin)
	}
	k := digits(0, dec)
	if k < len(s) && s[k] == '.' {
		k = digits(k+1, dec)
	}
	if k < len(s) && (s[k] == 'e' || s[k] == 'E') {
		e := k + 1
		if e < len(s) && (s[e] == '+' || s[e] == '-') {
			e++
		}
		if e < len(s) && dec(s[e]) {
			k = digits(e, dec)
		}
	}
	return k
}

// blockCommentLen is the length of the /* comment at the start of s. It
// walks byte by byte, checking for an opener before a closer at every
// position, as DuckDB does. DuckDB nests comments and Arc refuses nesting,
// so a second opener before the close is refused here too.
func blockCommentLen(s string) (int, error) {
	for k := 2; k+1 < len(s); k++ {
		switch {
		case s[k] == '/' && s[k+1] == '*':
			return 0, errNestedComment
		case s[k] == '*' && s[k+1] == '/':
			return k + 2, nil
		}
	}
	return 0, errUnterminatedComment
}

// quotedLen is the length of the quoted literal at the start of s,
// including both quotes. A doubled quote is the only escape, plus a
// backslash before any byte when backslash is set (E'...' strings).
func quotedLen(s string, quote byte, backslash bool) (int, bool) {
	for k := 1; k < len(s); k++ {
		switch {
		case backslash && s[k] == '\\':
			k++
		case s[k] == quote && k+1 < len(s) && s[k+1] == quote:
			k++
		case s[k] == quote:
			return k + 1, true
		}
	}
	return 0, false
}

// is reports whether t is the symbol or word w.
func (t token) is(w string) bool {
	return (t.kind == symbolToken || t.kind == wordToken) && t.text == w
}

// Statement keywords. Arc's query API runs the same read statements, and
// WITH and EXPLAIN when what they lead into is one of them.
var (
	readStatements   = map[string]bool{"select": true, "from": true, "values": true, "pivot": true, "unpivot": true, "show": true, "describe": true, "summarize": true}
	selectStatements = map[string]bool{"select": true, "from": true, "values": true, "pivot": true, "unpivot": true, "with": true}
	// Words that may follow a CTE's closing parenthesis inside the WITH
	// clause: another CTE, the AS after a column list, USING KEY, and the
	// [NOT] MATERIALIZED hints.
	cteContinuations = map[string]bool{",": true, "as": true, "using": true, "key": true, "not": true, "materialized": true}
)

// firstToken returns the first token after any opening parentheses.
func firstToken(toks []token) (token, bool) {
	for _, t := range toks {
		if !t.is("(") {
			return t, true
		}
	}
	return token{}, false
}

// readsOnly reports whether toks are one read statement. It walks the
// prefixes a read may carry, left to right:
//   - opening parentheses;
//   - EXPLAIN [ANALYZE] [(options)], once and only first (EXPLAIN ANALYZE
//     runs the statement, so what follows must itself be a read);
//   - WITH and its CTEs, after which the main statement must open with a
//     parenthesis or a read keyword (not another WITH or an EXPLAIN).
//
// What remains must start with a read keyword.
func readsOnly(toks []token) bool {
	explainAllowed := true
	for i := 0; ; {
		for i < len(toks) && toks[i].is("(") {
			i++
		}
		if i >= len(toks) {
			return false
		}
		switch t := toks[i]; {
		case t.is("explain") && explainAllowed:
			explainAllowed = false
			i++
			if i < len(toks) && (toks[i].is("analyze") || toks[i].is("analyse")) {
				i++
			}
			if i < len(toks) && toks[i].is("(") {
				i = groupEnd(toks, i)
			}
		case t.is("with"):
			explainAllowed = false
			i = cteListEnd(toks, i+1)
			if i < 0 || !opensMainStatement(toks[i]) {
				return false
			}
		default:
			return t.kind == wordToken && readStatements[t.text]
		}
	}
}

// opensMainStatement reports whether t may start the statement after a
// WITH clause: an opening parenthesis or a read keyword.
func opensMainStatement(t token) bool {
	return t.is("(") || t.kind == wordToken && readStatements[t.text]
}

// cteListEnd returns the index of the token that starts the main statement
// after a WITH clause whose CTEs begin at i: the token after the first
// top-level closing parenthesis that is not followed by a CTE
// continuation. -1 when there is none.
func cteListEnd(toks []token, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		switch {
		case toks[i].is("("):
			depth++
		case toks[i].is(")"):
			depth--
			if depth == 0 && i+1 < len(toks) && !cteContinuations[toks[i+1].text] {
				return i + 1
			}
		}
	}
	return -1
}

// groupEnd returns the index after the parenthesised group opening at i
// (lex has already checked that parentheses balance).
func groupEnd(toks []token, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		switch {
		case toks[i].is("("):
			depth++
		case toks[i].is(")"):
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return len(toks)
}
