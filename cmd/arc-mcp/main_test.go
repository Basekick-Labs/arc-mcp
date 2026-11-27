package main

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

	// An empty (or whitespace-only) file is refused, not read as no token.
	t.Run("empty file returns error", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "token.txt")
		if err := os.WriteFile(f, []byte(" \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ARC_TOKEN_FILE", f)
		t.Setenv("ARC_TOKEN", "fallback")
		if tok, err := resolveToken(""); err == nil {
			t.Errorf("empty ARC_TOKEN_FILE accepted as %q", tok)
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
		{"http IPv4-mapped loopback with token ok", "http://[::ffff:127.0.0.1]:8000", "abc", false, false},
		// A name under .localhost is not exempt from HTTP_PROXY: remote.
		{"http *.localhost with token rejected", "http://foo.localhost:8000", "abc", false, true},
		{"http *.localhost with query and no token rejected", "http://foo.localhost:8000/?p=abc", "", false, true},
		{"http *.localhost without token ok", "http://foo.localhost:8000", "", false, false},
		{"http *.localhost with token and insecure ok", "http://foo.localhost:8000", "abc", true, false},
		{"https *.localhost with token ok", "https://foo.localhost:8000", "abc", false, false},
		{"http LOCALHOST with token rejected", "http://LOCALHOST:8000", "abc", false, true},
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

// loopbackHosts are Hostname() values (IPv6 without brackets) and whether
// they may receive credentials over plaintext HTTP.
var loopbackHosts = []struct {
	host string
	want bool
}{
	{"localhost", true},
	{"127.0.0.1", true},
	{"127.1.2.3", true},
	{"127.255.255.255", true},
	{"::1", true},
	{"0:0:0:0:0:0:0:1", true},
	{"::ffff:127.0.0.1", true},
	// Proxied when HTTP_PROXY is set: the proxy rule exempts only exactly
	// "localhost" and loopback IP literals.
	{"LOCALHOST", false},
	{"Localhost", false},
	{"localhost.", false},
	{"foo.localhost", false},
	{"FOO.LOCALHOST", false},
	{"localhost.example.com", false},
	{"0.0.0.0", false},
	{"::", false},
	{"128.0.0.1", false},
	// Not IP literals: resolved as names.
	{"127.1", false},
	{"::1%lo", false},
	// Hostname() strips the brackets; a bracketed value is not a host.
	{"[::1]", false},
	{"", false},
}

func TestIsLoopbackHost(t *testing.T) {
	for _, tt := range loopbackHosts {
		if got := isLoopbackHost(tt.host); got != tt.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

// Every host isLoopbackHost accepts must be one net/http never sends to a
// proxy. ProxyFromEnvironment reads the environment once per process, so the
// check runs in a child test process with HTTP_PROXY set and NO_PROXY empty.
func TestLoopbackHostsAreNeverProxied(t *testing.T) {
	if os.Getenv("ARC_MCP_TEST_PROXY_CHILD") == "1" {
		checkLoopbackHostsNotProxied(t)
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLoopbackHostsAreNeverProxied$")
	cmd.Env = append(os.Environ(),
		"ARC_MCP_TEST_PROXY_CHILD=1",
		"HTTP_PROXY=http://proxy.invalid:3128", "http_proxy=",
		"NO_PROXY=", "no_proxy=",
		"REQUEST_METHOD=", // not a CGI process, which would refuse HTTP_PROXY
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child test failed: %v\n%s", err, out)
	}
}

func checkLoopbackHostsNotProxied(t *testing.T) {
	proxyFor := func(host string) *url.URL {
		t.Helper()
		u := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, "8000")}
		p, err := http.ProxyFromEnvironment(&http.Request{URL: u})
		if err != nil {
			t.Fatalf("ProxyFromEnvironment(%s): %v", u, err)
		}
		return p
	}
	if proxyFor("arc.example.com") == nil {
		t.Fatal("HTTP_PROXY is not in effect in the child; the check would prove nothing")
	}
	for _, tt := range loopbackHosts {
		if tt.host == "" || strings.HasPrefix(tt.host, "[") {
			continue
		}
		if isLoopbackHost(tt.host) && proxyFor(tt.host) != nil {
			t.Errorf("%q counts as loopback but a request to it goes through HTTP_PROXY", tt.host)
		}
	}
}
