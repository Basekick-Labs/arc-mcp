package arc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// NewClient keeps one form of the base URL: no trailing slash on its path,
// the path prefix and the query string as given.
func TestNewClient_NormalisesBaseURL(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"http://h:8000", "http://h:8000"},
		{"http://h:8000/", "http://h:8000"},
		{"http://h:8000///", "http://h:8000"},
		{"http://h:8000/arc", "http://h:8000/arc"},
		{"http://h:8000/arc/", "http://h:8000/arc"},
		{"http://h:8000/arc//", "http://h:8000/arc"},
		{"http://h:8000/?k=v", "http://h:8000?k=v"},
		{"http://h:8000/arc/?k=v", "http://h:8000/arc?k=v"},
		{"https://h/a/b/", "https://h/a/b"},
		// An escaped slash is part of the prefix's name, not a trailing slash.
		{"http://h:8000/a%2Fb/", "http://h:8000/a%2Fb"},
		{"http://h:8000/arc%2F", "http://h:8000/arc%2F"},
		{"http://h:8000/my%20arc/", "http://h:8000/my%20arc"},
		// Does not parse: kept as given, and every request reports it.
		{"http://h:8000/%zz/", "http://h:8000/%zz/"},
	} {
		if got := NewClient(tt.in, "", time.Second).baseURL; got != tt.want {
			t.Errorf("NewClient(%q).baseURL = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Every request, the startup check included, goes to the route under the
// configured prefix, never to //route, and carries the configured query
// string. Arc answers //api/v1/... with 404.
func TestClient_EveryRequestKeepsTheBasePath(t *testing.T) {
	// One body every method decodes as a success.
	const okBody = `{"status":"ok","valid":true,"permissions":["read"],"databases":[],"measurements":[],"count":0,"success":true,"columns":[],"data":[]}`
	routes := []struct {
		name  string
		route string
		call  func(context.Context, *Client) error
	}{
		{"health", "/health", func(ctx context.Context, c *Client) error { return c.Health(ctx) }},
		{"verify token", "/api/v1/auth/verify", func(ctx context.Context, c *Client) error { _, err := c.VerifyToken(ctx); return err }},
		{"list databases", "/api/v1/databases", func(ctx context.Context, c *Client) error { _, err := c.ListDatabases(ctx); return err }},
		{"list measurements", "/api/v1/databases/db/measurements", func(ctx context.Context, c *Client) error { _, err := c.ListMeasurements(ctx, "db"); return err }},
		{"query", "/api/v1/query", func(ctx context.Context, c *Client) error { _, err := c.Query(ctx, "db", "SELECT 1"); return err }},
	}
	shapes := []struct {
		suffix, prefix, query string
	}{
		{"", "", ""},
		{"/", "", ""},
		{"///", "", ""},
		{"/arc", "/arc", ""},
		{"/arc/", "/arc", ""},
		{"/arc//", "/arc", ""},
		{"/?k=v", "", "k=v"},
		{"/arc/?k=v", "/arc", "k=v"},
		{"/a%2Fb/", "/a%2Fb", ""},
	}
	for _, sh := range shapes {
		for _, rt := range routes {
			t.Run(fmt.Sprintf("%s %q", rt.name, sh.suffix), func(t *testing.T) {
				want := sh.prefix + rt.route
				var gotPath, gotQuery string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
					if gotPath != want {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(okBody))
				}))
				defer srv.Close()

				err := rt.call(context.Background(), NewClient(srv.URL+sh.suffix, "tok", 2*time.Second))
				if gotPath != want || gotQuery != sh.query {
					t.Errorf("request went to path %q query %q, want %q %q", gotPath, gotQuery, want, sh.query)
				}
				if err != nil {
					t.Errorf("error: %v", err)
				}
			})
		}
	}
}

// buildURL drops a trailing slash itself, so a client whose base URL did not
// come through NewClient still reaches Arc's routes.
func TestBuildURL_TrimsTheBasePathItself(t *testing.T) {
	for _, tt := range []struct{ base, want string }{
		{"http://h:8000/", "http://h:8000/api/v1/databases"},
		{"http://h:8000/arc//", "http://h:8000/arc/api/v1/databases"},
		{"http://h:8000/arc/?k=v", "http://h:8000/arc/api/v1/databases?k=v"},
		{"http://h:8000/a%2Fb/", "http://h:8000/a%2Fb/api/v1/databases"},
	} {
		c := &Client{baseURL: tt.base}
		got, err := c.buildURL("api", "v1", "databases")
		if err != nil || got != tt.want {
			t.Errorf("buildURL on %q = %q, %v; want %q", tt.base, got, err, tt.want)
		}
	}
}
