package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveToken(t *testing.T) {
	// ARC_TOKEN_FILE takes highest precedence.
	t.Run("file wins over env", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "token.txt")
		if err := os.WriteFile(f, []byte("file-token\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ARC_TOKEN_FILE", f)
		t.Setenv("ARC_TOKEN", "env-token")
		got, err := resolveToken("flag-token")
		if err != nil || got != "file-token" {
			t.Errorf("resolveToken = (%q, %v), want (\"file-token\", nil)", got, err)
		}
	})

	// ARC_TOKEN beats --arc-token flag.
	t.Run("env wins over flag", func(t *testing.T) {
		t.Setenv("ARC_TOKEN_FILE", "")
		t.Setenv("ARC_TOKEN", "env-token")
		got, err := resolveToken("flag-token")
		if err != nil || got != "env-token" {
			t.Errorf("resolveToken = (%q, %v), want (\"env-token\", nil)", got, err)
		}
	})

	// Falls back to the flag value when nothing else is set.
	t.Run("flag fallback", func(t *testing.T) {
		t.Setenv("ARC_TOKEN_FILE", "")
		t.Setenv("ARC_TOKEN", "")
		got, err := resolveToken("flag-token")
		if err != nil || got != "flag-token" {
			t.Errorf("resolveToken = (%q, %v), want (\"flag-token\", nil)", got, err)
		}
	})

	// Trailing newline is stripped from file.
	t.Run("file trailing whitespace stripped", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "token.txt")
		if err := os.WriteFile(f, []byte("  mytoken  \r\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ARC_TOKEN_FILE", f)
		t.Setenv("ARC_TOKEN", "")
		got, err := resolveToken("")
		if err != nil || got != "mytoken" {
			t.Errorf("resolveToken = (%q, %v), want (\"mytoken\", nil)", got, err)
		}
	})

	// Missing file returns an error.
	t.Run("missing file returns error", func(t *testing.T) {
		t.Setenv("ARC_TOKEN_FILE", "/nonexistent/path/token.txt")
		t.Setenv("ARC_TOKEN", "")
		_, err := resolveToken("")
		if err == nil {
			t.Error("resolveToken with missing file should return error")
		}
	})
}

func TestValidateArcURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		token    string
		insecure bool
		wantErr  bool
	}{
		{"https remote with token ok", "https://arc.example.com", "abc", false, false},
		{"http localhost with token ok", "http://localhost:8000", "abc", false, false},
		{"http 127.0.0.1 with token ok", "http://127.0.0.1:8000", "abc", false, false},
		{"http ::1 with token ok", "http://[::1]:8000", "abc", false, false},
		{"http remote with token rejected", "http://arc.example.com", "abc", false, true},
		{"http remote without token ok", "http://arc.example.com", "", false, false},
		{"http remote with token and insecure ok", "http://arc.example.com", "abc", true, false},
		{"empty url rejected", "", "abc", false, true},
		{"bad scheme rejected", "ftp://arc.example.com", "abc", false, true},
		{"missing host rejected", "https://", "abc", false, true},
		// A query string or userinfo in the URL is sent on every request,
		// and can carry credentials: the same rule as a token.
		{"http remote with query and no token rejected", "http://arc.example.com/?p=abc", "", false, true},
		{"http remote with userinfo and no token rejected", "http://u:p@arc.example.com", "", false, true},
		{"http remote with query and token rejected", "http://arc.example.com/?k=v", "abc", false, true},
		{"http remote with query and insecure ok", "http://arc.example.com/?p=abc", "", true, false},
		{"http loopback with query ok", "http://127.0.0.1:8000/?p=abc", "", false, false},
		{"https remote with query and userinfo ok", "https://u:p@arc.example.com/?p=abc", "", false, false},
		{"http remote with empty query ok", "http://arc.example.com/?", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateArcURL(tt.url, tt.token, tt.insecure)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateArcURL(%q, token=%q, insecure=%v) err = %v, wantErr %v", tt.url, tt.token, tt.insecure, err, tt.wantErr)
			}
		})
	}
}

// The refusals name the host, never the query string or the userinfo.
func TestValidateArcURL_ErrorsOmitQueryAndUserinfo(t *testing.T) {
	for _, raw := range []string{
		"http://user-marker:pass-marker@arc.example.com/arc/?p=query-marker",
		"http://user-marker:pass-marker@arc.example.com/%zz?p=query-marker",
	} {
		err := validateArcURL(raw, "", false)
		if err == nil {
			t.Fatalf("%q: accepted", raw)
		}
		for _, s := range []string{"user-marker", "pass-marker", "query-marker"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%q: error carries %q: %v", raw, s, err)
			}
		}
		if !strings.Contains(err.Error(), "arc.example.com") {
			t.Errorf("%q: error does not name the host: %v", raw, err)
		}
	}
}
