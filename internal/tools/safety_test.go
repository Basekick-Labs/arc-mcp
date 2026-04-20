package tools

import (
	"strings"
	"testing"
)

func TestValidateIdentifier(t *testing.T) {
	valid := []string{
		"cpu",
		"Cpu",
		"cpu_1",
		"_internal",
		"a",
		"measurement_with_underscores_123",
	}
	invalid := []string{
		"",
		"1cpu",                   // leading digit
		"cpu;DROP",               // semicolon
		"cpu DROP",               // space
		"cpu/*x*/",               // comment chars
		"../etc",                 // traversal-ish
		"cpu-1",                  // hyphen
		"\"cpu\"",                // quoted
		"cpu'",                   // quote
		"école",                  // non-ASCII
		"a b",                    // internal space
		strings.Repeat("a", 200), // too long
	}

	for _, s := range valid {
		if err := ValidateIdentifier(s); err != nil {
			t.Errorf("ValidateIdentifier(%q) returned error, want nil: %v", s, err)
		}
	}
	for _, s := range invalid {
		if err := ValidateIdentifier(s); err == nil {
			t.Errorf("ValidateIdentifier(%q) returned nil, want error", s)
		}
	}
}

func TestValidateReadOnly_Adversarial(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		{"keyword inside line comment", "SELECT 1 -- DROP TABLE x", false},
		{"keyword inside block comment", "SELECT 1 /* DROP TABLE x */ FROM cpu", false},
		{"keyword inside single-quoted string", "SELECT * FROM logs WHERE msg = 'DELETE FROM users'", false},
		{"keyword inside double-quoted identifier", `SELECT "DELETE" FROM cpu`, false},
		{"multi-statement with embedded semicolon", "SELECT 1; DROP TABLE cpu", true},
		{"trailing semicolon is ok", "SELECT 1 FROM cpu;", false},
		{"trailing semicolon with whitespace ok", "SELECT 1 FROM cpu ;  \n", false},
		{"multi-statement hidden by comment stripping", "SELECT 1/*;*/; DROP TABLE cpu", true},
		{"crlf line endings", "SELECT 1\r\nFROM cpu\r\n", false},
		{"single-quoted escape doubled quote", "SELECT 'it''s fine DELETE FROM x' FROM cpu", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateReadOnly(tt.sql)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateReadOnly(%q) err = %v, wantErr %v", tt.sql, err, tt.wantErr)
			}
		})
	}
}

func TestEnforceRowLimit_Caps(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		maxRows int
		want    string
	}{
		{"cap large existing limit", "SELECT * FROM cpu LIMIT 1000000", 500, "SELECT * FROM cpu LIMIT 500"},
		{"preserve small existing limit", "SELECT * FROM cpu LIMIT 10", 500, "SELECT * FROM cpu LIMIT 10"},
		{"preserve limit under cap with offset following", "SELECT * FROM cpu LIMIT 50 OFFSET 10", 500, "SELECT * FROM cpu LIMIT 50 OFFSET 10"},
		{"cap with offset", "SELECT * FROM cpu LIMIT 999999 OFFSET 10", 500, "SELECT * FROM cpu LIMIT 500 OFFSET 10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnforceRowLimit(tt.sql, tt.maxRows); got != tt.want {
				t.Errorf("EnforceRowLimit(%q, %d) = %q, want %q", tt.sql, tt.maxRows, got, tt.want)
			}
		})
	}
}

func TestValidateReadOnly(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		// Safe queries
		{"simple select", "SELECT * FROM cpu", false},
		{"select with where", "SELECT host, value FROM cpu WHERE time > '2024-01-01'", false},
		{"select count", "SELECT count(*) FROM cpu", false},
		{"select with join", "SELECT a.*, b.* FROM cpu a JOIN mem b ON a.host = b.host", false},
		{"show tables", "SHOW TABLES", false},
		{"show databases", "SHOW DATABASES", false},
		{"describe", "DESCRIBE cpu", false},
		{"select with CTE", "WITH recent AS (SELECT * FROM cpu ORDER BY time DESC LIMIT 10) SELECT * FROM recent", false},
		{"select with aggregation", "SELECT host, avg(value) FROM cpu GROUP BY host", false},

		// Dangerous queries
		{"insert", "INSERT INTO cpu VALUES (1, 2, 3)", true},
		{"update", "UPDATE cpu SET value = 0 WHERE host = 'a'", true},
		{"delete", "DELETE FROM cpu WHERE time < '2024-01-01'", true},
		{"drop table", "DROP TABLE cpu", true},
		{"drop database", "DROP DATABASE mydb", true},
		{"alter table", "ALTER TABLE cpu ADD COLUMN foo TEXT", true},
		{"create table", "CREATE TABLE evil (id INT)", true},
		{"truncate", "TRUNCATE TABLE cpu", true},
		{"attach", "ATTACH 'malicious.db' AS evil", true},
		{"copy", "COPY cpu TO '/tmp/data.csv'", true},
		{"export", "EXPORT DATABASE '/tmp/dump'", true},
		{"install", "INSTALL httpfs", true},
		{"load", "LOAD httpfs", true},

		// Case insensitivity
		{"INSERT uppercase", "INSERT INTO cpu VALUES (1)", true},
		{"insert lowercase", "insert into cpu values (1)", true},
		{"InSeRt mixed", "InSeRt InTo cpu VALUES (1)", true},

		// Edge cases
		{"empty", "", true},
		{"whitespace only", "   ", true},

		// False positive checks — these words in column names or values should NOT trigger
		{"select with 'deleted' column", "SELECT deleted FROM cpu", false},
		{"select with 'updated_at' column", "SELECT updated_at FROM cpu", false},
		{"where with 'dropped' value", "SELECT * FROM cpu WHERE status = 'dropped'", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateReadOnly(tt.sql)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateReadOnly(%q) error = %v, wantErr %v", tt.sql, err, tt.wantErr)
			}
		})
	}
}

func TestEnforceRowLimit(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		maxRows int
		want    string
	}{
		{
			"no limit adds limit",
			"SELECT * FROM cpu",
			500,
			"SELECT * FROM cpu LIMIT 500",
		},
		{
			"existing limit preserved",
			"SELECT * FROM cpu LIMIT 10",
			500,
			"SELECT * FROM cpu LIMIT 10",
		},
		{
			"strips trailing semicolon",
			"SELECT * FROM cpu;",
			500,
			"SELECT * FROM cpu LIMIT 500",
		},
		{
			"strips trailing whitespace and semicolon",
			"SELECT * FROM cpu ;  ",
			100,
			"SELECT * FROM cpu LIMIT 100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EnforceRowLimit(tt.sql, tt.maxRows)
			if got != tt.want {
				t.Errorf("EnforceRowLimit(%q, %d) = %q, want %q", tt.sql, tt.maxRows, got, tt.want)
			}
		})
	}
}

func TestTruncateResponse(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		maxChars  int
		wantLen   int
		truncated bool
	}{
		{"short string unchanged", "hello", 100, 5, false},
		{"exact length unchanged", "hello", 5, 5, false},
		{"long string truncated", "hello world this is a long string", 10, 10, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateResponse(tt.input, tt.maxChars)
			if tt.truncated {
				// Truncated notice is appended, so total length > maxChars,
				// but the data prefix (up to maxChars bytes) must match input.
				if got[:tt.maxChars] != tt.input[:tt.maxChars] {
					t.Errorf("truncated prefix mismatch")
				}
			} else {
				if got != tt.input {
					t.Errorf("TruncateResponse(%q, %d) = %q, want %q", tt.input, tt.maxChars, got, tt.input)
				}
			}
		})
	}
}

func TestEscapeMarkdownCell(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{"nil becomes NULL", nil, "NULL"},
		{"plain string unchanged", "hello", "hello"},
		{"pipe escaped", "a|b", `a\|b`},
		{"newline replaced with space", "a\nb", "a b"},
		{"carriage return replaced", "a\rb", "a b"},
		{"crlf replaced", "a\r\nb", "a b"},
		{"multiple pipes", "a|b|c", `a\|b\|c`},
		{"integer value", 42, "42"},
		{"float value", 3.14, "3.14"},
		{"long cell truncated", strings.Repeat("x", 600), strings.Repeat("x", 512) + "…"},
		{"pipe and newline combined", "a|b\nc", `a\|b c`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeMarkdownCell(tt.input)
			if got != tt.want {
				t.Errorf("escapeMarkdownCell(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
