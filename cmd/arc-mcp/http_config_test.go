package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnvBool(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "FALSE": false, "1": true, "true": true, "True": true} {
		badEnv = nil
		t.Setenv("ARC_MCP_TEST_BOOL", v)
		if got := envBool("ARC_MCP_TEST_BOOL"); got != want || badEnv != nil {
			t.Errorf("%q: %v (badEnv %v), want %v", v, got, badEnv, want)
		}
	}
	for _, v := range []string{"yes", "on", "2", "ture"} {
		badEnv = nil
		t.Setenv("ARC_MCP_TEST_BOOL", v)
		if envBool("ARC_MCP_TEST_BOOL") || badEnv == nil {
			t.Errorf("%q: accepted", v)
		}
	}
	badEnv = nil
}

func TestEnvDuration(t *testing.T) {
	badEnv = nil
	t.Setenv("ARC_MCP_TEST_DUR", "2m")
	if d := envDuration("ARC_MCP_TEST_DUR", time.Second); d != 2*time.Minute || badEnv != nil {
		t.Errorf("2m: %v %v", d, badEnv)
	}
	t.Setenv("ARC_MCP_TEST_DUR", "30")
	if d := envDuration("ARC_MCP_TEST_DUR", time.Second); d != time.Second || badEnv == nil {
		t.Errorf("30 (no unit): %v %v", d, badEnv)
	}
	badEnv = nil
}

func TestResolveHTTPToken(t *testing.T) {
	dir := t.TempDir()
	file := func(content string) string {
		p := filepath.Join(dir, "tok")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("ARC_MCP_HTTP_TOKEN", "from-env")
	t.Setenv("ARC_MCP_HTTP_TOKEN_FILE", file("from-file\n"))
	if tok, err := resolveHTTPToken(); tok != "from-file" || err != nil {
		t.Errorf("file wins: %q %v", tok, err)
	}
	t.Setenv("ARC_MCP_HTTP_TOKEN_FILE", file(" \n"))
	if _, err := resolveHTTPToken(); err == nil {
		t.Error("an empty token file was accepted")
	}
	t.Setenv("ARC_MCP_HTTP_TOKEN_FILE", filepath.Join(dir, "missing"))
	if _, err := resolveHTTPToken(); err == nil {
		t.Error("a missing token file was accepted")
	}
	t.Setenv("ARC_MCP_HTTP_TOKEN_FILE", "")
	if tok, err := resolveHTTPToken(); tok != "from-env" || err != nil {
		t.Errorf("env: %q %v", tok, err)
	}
	t.Setenv("ARC_MCP_HTTP_TOKEN", " \t ")
	if _, err := resolveHTTPToken(); err == nil {
		t.Error("a blank ARC_MCP_HTTP_TOKEN was accepted as no token")
	}
	t.Setenv("ARC_MCP_HTTP_TOKEN", "")
	if tok, err := resolveHTTPToken(); tok != "" || err != nil {
		t.Errorf("none: %q %v", tok, err)
	}
}

func TestEnvOrDefaultInt(t *testing.T) {
	badEnv = nil
	t.Setenv("ARC_MCP_TEST_INT", "16")
	if n := envOrDefaultInt("ARC_MCP_TEST_INT", 8); n != 16 || badEnv != nil {
		t.Errorf("16: %d %v", n, badEnv)
	}
	for _, v := range []string{"abc", "4x", "1.5"} {
		badEnv = nil
		t.Setenv("ARC_MCP_TEST_INT", v)
		if n := envOrDefaultInt("ARC_MCP_TEST_INT", 8); n != 8 || badEnv == nil {
			t.Errorf("%q accepted as %d", v, n)
		}
	}
	badEnv = nil
}

func TestCheckPassthrough(t *testing.T) {
	unsetTokens := func() {
		for _, k := range []string{"ARC_TOKEN", "ARC_TOKEN_FILE", "ARC_MCP_HTTP_TOKEN", "ARC_MCP_HTTP_TOKEN_FILE"} {
			t.Setenv(k, "")
		}
	}
	tests := []struct {
		name      string
		env       string
		transport string
		flagTok   string
		url       string
		insecure  bool
		wantErr   bool
	}{
		{"ok", "", "http", "", "https://arc.example.com", false, false},
		{"stdio", "", "stdio", "", "https://arc.example.com", false, true},
		{"ARC_TOKEN set", "ARC_TOKEN", "http", "", "https://arc.example.com", false, true},
		{"ARC_TOKEN_FILE set", "ARC_TOKEN_FILE", "http", "", "https://arc.example.com", false, true},
		{"--arc-token", "", "http", "x", "https://arc.example.com", false, true},
		{"shared inbound token", "ARC_MCP_HTTP_TOKEN", "http", "", "https://arc.example.com", false, true},
		{"shared inbound token file", "ARC_MCP_HTTP_TOKEN_FILE", "http", "", "https://arc.example.com", false, true},
		{"insecure-listen", "", "http", "", "https://arc.example.com", true, true},
		{"query string in the Arc URL", "", "http", "", "https://arc.example.com/?p=admin", false, true},
		{"userinfo in the Arc URL", "", "http", "", "https://u:p@arc.example.com", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetTokens()
			if tt.env != "" {
				t.Setenv(tt.env, "/some/value")
			}
			if err := checkPassthrough(tt.transport, tt.flagTok, tt.url, tt.insecure); (err != nil) != tt.wantErr {
				t.Errorf("err %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHTTPTokenMustBeSendable(t *testing.T) {
	t.Setenv("ARC_MCP_HTTP_TOKEN_FILE", "")
	for v, ok := range map[string]bool{"abc123-_.~+/=": true, "two words": false, "p\u00e4ss": false, "a\tb": false} {
		t.Setenv("ARC_MCP_HTTP_TOKEN", v)
		if _, err := resolveHTTPToken(); (err == nil) != ok {
			t.Errorf("%q: err %v", v, err)
		}
	}
}
