package tools

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerDataTools registers every data tool.
func registerDataTools(server *mcp.Server, client *arc.Client) {
	RegisterListDatabases(server, client)
	RegisterListMeasurements(server, client)
	RegisterDescribeMeasurement(server, client, 50_000)
	RegisterQuery(server, client, 500, 50_000)
	RegisterGetSampleData(server, client, 50_000)
}

// dataToolCalls is one call of each data tool.
var dataToolCalls = []struct {
	tool string
	args map[string]any
}{
	{"list_databases", map[string]any{}},
	{"list_measurements", map[string]any{"database": "metrics"}},
	{"describe_measurement", map[string]any{"database": "metrics", "measurement": "cpu"}},
	{"query", map[string]any{"database": "metrics", "sql": "SELECT 1"}},
	{"get_sample_data", map[string]any{"database": "metrics", "measurement": "cpu"}},
}

// Markers in the configured URL. Go's error for a failed request quotes the
// URL; none of it may reach the model.
const (
	urlPrefixMarker = "arc-prefix-marker"
	urlQueryMarker  = "query-marker"
)

func assertNoArcAddress(t *testing.T, tool, text, addr string) {
	t.Helper()
	_, port, _ := net.SplitHostPort(addr)
	for _, s := range []string{urlPrefixMarker, urlQueryMarker, "127.0.0.1", ":" + port, "http", "dial", "Arc error"} {
		if strings.Contains(text, s) {
			t.Errorf("%s: answer carries %q: %q", tool, s, text)
		}
	}
}

// With Arc down, every data tool says so, in the same words, without
// Arc's address.
func TestDataTools_ArcUnreachableAnswersWithoutTheAddress(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	client := arc.NewClient("http://"+addr+"/"+urlPrefixMarker+"/?m="+urlQueryMarker, "tok", 10*time.Second)
	cs := newSessionClient(t, client, registerDataTools)

	const want = "Arc could not be reached. Ask the user to check that the Arc server is running at the configured URL."
	for _, c := range dataToolCalls {
		text, isErr := call(t, cs, c.tool, c.args)
		if !isErr || text != want {
			t.Errorf("%s: isError=%v text=%q, want the unreachable text", c.tool, isErr, text)
		}
		assertNoArcAddress(t, c.tool, text, addr)
	}
}

// A listing arc-mcp gave up on says that Arc did not answer in time, not the
// query's advice to change the SQL, and not Arc's address.
func TestListingTools_TimeoutAnswersWithoutTheAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	client := arc.NewClient(srv.URL+"/"+urlPrefixMarker+"?m="+urlQueryMarker, "tok", 100*time.Millisecond)
	cs := newSessionClient(t, client, registerDataTools)

	for _, c := range dataToolCalls[:2] {
		text, isErr := call(t, cs, c.tool, c.args)
		if !isErr || !strings.Contains(text, "did not answer within arc-mcp's --timeout") || strings.Contains(text, "Aggregate") {
			t.Errorf("%s: isError=%v text=%q", c.tool, isErr, text)
		}
		assertNoArcAddress(t, c.tool, text, addr)
	}
}

// A trailing slash on the configured URL, or on a path prefix in it, reaches
// Arc's routes rather than //api/v1/..., which Arc answers with 404.
func TestDataTools_TrailingSlashInTheURL(t *testing.T) {
	for _, tt := range []struct{ suffix, prefix string }{
		{"/", ""},
		{"//", ""},
		{"/arc", "/arc"},
		{"/arc/", "/arc"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.EscapedPath() {
			case tt.prefix + "/api/v1/databases":
				_, _ = w.Write([]byte(listBodyOK))
			case tt.prefix + "/api/v1/databases/metrics/measurements":
				_, _ = w.Write([]byte(measBodyOK))
			case tt.prefix + "/api/v1/query":
				_, _ = w.Write([]byte(`{"success":true,"columns":["x"],"data":[[1]],"row_count":1}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(notFound404))
			}
		}))
		cs := newSessionClient(t, arc.NewClient(srv.URL+tt.suffix, "tok", 10*time.Second), registerDataTools)
		for _, c := range []struct {
			tool string
			args map[string]any
			want string
		}{
			{"list_databases", map[string]any{}, "metrics"},
			{"list_measurements", map[string]any{"database": "metrics"}, "cpu"},
			{"query", map[string]any{"database": "metrics", "sql": "SELECT 1"}, "x"},
		} {
			text, isErr := call(t, cs, c.tool, c.args)
			if isErr || !strings.Contains(text, c.want) {
				t.Errorf("ARC_URL %q, %s: isError=%v text=%q", srv.URL+tt.suffix, c.tool, isErr, text)
			}
		}
		srv.Close()
	}
}
