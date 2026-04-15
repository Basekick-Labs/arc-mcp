package tools

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

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

// dangerousPatterns matches SQL statements that modify data.
// Arc server-side is the authoritative read-only guard; this layer is advisory
// and defense-in-depth for accidental LLM-generated writes.
var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bINSERT\s+INTO\b`),
	regexp.MustCompile(`(?i)\bUPDATE\s+\w+\s+SET\b`),
	regexp.MustCompile(`(?i)\bDELETE\s+FROM\b`),
	regexp.MustCompile(`(?i)\bDROP\s+(?:TABLE|DATABASE|INDEX|VIEW|SCHEMA)\b`),
	regexp.MustCompile(`(?i)\bALTER\s+(?:TABLE|DATABASE)\b`),
	regexp.MustCompile(`(?i)\bCREATE\s+(?:TABLE|DATABASE|INDEX|VIEW|SCHEMA)\b`),
	regexp.MustCompile(`(?i)\bTRUNCATE\s+TABLE\b`),
	regexp.MustCompile(`(?i)\bATTACH\b`),
	regexp.MustCompile(`(?i)\bDETACH\b`),
	regexp.MustCompile(`(?i)\bCOPY\b`),
	regexp.MustCompile(`(?i)\bEXPORT\b`),
	regexp.MustCompile(`(?i)\bIMPORT\b`),
	regexp.MustCompile(`(?i)\bLOAD\b`),
	regexp.MustCompile(`(?i)\bINSTALL\b`),
}

// normalizeForCheck strips comments and string/identifier literals so the
// dangerous-pattern regexes don't fire on keywords inside them, and rejects
// multi-statement queries (embedded semicolons). A trailing semicolon is OK.
func normalizeForCheck(sql string) (string, error) {
	var b strings.Builder
	b.Grow(len(sql))

	runes := []rune(sql)
	i := 0
	for i < len(runes) {
		r := runes[i]

		// -- line comment
		if r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}

		// /* block comment */ (non-nested; nested block comments are extremely rare)
		if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			i += 2
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			if i+1 < len(runes) {
				i += 2
			} else {
				i = len(runes)
			}
			continue
		}

		// 'single-quoted' string (SQL doubles the quote to escape)
		if r == '\'' {
			i++
			for i < len(runes) {
				if runes[i] == '\'' {
					if i+1 < len(runes) && runes[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		// "double-quoted" identifier
		if r == '"' {
			i++
			for i < len(runes) {
				if runes[i] == '"' {
					if i+1 < len(runes) && runes[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		b.WriteRune(r)
		i++
	}

	cleaned := b.String()

	// Reject embedded semicolons (allow one trailing).
	stripped := strings.TrimRight(cleaned, " \t\r\n")
	stripped = strings.TrimSuffix(stripped, ";")
	if strings.ContainsRune(stripped, ';') {
		return "", errors.New("multi-statement queries are not allowed")
	}

	return cleaned, nil
}

// ValidateReadOnly checks that the SQL query is read-only.
// Returns a generic error when rejected; callers should log the underlying
// detail (matched pattern, SQL preview) to stderr for operator debugging.
func ValidateReadOnly(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return errors.New("empty SQL query")
	}

	cleaned, err := normalizeForCheck(trimmed)
	if err != nil {
		return fmt.Errorf("query rejected: %w", err)
	}

	for _, pattern := range dangerousPatterns {
		if pattern.MatchString(cleaned) {
			return errors.New("query rejected: only read-only SQL is allowed")
		}
	}

	return nil
}

// limitPattern captures an existing LIMIT clause and its integer argument.
var limitPattern = regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)\b`)

// EnforceRowLimit caps the query's row output at maxRows. If the query has no
// LIMIT, one is appended. If it has a LIMIT > maxRows, the existing clause is
// rewritten. Existing LIMITs ≤ maxRows are preserved unchanged.
func EnforceRowLimit(sql string, maxRows int) string {
	trimmed := strings.TrimSpace(sql)

	if m := limitPattern.FindStringSubmatchIndex(trimmed); m != nil {
		var n int
		fmt.Sscanf(trimmed[m[2]:m[3]], "%d", &n)
		if n > maxRows {
			return trimmed[:m[2]] + fmt.Sprintf("%d", maxRows) + trimmed[m[3]:]
		}
		return trimmed
	}

	trimmed = strings.TrimRight(trimmed, "; \t\n")
	return fmt.Sprintf("%s LIMIT %d", trimmed, maxRows)
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
