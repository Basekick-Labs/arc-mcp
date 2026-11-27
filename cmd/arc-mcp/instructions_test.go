package main

import (
	"os"
	"strings"
	"testing"

	"github.com/basekick-labs/arc-mcp/internal/tools"
)

// The instructions carry the row cap the server actually enforces, and the
// habits the README promises: time bounds and aggregation.
func TestServerInstructions(t *testing.T) {
	got := serverInstructions(1234, tools.Groups{Data: true})
	for _, want := range []string{"time-series lakehouse", "capped at 1234 rows", "WHERE time > now() - INTERVAL", "time_bucket(INTERVAL '5 minutes', time)", `"load"`} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	if strings.Contains(got, "columnar analytical") {
		t.Error("old positioning still present")
	}
}

// With the default group, a client receives exactly what it did before tool
// groups existed. testdata/instructions-data-500.txt is that text, taken
// from arc-mcp c5f3fd5 (and identical to its binary's initialize answer).
func TestServerInstructions_DefaultIsUnchanged(t *testing.T) {
	want, err := os.ReadFile("testdata/instructions-data-500.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := serverInstructions(500, tools.Groups{Data: true}); got != string(want) {
		t.Errorf("default instructions changed:\n got %q\nwant %q", got, want)
	}
}

// The operations paragraph follows the data text unchanged; alone, the
// operations tools get a lead with no query habits, since nothing queries.
func TestServerInstructions_Ops(t *testing.T) {
	data := serverInstructions(500, tools.Groups{Data: true})
	both := serverInstructions(500, tools.Groups{Data: true, Ops: true})
	if both != data+"\n\n"+opsInstructions {
		t.Errorf("data+ops instructions are not the data text plus the ops paragraph:\n%s", both)
	}
	for _, want := range []string{"arc_health", "arc_metrics", "configured URL", "load balancer", "503"} {
		if !strings.Contains(opsInstructions, want) {
			t.Errorf("ops paragraph lacks %q", want)
		}
	}

	only := serverInstructions(500, tools.Groups{Ops: true})
	if only != opsOnlyLead+"\n\n"+opsInstructions {
		t.Errorf("ops-only instructions:\n%s", only)
	}
	for _, habit := range []string{"time_bucket", "WHERE time", "capped at", "list_databases", "SQL"} {
		if strings.Contains(only, habit) {
			t.Errorf("ops-only instructions mention %q, a data tool habit", habit)
		}
	}
}
