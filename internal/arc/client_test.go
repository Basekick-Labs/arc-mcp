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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write a malformed JSON stream of ~maxArcResponseBytes+1KiB so the cap
		// is exceeded before the decoder finishes.
		w.Write([]byte(`{"success":true,"columns":["x"],"data":[`))
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
