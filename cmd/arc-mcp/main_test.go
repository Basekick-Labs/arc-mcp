package main

import (
	"os"
	"path/filepath"
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
