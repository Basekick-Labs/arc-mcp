package arc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient_ListMeasurements_PathEscaping(t *testing.T) {
	tests := []struct {
		name     string
		database string
		wantPath string
	}{
		{"plain name", "telemetry", "/api/v1/databases/telemetry/measurements"},
		{"name with space", "my db", "/api/v1/databases/my%20db/measurements"},
		{"name with slash", "a/b", "/api/v1/databases/a%2Fb/measurements"},
		{"name with dotdot", "..", "/api/v1/databases/../measurements"},
		{"name with percent", "a%b", "/api/v1/databases/a%25b/measurements"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.EscapedPath()
				fmt.Fprint(w, `{"database":"","measurements":[],"count":0}`)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", time.Second)
			_, err := c.ListMeasurements(context.Background(), tt.database)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantPath {
				t.Errorf("outgoing path = %q, want %q", got, tt.wantPath)
			}
		})
	}
}

func TestClient_Query_ResponseSizeCap(t *testing.T) {
	// Stream a response larger than maxArcResponseBytes; the LimitReader should
	// cause JSON decoding to fail cleanly instead of buffering unbounded bytes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write a malformed JSON stream of ~maxArcResponseBytes+1KiB so the cap
		// is exceeded before the decoder finishes.
		_, _ = w.Write([]byte(`{"success":true,"columns":["x"],"data":[`))
		row := []byte(`["` + strings.Repeat("a", 1024) + `"],`)
		written := 0
		for written < maxArcResponseBytes+1024 {
			if _, err := w.Write(row); err != nil {
				return
			}
			written += len(row)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", 5*time.Second)
	_, err := c.Query(context.Background(), "db", "SELECT 1")
	if err == nil {
		t.Fatal("expected error from oversized response, got nil")
	}
}

func TestClient_SetsAuthAndDatabaseHeaders(t *testing.T) {
	var gotAuth, gotDB string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDB = r.Header.Get("x-arc-database")
		fmt.Fprint(w, `{"success":true,"columns":[],"data":[],"row_count":0,"execution_time_ms":0}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret", time.Second)
	if _, err := c.Query(context.Background(), "telemetry", "SELECT 1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret")
	}
	if gotDB != "telemetry" {
		t.Errorf("x-arc-database header = %q, want %q", gotDB, "telemetry")
	}
}

func TestClient_VerifyToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/auth/verify" {
			t.Errorf("request = %s %s, want GET /api/v1/auth/verify", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"valid":true,"permissions":["read"]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret", time.Second)
	permissions, err := c.VerifyToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret")
	}
	if len(permissions) != 1 || permissions[0] != "read" {
		t.Errorf("permissions = %v, want [read]", permissions)
	}
}
func TestClient_GetJSON_EncodesQueryAndAuth(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"type":"system","data":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/proxy", "secret", time.Second)
	query := make(url.Values)
	query.Set("duration_minutes", "30")
	body, err := c.GetJSON(context.Background(), "/api/v1/metrics/timeseries/system", query)
	if err != nil {
		t.Fatalf("GetJSON returned error: %v", err)
	}
	if string(body) != `{"type":"system","data":[]}` {
		t.Errorf("body = %s", body)
	}
	if gotPath != "/proxy/api/v1/metrics/timeseries/system" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "duration_minutes=30" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestClient_GetJSON_Readiness503IsAValidSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"not_ready","reason":"startup recovery"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", time.Second)
	body, err := c.GetJSON(context.Background(), "/ready", nil)
	if err != nil {
		t.Fatalf("not-ready response should be returned as data: %v", err)
	}
	if !json.Valid(body) || !strings.Contains(string(body), "startup recovery") {
		t.Errorf("unexpected readiness body: %s", body)
	}
}

func TestClient_GetJSON_ClassifiesUnsupportedAndAdminOnlyEndpoints(t *testing.T) {
	for _, tt := range []struct {
		name     string
		endpoint string
		status   int
		wantKind string
	}{
		{"unsupported Arc version", "/api/v1/metrics/memory", http.StatusNotFound, "unsupported"},
		{"application logs require admin", "/api/v1/logs", http.StatusForbidden, "admin_required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, `{"error":"not available"}`)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "token", time.Second)
			_, err := c.GetJSON(context.Background(), tt.endpoint, nil)
			apiErr, ok := err.(*Error)
			if !ok {
				t.Fatalf("error type = %T, want *Error", err)
			}
			if apiErr.Kind != tt.wantKind {
				t.Errorf("error kind = %q, want %q", apiErr.Kind, tt.wantKind)
			}
		})
	}
}
