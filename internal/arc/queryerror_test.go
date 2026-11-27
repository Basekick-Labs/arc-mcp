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
	for _, raw := range []string{
		"Invalid Input Error: File '/data/arc/db/cpu/2026/11/26/00/x.parquet' too small to be a Parquet file",
		"Invalid Input Error: No magic bytes found at end of file 'azure://acct/container/db/cpu/x.parquet'",
		"Invalid Input Error: failed to read s3://bucket/db/cpu/x.parquet now",
		`Binder Error: table function read_parquet('./data/db/cpu/**/*.parquet') failed`,
		"Conversion Error: could not parse /var/lib/arc/tmp/file as a number",
		`Invalid Input Error: path "C:\arc\data\x.parquet" unreadable`,
	} {
		got := QueryErrorText(raw)
		if got == "" {
			t.Errorf("%q: dropped entirely, want the class kept", raw)
			continue
		}
		for _, leak := range []string{"/data/", "azure://", "s3://", "./data", "/var/lib", `C:\arc`, "x.parquet", "file '/"} {
			if strings.Contains(got, leak) {
				t.Errorf("%q leaked %q: %q", raw, leak, got)
			}
		}
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
	if got := QueryErrorText("some wrapper failed: Binder Error: x"); got != "" {
		t.Errorf("a prefix other than Arc's was accepted: %q", got)
	}
}

func TestUserMessage_MemoryAndTimeoutSayWhatToChange(t *testing.T) {
	oom := UserMessage(arcErrorFrom("query", 500, errBody("arrow query failed: Out of Memory Error: failed to allocate block of 262144 bytes (/tmp/duckdb_spill)")))
	if !strings.Contains(oom, "ran out of memory") || strings.Contains(oom, "/tmp") {
		t.Errorf("OOM: %q", oom)
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
