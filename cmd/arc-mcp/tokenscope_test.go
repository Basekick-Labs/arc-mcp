package main

import (
	"strings"
	"testing"
)

func TestTokenScopeLog(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []string
		warn        string // substring of the warning line; "" = no warning
	}{
		{"read only", []string{"read"}, ""},
		{"Arc default read,write", []string{"read", "write"}, "carries write;"},
		{"admin token", []string{"read", "write", "delete", "admin"}, "carries write, delete, admin;"},
		{"admin alone", []string{"admin"}, "carries admin;"},
		{"delete without write", []string{"read", "delete"}, "carries delete;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := tokenScopeLog(tc.permissions)
			if want := "Arc token permissions: " + strings.Join(tc.permissions, ", "); lines[0] != want {
				t.Fatalf("first line = %q, want %q", lines[0], want)
			}
			var warnings []string
			for _, l := range lines[1:] {
				if strings.HasPrefix(l, "WARNING:") {
					warnings = append(warnings, l)
				}
			}
			if tc.warn == "" {
				if len(warnings) != 0 {
					t.Fatalf("read-only token warned: %v", warnings)
				}
				return
			}
			// Exactly one warning, naming every write-class permission.
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.warn) {
				t.Fatalf("warnings = %v, want one containing %q", warnings, tc.warn)
			}
		})
	}
}

// An RBAC-only token (created with "permissions": []) reports no direct
// permissions. That is not "read-only": its roles decide, and verify does not
// report them, so the log must say so rather than print an empty list.
func TestTokenScopeLog_NoDirectPermissions(t *testing.T) {
	for _, perms := range [][]string{nil, {}} {
		lines := tokenScopeLog(perms)
		if len(lines) != 1 || !strings.Contains(lines[0], "RBAC roles") {
			t.Fatalf("tokenScopeLog(%v) = %v", perms, lines)
		}
	}
}
