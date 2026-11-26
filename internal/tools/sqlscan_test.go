package tools

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The reader must see a query the way DuckDB does, or the read-only check
// and the row limit judge a different statement from the one Arc runs.
func TestLex(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr error
	}{
		{"plain", "SELECT 1", nil},
		{"trailing semicolon", "SELECT 1;", nil},
		{"several trailing semicolons (Arc allows)", "SELECT 1;;  ;", nil},
		{"semicolon then comment", "SELECT 1; -- done", nil},
		{"semicolon inside string", "SELECT ';' AS s", nil},
		{"semicolon inside dollar string", "SELECT $$ ; $$ AS s", nil},
		{"semicolon inside tagged dollar string", "SELECT $q$ ; DROP $q$ AS s", nil},
		{"positional parameters are not quotes", "SELECT $1, $2", nil},
		{"dollar inside a name is not a quote", "SELECT a$$b FROM t", nil},
		{"second statement", "SELECT 1; SELECT 2", errMultipleStatements},
		{"line comment ends at CR", "SELECT 1 --x\r; DELETE FROM t", errMultipleStatements},
		{"line comment ends at LF", "SELECT 1 --x\n; DELETE FROM t", errMultipleStatements},
		{"E string backslash escape", `SELECT E'\'' ; DELETE FROM t --'`, errMultipleStatements},
		{"lower-case e string", `SELECT e'it\'s' AS s`, nil},
		{"e ending a name opens a plain string", `SELECT name'x' FROM t`, nil},
		{"doubled quote in string", "SELECT 'it''s; fine' AS s", nil},
		{"doubled quote in identifier", `SELECT "a""b;" FROM t`, nil},
		{"nested block comment", "SELECT 1 /* a /* b */ c */", errNestedComment},
		{"unterminated block comment", "SELECT 1 /* open", errUnterminatedComment},
		{"unterminated string", "SELECT 'open", errUnterminatedString},
		{"unterminated identifier", `SELECT "open`, errUnterminatedString},
		{"unterminated dollar string", "SELECT $$ open", errUnterminatedString},
		{"unbalanced open", "SELECT (1", errUnbalancedParens},
		{"unbalanced close", "SELECT 1)", errUnbalancedParens},
		{"close before open", "SELECT 1) AS a, (2", errUnbalancedParens},
		{"paren inside string is not code", "SELECT ')' AS s", nil},
		{"NUL byte", "SELECT 1\x00", errNULByte},
		{"too long", "SELECT " + strings.Repeat("1+", maxSQLBytes/2) + "1", errTooLong},

		// A no-break space is a space to DuckDB, so a $$ or E' after it
		// opens a literal.
		{"NBSP before dollar quote", "SELECT\u00a0$$)$$ AS s", nil},
		{"NBSP before dollar quote, unclosed", "SELECT\u00a0$$ x", errUnterminatedString},
		{"NBSP before E string", "SELECT\u00a0e'\\'' AS s", nil},
		// Overlapping comment openers: the second /* shares the first's slash.
		{"overlapping comment openers", "SELECT 1 /*/*/ */ */", errNestedComment},
		// A number ends where its digits end; the letters after it start a
		// word, which may carry $ and so opens no dollar quote.
		{"number then word with dollars", "SELECT 1abc$$ FROM t", nil},
		{"number then word with dollars; semicolon is code", "SELECT 1abc$$ ; SELECT 2 --$$", errMultipleStatements},
		{"decimal then word with dollars", "SELECT 1.5abc$$ ; SELECT 2 --$$", errMultipleStatements},
		{"underscored number then word", "SELECT 1_000abc$$ ; SELECT 2 --$$", errMultipleStatements},
		// `1where'x'` is 1, the word where, then a plain string (no E prefix).
		{"number then word ending in e", "SELECT 1where'\\' ; SELECT '\\'", errMultipleStatements},
		{"invalid UTF-8", "SELECT 1 /*\xff*/", errInvalidUTF8},
		{"0x then a word is 0 and an alias", "SELECT 0xg$$ FROM t", nil},
		{"hex literal", "SELECT 0x1F", nil},
		{"form feed after semicolon", "SELECT 1;\f", nil},
		// A number directly followed by a dollar quote: DuckDB opens the
		// string (and then rejects the syntax); so does the reading here.
		{"number then dollar quote", "SELECT 1$a$ ' $a$ ) --'", errUnbalancedParens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lex(tt.sql)
			if !errors.Is(err, tt.wantErr) || (err == nil) != (tt.wantErr == nil) {
				t.Errorf("lex(%q) err = %v, want %v", tt.sql, err, tt.wantErr)
			}
		})
	}
}

// Folding keeps byte offsets and is what reaches Arc; words inside
// literals and comments are never tokens.
func TestLex_FoldsSpacesAndHidesLiterals(t *testing.T) {
	in := "\uFEFFSELECT\u00a0'load' AS \"copy\", $$set$$ -- pragma\n/* call */ FROM t"
	l, err := lex(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.sql) != len(in) {
		t.Fatalf("length changed: in %d, sql %d", len(in), len(l.sql))
	}
	if strings.ContainsAny(l.sql, "\u00a0\uFEFF") {
		t.Errorf("sql still holds Unicode spaces: %q", l.sql)
	}
	for _, tok := range l.toks {
		if tok.kind == wordToken && slices.Contains([]string{"load", "copy", "set", "pragma", "call"}, tok.text) {
			t.Errorf("word %q was read from a literal or comment", tok.text)
		}
	}
	if first, _ := firstToken(l.toks); !first.is("select") {
		t.Errorf("first token = %+v, want select", first)
	}
}

// A code point DuckDB keeps inside identifiers must not be folded, or the
// identifier sent is not the one written.
func TestLex_OnlyDuckDBSpacesAreFolded(t *testing.T) {
	in := "SELECT 1 AS a\u1680b, 2 AS c\u2028d"
	l, err := lex(in)
	if err != nil {
		t.Fatal(err)
	}
	if l.sql != in {
		t.Errorf("sent %q, want it unchanged", l.sql)
	}
}

// One forward pass: a long run of dollars stays cheap.
func TestLex_LinearOnDollarRuns(t *testing.T) {
	sql := "SELECT a" + strings.Repeat("$", 200_000)
	start := time.Now()
	if _, err := lex(sql); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("lex took %s", d)
	}
}

func TestValidateReadOnly_ArcRules(t *testing.T) {
	tests := []struct {
		name     string
		sql      string
		contains string // "" means the query must pass; otherwise part of the refusal
	}{
		// Telemetry names Arc accepts.
		{"alias export", "SELECT avg(v) AS export FROM grid", ""},
		{"alias import", "SELECT avg(v) AS import FROM grid", ""},
		{"column update", "SELECT update FROM t", ""},
		{"column drop_in", "SELECT drop_in, load1 FROM net", ""},
		{"quoted load", `SELECT avg(cpu) AS "load" FROM host`, ""},
		{"quoted delete", `SELECT "delete" FROM t`, ""},
		{"word glued to a literal", "SELECT 'a'set", ""},

		// One row per refused word or shape, each inside a read so only that
		// rule can refuse it.
		{"attach", "SELECT 1 AS attach", `"attach"`},
		{"detach", "SELECT 1 AS detach", `"detach"`},
		{"copy", "SELECT 1 AS copy", `"copy"`},
		{"pragma", "SELECT 1 AS pragma", `"pragma"`},
		{"set", "SELECT 1 AS set", `"set"`},
		{"reset", "SELECT 1 AS reset", `"reset"`},
		{"load", "SELECT avg(cpu) AS load FROM host", `double-quote it: "load"`},
		{"install", "SELECT 1 AS install", `"install"`},
		{"call", "SELECT 1 AS call", `"call"`},
		{"prepare", "SELECT 1 AS prepare", `"prepare"`},
		{"execute", "SELECT 1 AS execute", `"execute"`},
		{"drop table", "SELECT drop table FROM t", "DROP TABLE"},
		{"create index", "SELECT create index FROM t", "CREATE INDEX"},
		{"alter table", "SELECT alter table FROM t", "ALTER TABLE"},
		{"truncate table", "SELECT truncate table FROM t", "TRUNCATE TABLE"},
		{"delete from", "SELECT delete FROM t", "DELETE FROM in a query"},
		{"merge into", "SELECT x FROM t WHERE a MERGE INTO", "MERGE INTO"},
		{"export database", "SELECT export database FROM t", "EXPORT DATABASE"},
		{"import database", "SELECT import database FROM t", "IMPORT DATABASE"},
		{"insert into", "SELECT insert into FROM t", "INSERT INTO"},
		{"insert or replace into", "SELECT insert or replace into FROM t", "INSERT INTO"},
		{"update x set", "SELECT update t set FROM x", "UPDATE ... SET"},
		{"create secret", "SELECT create, secret FROM t", "CREATE ... SECRET"},
		{"data-modifying CTE", "WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x", "DELETE FROM"},
		{"two words, one message", "SELECT load, copy FROM t", `"load", "copy"`},
		{"refused word inside a dollar name", "SELECT 1 AS set$x", `"set"`},
		{"refused word after a non-ASCII letter", "SELECT 1 AS \u00e9load", `"load"`},

		// Reads in every form Arc allows.
		{"from-first", "FROM cpu SELECT host", ""},
		{"values", "VALUES (1, 'a')", ""},
		{"parenthesised union", "(SELECT 1) UNION ALL (SELECT 2)", ""},
		{"parenthesised pivot", "(PIVOT t ON c USING count(*))", ""},
		{"summarize", "SUMMARIZE cpu", ""},
		{"explain", "EXPLAIN SELECT 1", ""},
		{"explain analyze read", "EXPLAIN ANALYZE SELECT 1", ""},
		{"explain with options", "EXPLAIN (FORMAT json) SELECT 1", ""},
		{"cte with column list", "WITH r(n) AS (SELECT 1) SELECT n FROM r", ""},
		{"two ctes", "WITH a AS (SELECT 1), b AS (SELECT 2) SELECT * FROM a, b", ""},
		{"materialized cte", "WITH a AS MATERIALIZED (SELECT 1) SELECT * FROM a", ""},
		{"recursive cte", "WITH RECURSIVE r AS (SELECT 1 UNION ALL SELECT 1 FROM r) SELECT * FROM r LIMIT 3", ""},
		{"BOM before select", "\uFEFFSELECT 1", ""},
		{"zero-width space before select", "\u200BSELECT 1", ""},

		// Not reads.
		{"pragma statement", "PRAGMA version", "starts with PRAGMA"},
		{"set statement", "SET threads = 1", "starts with SET"},
		{"use statement", "USE other", "starts with USE"},
		{"cte into insert", "WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", "after WITH is not a read"},
		{"with after with (Arc refuses)", "WITH a AS (SELECT 1) WITH b AS (SELECT 2) SELECT 1", "after WITH is not a read"},
		{"explain analyze delete", "EXPLAIN ANALYZE DELETE FROM t", "after EXPLAIN is not a read"},
		{"export statement", "EXPORT DATABASE '/tmp/x'", "starts with EXPORT"},
		{"comment only", "-- nothing", "no statement to run"},
		{"literal first", "'x'", "must start with a statement keyword"},
		{"empty", "   ", "empty SQL query"},

		// Reader refusals carry their reason.
		{"nested comment", "SELECT 1 /* a /* b */ */", "nested block comments"},
		{"unbalanced", "SELECT (1", "parentheses"},
		{"cr comment hides statement", "SELECT 1 --x\r; DELETE FROM t", "multi-statement"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateReadOnly(tt.sql)
			switch {
			case tt.contains == "" && err != nil:
				t.Errorf("ValidateReadOnly(%q) = %v, want it to pass", tt.sql, err)
			case tt.contains != "" && err == nil:
				t.Errorf("ValidateReadOnly(%q) passed, want refusal containing %q", tt.sql, tt.contains)
			case tt.contains != "" && !strings.Contains(err.Error(), tt.contains):
				t.Errorf("ValidateReadOnly(%q) = %q, want it to contain %q", tt.sql, err, tt.contains)
			}
		})
	}
}

func TestWithRowLimit(t *testing.T) {
	const limit = 500
	tests := []struct {
		name string
		sql  string
		want string
	}{
		// Appending keeps the query a single-table SELECT, so Arc still
		// prunes partitions by the WHERE clause's time range.
		{"plain select", "SELECT * FROM cpu WHERE time > now() - INTERVAL 1 HOUR", "SELECT * FROM cpu WHERE time > now() - INTERVAL 1 HOUR\nLIMIT 501"},
		// The bug in #12: an appended LIMIT used to land inside this comment.
		{"trailing line comment", "SELECT * FROM cpu -- recent", "SELECT * FROM cpu -- recent\nLIMIT 501"},
		{"trailing block comment", "SELECT * FROM cpu /* x */", "SELECT * FROM cpu /* x */\nLIMIT 501"},
		{"trailing semicolons", "SELECT * FROM cpu;;", "SELECT * FROM cpu\nLIMIT 501"},
		{"semicolon then comment", "SELECT * FROM cpu; -- c", "SELECT * FROM cpu  -- c\nLIMIT 501"},
		{"semicolon in string kept", "SELECT ';' FROM cpu;", "SELECT ';' FROM cpu\nLIMIT 501"},
		{"order by and offset", "SELECT * FROM cpu ORDER BY time OFFSET 10", "SELECT * FROM cpu ORDER BY time OFFSET 10\nLIMIT 501"},
		// The other half of #12: a LIMIT inside a CTE or subquery is not the
		// statement's own.
		{"cte with inner limit", "WITH x AS (SELECT * FROM cpu LIMIT 5) SELECT * FROM big", "WITH x AS (SELECT * FROM cpu LIMIT 5) SELECT * FROM big\nLIMIT 501"},
		{"subquery with inner limit", "SELECT * FROM (SELECT * FROM cpu LIMIT 5) s", "SELECT * FROM (SELECT * FROM cpu LIMIT 5) s\nLIMIT 501"},
		{"from-first", "FROM cpu SELECT host", "FROM cpu SELECT host\nLIMIT 501"},
		{"values", "VALUES (1), (2)", "VALUES (1), (2)\nLIMIT 501"},
		{"parenthesised union", "(SELECT 1) UNION ALL (SELECT 2)", "(SELECT 1) UNION ALL (SELECT 2)\nLIMIT 501"},
		// The statement's own LIMIT.
		{"own limit under cap", "SELECT * FROM cpu ORDER BY time DESC LIMIT 7;", "SELECT * FROM cpu ORDER BY time DESC LIMIT 7;"},
		{"own limit at cap", "SELECT * FROM cpu LIMIT 500", "SELECT * FROM cpu LIMIT 500"},
		{"own limit over cap", "SELECT * FROM cpu LIMIT 100000", "SELECT * FROM cpu LIMIT 501"},
		{"own limit over cap, offset", "SELECT * FROM cpu LIMIT 100000 OFFSET 10 -- c", "SELECT * FROM cpu LIMIT 501 OFFSET 10 -- c"},
		{"own limit beyond uint64", "SELECT * FROM cpu LIMIT 99999999999999999999999", "SELECT * FROM cpu LIMIT 501"},
		{"lower-case limit", "select * from cpu limit 9000", "select * from cpu limit 501"},
		// Forms whose LIMIT cannot be rewritten in place are wrapped.
		{"limit percent", "SELECT * FROM cpu LIMIT 10%", "SELECT * FROM (\nSELECT * FROM cpu LIMIT 10%\n) AS arc_mcp_q LIMIT 501"},
		{"limit expression", "SELECT * FROM cpu LIMIT 2 * 300", "SELECT * FROM (\nSELECT * FROM cpu LIMIT 2 * 300\n) AS arc_mcp_q LIMIT 501"},
		{"limit all", "SELECT * FROM cpu LIMIT ALL;", "SELECT * FROM (\nSELECT * FROM cpu LIMIT ALL\n) AS arc_mcp_q LIMIT 501"},
		// Arc answers SHOW itself; the others cannot take a LIMIT.
		{"show passes through", "SHOW TABLES", "SHOW TABLES"},
		{"describe passes through", "DESCRIBE SELECT * FROM cpu", "DESCRIBE SELECT * FROM cpu"},
		{"summarize passes through", "SUMMARIZE cpu", "SUMMARIZE cpu"},
		{"explain passes through", "EXPLAIN SELECT 1", "EXPLAIN SELECT 1"},
		// What Arc receives is the folded code, with literals untouched.
		{"NBSP folded", "SELECT\u00a01", "SELECT  1\nLIMIT 501"},
		{"NBSP inside a literal kept", "SELECT * FROM t WHERE host = 'a\u00a0b'", "SELECT * FROM t WHERE host = 'a\u00a0b'\nLIMIT 501"},
		{"NBSP inside an identifier kept", "SELECT \"a\u202fb\" FROM t", "SELECT \"a\u202fb\" FROM t\nLIMIT 501"},
		// A column named limit is not the LIMIT clause; appending keeps
		// Arc's time pruning.
		{"alias named limit", "SELECT max(v) AS limit FROM cpu WHERE time >= '2026-01-01'", "SELECT max(v) AS limit FROM cpu WHERE time >= '2026-01-01'\nLIMIT 501"},
		{"qualified column named limit", "SELECT t.limit FROM t", "SELECT t.limit FROM t\nLIMIT 501"},
		// DuckDB rejects a LIMIT after a parenthesised statement.
		{"parenthesised statement with limit", "(SELECT * FROM t LIMIT 5)", "SELECT * FROM (\n(SELECT * FROM t LIMIT 5)\n) AS arc_mcp_q LIMIT 501"},
		{"parenthesised statement", "(SELECT * FROM t);", "SELECT * FROM (\n(SELECT * FROM t)\n) AS arc_mcp_q LIMIT 501"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := ValidateReadOnly(tt.sql)
			if err != nil {
				t.Fatalf("ValidateReadOnly(%q): %v", tt.sql, err)
			}
			if got := q.WithRowLimit(limit); got != tt.want {
				t.Errorf("WithRowLimit(%q)\n got %q\nwant %q", tt.sql, got, tt.want)
			}
		})
	}
}

func TestClampRowLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 1}, {-5, 1}, {1, 1}, {500, 500}, {1 << 62, MaxRowLimit}} {
		if got := ClampRowLimit(tc.in); got != tc.want {
			t.Errorf("ClampRowLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
