package main

import (
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
