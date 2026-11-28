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

// With the default group, a client receives the data text alone, with no
// operations paragraph.
func TestServerInstructions_DefaultIsUnchanged(t *testing.T) {
	groups, _, err := tools.ParseGroups(tools.DefaultGroups)
	if err != nil {
		t.Fatal(err)
	}
	if got := serverInstructions(500, groups); got != dataInstructions(500) {
		t.Errorf("default instructions are not the data text alone:\n%s", got)
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

// The Enterprise paragraph follows the others unchanged; without the data
// tools the lead says the server reports on Arc itself, never that only
// operations tools are on.
func TestServerInstructions_Enterprise(t *testing.T) {
	data := serverInstructions(500, tools.Groups{Data: true})
	ops := serverInstructions(500, tools.Groups{Data: true, Ops: true})
	for groups, want := range map[tools.Groups]string{
		{Data: true, Enterprise: true}:            data + "\n\n" + enterpriseInstructions,
		{Data: true, Ops: true, Enterprise: true}: ops + "\n\n" + enterpriseInstructions,
		{Enterprise: true}:                        serverOnlyLead + "\n\n" + enterpriseInstructions,
		{Ops: true, Enterprise: true}:             serverOnlyLead + "\n\n" + opsInstructions + "\n\n" + enterpriseInstructions,
	} {
		if got := serverInstructions(500, groups); got != want {
			t.Errorf("%s instructions:\n%s", groups, got)
		}
	}
	for _, want := range []string{"arc_enterprise", "capabilities", "not licensed", "answered_by", "configured URL"} {
		if !strings.Contains(enterpriseInstructions, want) {
			t.Errorf("enterprise paragraph lacks %q", want)
		}
	}
	if strings.Contains(serverOnlyLead, "operations tools") {
		t.Error("the lead without data names only the operations tools")
	}
}
