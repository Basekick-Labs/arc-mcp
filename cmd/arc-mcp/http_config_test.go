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
	t.Setenv("ARC_MCP_HTTP_TOKEN", "")
	if tok, err := resolveHTTPToken(); tok != "" || err != nil {
		t.Errorf("none: %q %v", tok, err)
	}
}
