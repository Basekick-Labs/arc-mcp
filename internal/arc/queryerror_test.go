package arc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// errBody is Arc's JSON body for a failed query.
func errBody(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"success": false, "error": msg})
	return b
}

// Bodies as Arc returns them for DuckDB errors (collected from a live
// instance), each with the LINE block that carries Arc's rewritten SQL.
func TestUserMessage_QueryErrorsKeepTheFixAndLoseThePaths(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		keep []string
	}{
		{"binder, misspelled column",
			"arrow query failed: Binder Error: Referenced column \"altitud\" not found in FROM clause!\nCandidate bindings: \"altitude\", \"latitude\", \"longitude\", \"callsign\", \"time\"\n\nLINE 1: SELECT avg(altitud) FROM read_parquet(['/app/.tmp/arc-uploads/schema...\n                   ^",
			[]string{"Binder Error", `"altitud"`, `Candidate bindings: "altitude"`}},
		{"binder, GROUP BY advice",
			"arrow query failed: Binder Error: column \"icao24\" must appear in the GROUP BY clause or must be part of an aggregate function.\nEither add it to the GROUP BY list, or use \"ANY_VALUE(icao24)\" if the exact value of \"icao24\" is not important.\n\nLINE 1: SELECT icao24 FROM read_parquet(['/app/.tmp/arc-uploads/schema...\n",
			[]string{"GROUP BY", "ANY_VALUE(icao24)"}},
		{"conversion, the user's own value",
			"arrow query failed: Conversion Error: Could not convert string 'JBU193' to INT32 when casting from source column callsign\n\nLINE 1: SELECT CAST(callsign AS INTEGER) FROM read_parquet(['/app/.tmp...\n               ^",
			[]string{"Conversion Error", "'JBU193'", "callsign"}},
		{"catalog, did you mean",
			"arrow query failed: Catalog Error: Scalar Function with name nosuchfunc does not exist!\nDid you mean \"cosh\"?\n\nLINE 1: SELECT nosuchfunc(altitude) FROM read_parquet(['/app/.tmp/arc-uploads...\n               ^",
			[]string{"nosuchfunc", `Did you mean "cosh"`}},
		{"parser", "arrow query failed: Parser Error: SELECT clause without selection list", []string{"Parser Error"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UserMessage(arcErrorFrom("query", 500, errBody(tt.raw)))
			if !strings.HasPrefix(got, "The query failed: ") {
				t.Fatalf("got %q", got)
			}
			for _, k := range tt.keep {
				if !strings.Contains(got, k) {
					t.Errorf("lost %q: %q", k, got)
				}
			}
			for _, leak := range []string{"LINE 1", "/app/", "read_parquet([", "arrow query failed"} {
				if strings.Contains(got, leak) {
					t.Errorf("leaked %q: %q", leak, got)
				}
			}
		})
	}
}

// Anything that names storage is removed, whatever its shape, including in
// the message line itself.
func TestQueryErrorText_RedactsPathsByContent(t *testing.T) {
	tests := []struct{ raw, leak string }{
		{"Invalid Input Error: failed to read s3://bucket/db/cpu/x.parquet now", "s3://"},
		{`Binder Error: table function read_parquet('./data/db/cpu/**/*.parquet') failed`, "./data"},
		{"Conversion Error: could not parse /var/lib/arc/tmp/file as a number", "/var/lib"},
		{`Binder Error: path "C:\arc\data\x" unreadable`, `C:\arc`},
		{`Binder Error: share \\fileserver\share\arc missing`, "fileserver"},
		{"Binder Error: name `/app/data/x` bad", "/app/data"},
		{"Binder Error: at file:/app/data/tenant2/cpu", "tenant2"},
		{"Binder Error: key %2Fapp%2Fdata%2Ftenant2 bad", "tenant2"},
		{"Binder Error: under data/tenant2/cpu/2026/11/x.parquet", "tenant2"},
		{"Binder Error: partition year=2026/month=11 bad", "year=2026"},
		{"Binder Error: endpoint minio.internal.corp:9000 refused", "minio.internal"},
		{"Binder Error: endpoint 10.0.3.7 refused", "10.0.3.7"},
		{"Binder Error: file /app/my data/db/x.parquet", "data/db"},
		{"Binder Error: read_parquet(['/app/a(1).parquet', '/app/b.parquet']) failed", "a(1)"},
		{"Binder Error: no way 'azure://acct/container/db/cpu/x'", "azure://"},
	}
	for _, tt := range tests {
		got := queryErrorText(tt.raw)
		if got == "" {
			t.Errorf("%q: dropped entirely, want the class kept", tt.raw)
			continue
		}
		if strings.Contains(got, tt.leak) {
			t.Errorf("%q leaked %q: %q", tt.raw, tt.leak, got)
		}
	}
}

// What the model needs to fix its query survives: operators, its own
// values, field names with a single slash.
func TestQueryErrorText_KeepsTheModelsOwnText(t *testing.T) {
	for raw, keep := range map[string]string{
		"Binder Error: No function matches the given name and argument types '/(VARCHAR, INTEGER_LITERAL)'": "'/(VARCHAR, INTEGER_LITERAL)'",
		"Conversion Error: Could not convert string '2026/11/26' to TIMESTAMP":                              "'2026/11/26'",
		`Binder Error: Referenced column "usage/idle" not found`:                                            `"usage/idle"`,
		`Binder Error: Referenced column "altitud" not found`:                                               `"altitud"`,
	} {
		if got := queryErrorText(raw); !strings.Contains(got, keep) {
			t.Errorf("%q lost %q: %q", raw, keep, got)
		}
	}
}

// Problems in the stored files are not the statement's fault: no change to
// the SQL fixes them, and their advice is not the model's to follow.
func TestQueryErrorText_StorageFaultsStayGeneric(t *testing.T) {
	for _, raw := range []string{
		"Invalid Input Error: File '/data/arc/db/cpu/x.parquet' too small to be a Parquet file",
		"Invalid Input Error: No magic bytes found at end of file 'azure://acct/c/x.parquet'",
		"Invalid Input Error: Footer length error in file '/data/x.parquet'",
		"Conversion Error: In file \"/app/data/acme/cpu/x.parquet\" the column \"usage_idle\" has type VARCHAR, but we are trying to read it as type DOUBLE.\nThis means the parquet schema does not match\n* Enable the union_by_name=True option",
		"Invalid Input Error: Failed to read file \"s3://b/x\": schema mismatch in glob",
	} {
		if got := queryErrorText(raw); got != "" {
			t.Errorf("%q passed through: %q", raw, got)
		}
	}
	if got := queryErrorText("Binder Error: \n\nLINE 1: SELECT"); got != "" {
		t.Errorf("an empty message passed through: %q", got)
	}
	if got := queryErrorText("Expression Error: bad expression x"); got == "" {
		t.Error("Expression Error dropped")
	}
}

// Classes the model cannot fix, or that are about storage, stay generic.
func TestUserMessage_OtherFailuresStayGeneric(t *testing.T) {
	generic := "Arc returned an error — see server logs for details."
	for _, tc := range []struct {
		op     string
		status int
		body   []byte
	}{
		{"query", 500, errBody("arrow query failed: IO Error: Cannot open file \"/data/arc/x.parquet\": No such file")},
		{"query", 500, errBody("arrow query failed: HTTP Error: Unable to connect to s3://bucket")},
		{"query", 500, []byte("<html>500 Internal Server Error</html>")},
		{"list databases", 500, errBody("arrow query failed: Binder Error: x")},
	} {
		if got := UserMessage(arcErrorFrom(tc.op, tc.status, tc.body)); got != generic {
			t.Errorf("%s %d %q: got %q, want generic", tc.op, tc.status, tc.body, got)
		}
	}
	if got := queryErrorText("some wrapper failed: Binder Error: x"); got != "" {
		t.Errorf("a prefix other than Arc's was accepted: %q", got)
	}
}

func TestUserMessage_MemoryAndTimeoutSayWhatToChange(t *testing.T) {
	oom := UserMessage(arcErrorFrom("query", 500, errBody("arrow query failed: Out of Memory Error: failed to allocate block of 262144 bytes (/tmp/duckdb_spill)")))
	if !strings.Contains(oom, "ran out of memory") || strings.Contains(oom, "/tmp") {
		t.Errorf("OOM: %q", oom)
	}
	notOOM := UserMessage(arcErrorFrom("query", 500, errBody("arrow query failed: Conversion Error: Could not convert string 'Out of Memory Error' to INT32")))
	if strings.Contains(notOOM, "ran out of memory") {
		t.Errorf("a value mentioning memory read as OOM: %q", notOOM)
	}
	timeout := UserMessage(arcErrorFrom("query", 504, errBody("Query timed out")))
	if !strings.Contains(timeout, "timed out") || !strings.Contains(timeout, "narrow the time range") {
		t.Errorf("timeout: %q", timeout)
	}
}

// A failed query over many partitions carries a long LINE block; the error
// is still found when the body runs well past what is logged.
func TestClient_Query_LongErrorBodyStillParses(t *testing.T) {
	var paths []string
	for i := 0; i < 400; i++ {
		paths = append(paths, fmt.Sprintf("'/app/data/db/cpu/2026/11/%02d/%02d/cpu_%d.parquet'", i%28+1, i%24, i))
	}
	raw := "arrow query failed: Binder Error: Referenced column \"altitud\" not found in FROM clause!\nCandidate bindings: \"altitude\"\n\nLINE 1: SELECT avg(altitud) FROM read_parquet([" + strings.Join(paths, ", ") + "])"
	body := errBody(raw)
	if len(body) < 4*maxErrorBodyBytes {
		t.Fatalf("fixture too short: %d bytes", len(body))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok", time.Second).Query(context.Background(), "db", "SELECT 1")
	got := UserMessage(err)
	if !strings.Contains(got, `Candidate bindings: "altitude"`) || strings.Contains(got, "/app/") {
		t.Errorf("got %q", got)
	}
}

func TestQueryErrorText_StreamFailurePrefixes(t *testing.T) {
	for _, raw := range []string{
		"arrow reader error after 1000 rows: Conversion Error: Could not convert string '...' to INT64",
		"iterator error after 1 row: Conversion Error: Could not convert string '...' to INT64",
		"scan failed at row 7: Conversion Error: Could not convert string '...' to INT64",
	} {
		if got := queryErrorText(raw); got != "Conversion Error: Could not convert string '...' to INT64" {
			t.Errorf("%q -> %q", raw, got)
		}
	}
}

// arc-mcp's own --timeout fires before Arc's; the model gets the hint, and
// neither that nor an unreachable Arc puts Arc's URL in front of it.
func TestClient_Query_TimeoutAndNetworkErrorsHideTheURL(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	_, err := NewClient(slow.URL, "tok", 50*time.Millisecond).Query(context.Background(), "db", "SELECT 1")
	got := UserMessage(err)
	if !strings.Contains(got, "timed out") || strings.Contains(got, "127.0.0.1") || strings.Contains(got, "http") {
		t.Errorf("timeout: %q", got)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	addr := gone.URL
	gone.Close()
	_, err = NewClient(addr, "tok", time.Second).Query(context.Background(), "db", "SELECT 1")
	got = UserMessage(err)
	if !strings.Contains(got, "could not be reached") || strings.Contains(got, "127.0.0.1") {
		t.Errorf("network: %q", got)
	}
}

func TestUserMessage_RetryOnlyWhenArcSaysSo(t *testing.T) {
	vanished := UserMessage(arcErrorFrom("query", 503, errBody("data read by this query was removed while it ran (retention, compaction or a delete); retry the query")))
	if !strings.Contains(vanished, "Run it again once") {
		t.Errorf("vanished: %q", vanished)
	}
	other := UserMessage(arcErrorFrom("query", 503, errBody("Server shutting down")))
	if other != "Arc returned an error — see server logs for details." {
		t.Errorf("other 503: %q", other)
	}
}

func TestUserMessage_QueryKindCarriesArcsText(t *testing.T) {
	err := &Error{Kind: KindQuery, Message: queryErrorText("Binder Error: Referenced column \"x\" not found")}
	if got := UserMessage(err); got != `The query failed: Binder Error: Referenced column "x" not found` {
		t.Errorf("got %q", got)
	}
	if got := UserMessage(&Error{Kind: KindQuery}); got != "Arc rejected the query — check the SQL syntax." {
		t.Errorf("no text: %q", got)
	}
}

// A body past the read limit no longer parses; the model gets the generic
// message, never a fragment.
func TestUserMessage_BodyPastTheLimitIsGeneric(t *testing.T) {
	big := errBody("arrow query failed: Binder Error: x\n\nLINE 1: " + strings.Repeat("'/app/data/x.parquet', ", maxQueryErrorBodyBytes/20))
	cut := big[:maxQueryErrorBodyBytes]
	if got := UserMessage(arcErrorFrom("query", 500, cut)); got != "Arc returned an error — see server logs for details." {
		t.Errorf("got %q", got)
	}
}
