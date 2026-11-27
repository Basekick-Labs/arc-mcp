package arc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recordingArc answers every request with an empty query result and records
// the Authorization header of the last one.
func recordingArc(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Int32) {
	t.Helper()
	var auth atomic.Value
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"columns":["n"],"data":[[1]],"row_count":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth, &n
}

func TestPassthroughSendsTheCallersToken(t *testing.T) {
	srv, auth, n := recordingArc(t)
	c := NewClient(srv.URL, "static-token", 5*time.Second)
	c.EnablePassthrough(nil)

	// No caller token: refused before anything is sent.
	_, err := c.Query(t.Context(), "db", "SELECT 1")
	var ae *Error
	if !errors.As(err, &ae) || ae.Kind != KindAuth || n.Load() != 0 {
		t.Fatalf("no caller token: err %v, %d requests sent", err, n.Load())
	}
	for name, call := range map[string]func(context.Context) error{
		"list databases":    func(ctx context.Context) error { _, err := c.ListDatabases(ctx); return err },
		"list measurements": func(ctx context.Context) error { _, err := c.ListMeasurements(ctx, "db"); return err },
		"ops":               func(ctx context.Context) error { _, _, err := c.Probe(ctx, "/health"); return err },
	} {
		if err := call(t.Context()); err == nil || n.Load() != 0 {
			t.Errorf("%s without a caller token: err %v, %d requests sent", name, err, n.Load())
		}
	}

	// With one: that token, never the static one.
	if _, err := c.Query(WithToken(t.Context(), "caller-a"), "db", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "Bearer caller-a" {
		t.Errorf("Authorization %q, want the caller's token", got)
	}
}

func TestStaticIgnoresAContextToken(t *testing.T) {
	srv, auth, _ := recordingArc(t)
	c := NewClient(srv.URL, "static-token", 5*time.Second)
	if _, err := c.Query(WithToken(t.Context(), "caller-a"), "db", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "Bearer static-token" {
		t.Errorf("Authorization %q, want the static token", got)
	}
}

func TestPassthroughReportsRejectedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"Invalid or expired token"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "", 5*time.Second)
	var rejected atomic.Value
	c.EnablePassthrough(func(token string) { rejected.Store(token) })
	_, err := c.Query(WithToken(t.Context(), "revoked"), "db", "SELECT 1")
	if rejected.Load() != "revoked" {
		t.Errorf("onRejected got %v", rejected.Load())
	}
	if msg := c.UserMessage(err); !strings.Contains(msg, "your MCP client sent") {
		t.Errorf("passthrough auth text: %q", msg)
	}
	if msg := NewClient(srv.URL, "x", time.Second).UserMessage(err); strings.Contains(msg, "your MCP client") {
		t.Errorf("static auth text mentions the client: %q", msg)
	}
}

func TestVerifyCaller(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"valid", 200, `{"valid":true,"token_info":{"id":7,"name":"alice","permissions":["read"]},"permissions":["read"]}`, nil},
		{"200 but not valid", 200, `{"valid":false}`, ErrVerifyUnavailable},
		{"200 not Arc's answer", 200, `<html>login</html>`, ErrVerifyUnavailable},
		{"invalid", 401, `{"valid":false,"error":"Invalid or expired token"}`, ErrCallerRejected},
		{"forbidden", 403, `{}`, ErrCallerRejected},
		{"auth turned off", 404, `Cannot GET`, ErrVerifyUnavailable},
		{"server error", 500, `{}`, ErrVerifyUnavailable},
		{"redirect to a login page", 302, ``, ErrVerifyUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("verify sent %q", r.Header.Get("Authorization"))
				}
				if tt.status == 302 {
					http.Redirect(w, r, "/login", http.StatusFound)
					return
				}
				if r.URL.Path == "/login" {
					_, _ = io.WriteString(w, `{"valid":true}`)
					return
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "", 0)
			c.EnablePassthrough(nil)
			caller, err := c.VerifyCaller(t.Context(), "tok")
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("err %v, want %v", err, tt.want)
			}
			if tt.want == nil && (caller.ID != 7 || caller.Name != "alice" || len(caller.Permissions) != 1) {
				t.Errorf("caller %+v", caller)
			}
		})
	}
	c := NewClient("http://127.0.0.1:1", "", 0)
	if _, err := c.VerifyCaller(t.Context(), "tok"); !errors.Is(err, ErrVerifyUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestVerifyCallerExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"valid":true,"token_info":{"id":1,"expires_at":"2030-01-02T03:04:05Z"}}`)
	}))
	defer srv.Close()
	caller, err := NewClient(srv.URL, "", 0).VerifyCaller(t.Context(), "tok")
	if err != nil || !caller.ExpiresAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("caller %+v, err %v", caller, err)
	}
}

func TestProbeAuthEnforced(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		ok      bool
		notEnfd bool
	}{
		{"Arc's 401", 401, `{"valid":false,"error":"Invalid or expired token"}`, true, false},
		{"a proxy's 401", 401, `Unauthorized`, false, false},
		{"auth off (no route)", 404, `Cannot GET /api/v1/auth/verify`, false, true},
		{"accepts anything", 200, `{"valid":true}`, false, true},
		{"server error", 500, `{}`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer arc-mcp-probe-") {
					t.Errorf("probe sent %q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			err := NewClient(srv.URL, "", 0).ProbeAuthEnforced(t.Context())
			if (err == nil) != tt.ok || errors.Is(err, ErrAuthNotEnforced) != tt.notEnfd {
				t.Errorf("err %v", err)
			}
		})
	}
	if err := NewClient("http://127.0.0.1:1", "", 0).ProbeAuthEnforced(t.Context()); err == nil || errors.Is(err, ErrAuthNotEnforced) {
		t.Errorf("unreachable: %v", err)
	}
}
