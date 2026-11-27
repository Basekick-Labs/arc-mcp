package tools

import (
	"context"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// describeBodyMs is describeBody with the DESCRIBE time Arc reports.
func describeBodyMs(ms int) string {
	return fmt.Sprintf(`{"success":true,"columns":["column_name","column_type","null","key","default","extra"],`+
		`"data":[["time","TIMESTAMP WITH TIME ZONE","YES",null,null,null]],"row_count":1,"execution_time_ms":%d}`, ms)
}

const (
	fullStatsSQL   = "SELECT count(*) as row_count, min(time) as earliest, max(time) as latest FROM cpu"
	foldedStatsSQL = "SELECT (SELECT count(*) FROM cpu) AS row_count, (SELECT time FROM cpu ORDER BY time LIMIT 1) AS earliest, " +
		"(SELECT max(time) FROM cpu WHERE time > (SELECT now() - INTERVAL '30 days')) AS latest"
	latestSQL = "SELECT max(time) AS latest FROM cpu"
)

// statsRowBody is an Arc success body with one row of statistics, values as
// raw JSON.
func statsRowBody(values ...string) string {
	return fmt.Sprintf(`{"success":true,"columns":["row_count","earliest","latest"],"data":[[%s]],"row_count":1,"execution_time_ms":5}`,
		strings.Join(values, ","))
}

// statsBlock is the Statistics block as describe_measurement has always
// rendered it, so a computed block can be compared byte for byte.
func statsBlock(count, earliest, latest string) string {
	return "\n### Statistics\n\n" +
		"- **Row count:** " + count + "\n" +
		"- **Earliest:** " + earliest + "\n" +
		"- **Latest:** " + latest + "\n"
}

func describeCPU(t *testing.T, fake *fakeArc, timeout time.Duration) string {
	t.Helper()
	cs := newSessionTimeout(t, fake, timeout, func(server *mcp.Server, client *arc.Client) {
		RegisterDescribeMeasurement(server, client, 50_000)
	})
	text, isErr := call(t, cs, "describe_measurement", map[string]any{"database": "db", "measurement": "cpu"})
	if isErr {
		t.Fatalf("error result: %s", text)
	}
	return text
}

func wantSent(t *testing.T, fake *fakeArc, want ...string) {
	t.Helper()
	got := fake.received()
	if len(got) != len(want) {
		t.Fatalf("statements sent = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The DESCRIBE time picks the statement: below the gate the folded one, at
// or above it the full scan. Either way the block is the text describe has
// always printed for the same values.
func TestDescribe_StatsGate(t *testing.T) {
	tests := []struct {
		name       string
		describeMs int
		wantSQL    string
	}{
		{"absent (unknown)", 0, fullStatsSQL},
		{"one millisecond", 1, foldedStatsSQL},
		{"just under the gate", statsGateMs - 1, foldedStatsSQL},
		{"at the gate", statsGateMs, fullStatsSQL},
		{"many files", 460, fullStatsSQL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArc{reply: func(sql string) (int, string) {
				if strings.HasPrefix(sql, "DESCRIBE") {
					return 200, describeBodyMs(tt.describeMs)
				}
				return 200, statsRowBody(`150000000`, `"2024-11-26T00:00:00Z"`, `"2026-11-25T23:59:59.57952Z"`)
			}}
			text := describeCPU(t, fake, 5*time.Second)
			wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", tt.wantSQL)
			want := statsBlock("150000000", "2024-11-26T00:00:00Z", "2026-11-25T23:59:59.57952Z")
			if !strings.HasSuffix(text, want) {
				t.Errorf("statistics block differs; want suffix %q\ngot:\n%s", want, text)
			}
		})
	}
}

// A measurement with no data in the last 30 days: the window's aggregate
// returns its row with a null, and only then is the unbounded Latest asked.
func TestDescribe_StaleLatestFallsBack(t *testing.T) {
	fake := &fakeArc{reply: func(sql string) (int, string) {
		switch sql {
		case "DESCRIBE SELECT * FROM cpu":
			return 200, describeBodyMs(40)
		case latestSQL:
			return 200, `{"success":true,"columns":["latest"],"data":[["2024-05-31T23:59:58.5Z"]],"row_count":1}`
		}
		return 200, statsRowBody(`20000000`, `"2023-06-01T00:00:00Z"`, `null`)
	}}
	text := describeCPU(t, fake, 5*time.Second)
	wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", foldedStatsSQL, latestSQL)
	if want := statsBlock("20000000", "2023-06-01T00:00:00Z", "2024-05-31T23:59:58.5Z"); !strings.HasSuffix(text, want) {
		t.Errorf("want suffix %q\ngot:\n%s", want, text)
	}
}

// At or above the gate the full statement is the only one: its max(time) is
// already unbounded, so a null Latest there is final, not stale. The fixture
// (rows, but a null Latest) is not a state Arc produces unless every time is
// null: the guard it pins is defensive, against a wasted second scan.
func TestDescribe_FullStatementHasNoFallback(t *testing.T) {
	fake := &fakeArc{reply: func(sql string) (int, string) {
		if strings.HasPrefix(sql, "DESCRIBE") {
			return 200, describeBodyMs(400)
		}
		return 200, statsRowBody(`5`, `null`, `null`)
	}}
	text := describeCPU(t, fake, 5*time.Second)
	wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", fullStatsSQL)
	if want := statsBlock("5", "none", "none"); !strings.HasSuffix(text, want) {
		t.Errorf("want suffix %q\ngot:\n%s", want, text)
	}
}

// Files with no rows: count 0 and null times, as the full statement gives,
// and no second statement.
func TestDescribe_ZeroRowsNoFallback(t *testing.T) {
	fake := &fakeArc{reply: func(sql string) (int, string) {
		if strings.HasPrefix(sql, "DESCRIBE") {
			return 200, describeBodyMs(2)
		}
		return 200, statsRowBody(`0`, `null`, `null`)
	}}
	text := describeCPU(t, fake, 5*time.Second)
	wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", foldedStatsSQL)
	if want := statsBlock("0", "none", "none"); !strings.HasSuffix(text, want) {
		t.Errorf("want suffix %q\ngot:\n%s", want, text)
	}
}

// waitForCancel holds a statement until arc-mcp gives up on it.
func waitForCancel(ctx context.Context) (int, string) {
	<-ctx.Done()
	return 200, statsRowBody(`1`, `null`, `null`)
}

// Out of time: each line says so instead of the block disappearing, and
// nothing more is sent.
func TestDescribe_StatsDeadline(t *testing.T) {
	t.Run("folded statement", func(t *testing.T) {
		fake := &fakeArc{replyCtx: func(ctx context.Context, sql string) (int, string) {
			if strings.HasPrefix(sql, "DESCRIBE") {
				return 200, describeBodyMs(30)
			}
			return waitForCancel(ctx)
		}}
		text := describeCPU(t, fake, 200*time.Millisecond)
		wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", foldedStatsSQL)
		nc := "not computed (timed out after 200ms)"
		if want := statsBlock(nc, nc, nc); !strings.HasSuffix(text, want) {
			t.Errorf("want suffix %q\ngot:\n%s", want, text)
		}
	})
	t.Run("full statement", func(t *testing.T) {
		fake := &fakeArc{replyCtx: func(ctx context.Context, sql string) (int, string) {
			if strings.HasPrefix(sql, "DESCRIBE") {
				return 200, describeBodyMs(400)
			}
			return waitForCancel(ctx)
		}}
		text := describeCPU(t, fake, 200*time.Millisecond)
		wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", fullStatsSQL)
		nc := "not computed (timed out after 200ms)"
		if want := statsBlock(nc, nc, nc); !strings.HasSuffix(text, want) {
			t.Errorf("want suffix %q\ngot:\n%s", want, text)
		}
	})
	t.Run("stale fallback keeps what was computed", func(t *testing.T) {
		fake := &fakeArc{replyCtx: func(ctx context.Context, sql string) (int, string) {
			switch sql {
			case "DESCRIBE SELECT * FROM cpu":
				return 200, describeBodyMs(30)
			case latestSQL:
				return waitForCancel(ctx)
			}
			return 200, statsRowBody(`20000000`, `"2023-06-01T00:00:00Z"`, `null`)
		}}
		text := describeCPU(t, fake, 200*time.Millisecond)
		wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", foldedStatsSQL, latestSQL)
		if want := statsBlock("20000000", "2023-06-01T00:00:00Z", "not computed (timed out after 200ms)"); !strings.HasSuffix(text, want) {
			t.Errorf("want suffix %q\ngot:\n%s", want, text)
		}
	})
}

// The deadline covers all statistics statements together, not each one: a
// folded statement that uses most of it leaves the fallback only the rest.
// Timed inside the fake, from the folded statement's arrival to the
// fallback's cancellation, so session setup does not count.
func TestDescribe_StatsDeadlineIsShared(t *testing.T) {
	const timeout = 400 * time.Millisecond
	foldAt, cancelledAt := make(chan time.Time, 1), make(chan time.Time, 1)
	fake := &fakeArc{replyCtx: func(ctx context.Context, sql string) (int, string) {
		switch sql {
		case "DESCRIBE SELECT * FROM cpu":
			return 200, describeBodyMs(30)
		case latestSQL:
			status, body := waitForCancel(ctx)
			cancelledAt <- time.Now()
			return status, body
		}
		foldAt <- time.Now()
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
		}
		return 200, statsRowBody(`20000000`, `"2023-06-01T00:00:00Z"`, `null`)
	}}
	text := describeCPU(t, fake, timeout)
	wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", foldedStatsSQL, latestSQL)
	if !strings.Contains(text, "- **Row count:** 20000000\n") || !strings.Contains(text, "- **Latest:** not computed (timed out after 400ms)\n") {
		t.Fatalf("unexpected block:\n%s", text)
	}
	// Shared: about 400ms in all. Per statement it would be 300ms + 400ms.
	if d := (<-cancelledAt).Sub(<-foldAt); d >= 600*time.Millisecond {
		t.Errorf("statistics ran %v; the fallback got its own --timeout instead of what was left", d)
	}
}

// Arc's own query timeout (504) is said as such, without the query tool's
// advice to aggregate.
func TestDescribe_StatsArcTimeout(t *testing.T) {
	fake := &fakeArc{reply: func(sql string) (int, string) {
		if strings.HasPrefix(sql, "DESCRIBE") {
			return 200, describeBodyMs(400)
		}
		return 504, `{"success":false,"error":"query timeout"}`
	}}
	text := describeCPU(t, fake, 5*time.Second)
	nc := "not computed (Arc timed out)"
	if want := statsBlock(nc, nc, nc); !strings.HasSuffix(text, want) {
		t.Errorf("want suffix %q\ngot:\n%s", want, text)
	}
}

// A governance refusal (429) carries Arc's reason to every line it costs,
// and stops the statistics: a refused query counts against nothing, but the
// next one would be refused the same way.
func TestDescribe_StatsRefusedByGovernance(t *testing.T) {
	const refusal = `{"success":false,"error":"Rate limit exceeded: 10 queries per minute","execution_time_ms":0,"timestamp":"2026-11-27T03:00:00Z"}`
	nc := "not computed: Arc refused the request: Rate limit exceeded: 10 queries per minute"
	tests := []struct {
		name       string
		describeMs int
		refuse     string // the statement Arc refuses
		wantSent   []string
		want       string
	}{
		{"folded", 30, foldedStatsSQL, []string{foldedStatsSQL}, statsBlock(nc, nc, nc)},
		{"full", 400, fullStatsSQL, []string{fullStatsSQL}, statsBlock(nc, nc, nc)},
		{"stale fallback", 30, latestSQL, []string{foldedStatsSQL, latestSQL}, statsBlock("20000000", "2023-06-01T00:00:00Z", nc)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArc{reply: func(sql string) (int, string) {
				switch sql {
				case "DESCRIBE SELECT * FROM cpu":
					return 200, describeBodyMs(tt.describeMs)
				case tt.refuse:
					return 429, refusal
				}
				return 200, statsRowBody(`20000000`, `"2023-06-01T00:00:00Z"`, `null`)
			}}
			text := describeCPU(t, fake, 5*time.Second)
			wantSent(t, fake, append([]string{"DESCRIBE SELECT * FROM cpu"}, tt.wantSent...)...)
			if !strings.HasSuffix(text, tt.want) {
				t.Errorf("want suffix %q\ngot:\n%s", tt.want, text)
			}
		})
	}
}

// The query tool answers a governance refusal with Arc's reason too, rather
// than pointing the model at server logs it cannot read.
func TestQuery_GovernanceRefusalReason(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		return 429, `{"success":false,"error":"Daily query quota exceeded","execution_time_ms":0}`
	}}
	cs := dataSession(t, fake, 500, 50_000)
	text, isErr := call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT 1"})
	if !isErr || text != "Arc refused the request: Daily query quota exceeded" {
		t.Errorf("isErr=%v text=%q", isErr, text)
	}
}

// DESCRIBE refused: the LIMIT 0 fallback's time gates the statistics the
// same way.
func TestDescribe_FallbackColumnsTimeGates(t *testing.T) {
	tests := []struct {
		ms      int
		wantSQL string
	}{
		{30, foldedStatsSQL},
		{400, fullStatsSQL},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.ms, "ms"), func(t *testing.T) {
			fake := &fakeArc{reply: func(sql string) (int, string) {
				switch {
				case strings.HasPrefix(sql, "DESCRIBE"):
					return 400, `{"success":false,"error":"Dangerous SQL operation not allowed"}`
				case strings.HasSuffix(sql, "LIMIT 0"):
					return 200, fmt.Sprintf(`{"success":true,"columns":["time"],"data":[],"row_count":0,"execution_time_ms":%d}`, tt.ms)
				}
				return 200, statsRowBody(`7`, `"2026-11-01T00:00:00Z"`, `"2026-11-26T00:00:00Z"`)
			}}
			describeCPU(t, fake, 5*time.Second)
			wantSent(t, fake, "DESCRIBE SELECT * FROM cpu", "SELECT * FROM cpu LIMIT 0", tt.wantSQL)
		})
	}
}

// Out of memory on a statistics statement: said as such, without the query
// tool's advice to aggregate, which is for SQL the model wrote. Any other
// 500 keeps the query path's text.
func TestDescribe_StatsOutOfMemory(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"out of memory", `{"success":false,"error":"arrow query failed: Out of Memory Error: failed to allocate data of size 64.0 MiB (3.7 GiB/3.7 GiB used)"}`,
			"not computed (Arc ran out of memory)"},
		{"other DuckDB error", `{"success":false,"error":"Binder Error: Referenced column \"time\" not found"}`,
			`not computed: The query failed: Binder Error: Referenced column "time" not found`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArc{reply: func(sql string) (int, string) {
				if strings.HasPrefix(sql, "DESCRIBE") {
					return 200, describeBodyMs(30)
				}
				return 500, tt.body
			}}
			text := describeCPU(t, fake, 5*time.Second)
			if want := statsBlock(tt.want, tt.want, tt.want); !strings.HasSuffix(text, want) {
				t.Errorf("want suffix %q\ngot:\n%s", want, text)
			}
		})
	}
}

// A timeout with no known deadline (--timeout 0 and no caller deadline; a
// transport timeout) names no duration.
func TestFailureReason_TimeoutWithoutBudget(t *testing.T) {
	err := &arc.Error{Kind: arc.KindTimeout, Detail: "query: dial tcp: i/o timeout"}
	if got := failureReason(0, err); got != "timed out" {
		t.Errorf("failureReason(0) = %q, want %q", got, "timed out")
	}
	if got := failureReason(2*time.Second, err); got != "timed out after 2s" {
		t.Errorf("failureReason(2s) = %q", got)
	}
}

// The DESCRIBE itself running out of time or memory: the same short reason,
// not the query tool's advice.
func TestDescribe_DescribeFailureReason(t *testing.T) {
	tests := []struct {
		name  string
		reply func(ctx context.Context) (int, string)
		want  string
	}{
		{"arc-mcp timeout", waitForCancel, "Could not describe the measurement: timed out after 200ms."},
		{"Arc timeout", func(context.Context) (int, string) { return 504, `{"success":false,"error":"query timeout"}` },
			"Could not describe the measurement: Arc timed out."},
		{"out of memory", func(context.Context) (int, string) {
			return 500, `{"success":false,"error":"Out of Memory Error: failed to allocate"}`
		}, "Could not describe the measurement: Arc ran out of memory."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArc{replyCtx: func(ctx context.Context, _ string) (int, string) { return tt.reply(ctx) }}
			cs := newSessionTimeout(t, fake, 200*time.Millisecond, func(server *mcp.Server, client *arc.Client) {
				RegisterDescribeMeasurement(server, client, 50_000)
			})
			text, isErr := call(t, cs, "describe_measurement", map[string]any{"database": "db", "measurement": "cpu"})
			if !isErr || text != tt.want {
				t.Errorf("isErr=%v text=%q, want %q", isErr, text, tt.want)
			}
			if n := len(fake.received()); n != 1 {
				t.Errorf("%d statements sent, want 1", n)
			}
		})
	}
}

// A caller deadline sooner than --timeout: the text names the time the
// statistics actually had.
func TestDescribe_StatsTimeoutNamesTheDeadlineThatFired(t *testing.T) {
	fake := &fakeArc{replyCtx: func(ctx context.Context, _ string) (int, string) { return waitForCancel(ctx) }}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client := arc.NewClient(srv.URL, "tok", 5*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	got := measurementStats(ctx, client, "db", "cpu", 30)
	// The budget is read a moment after the deadline was set: on a slow or
	// coarse clock (Windows CI) that is 149ms, never more than 150ms, and
	// far from --timeout's 5s.
	want := regexp.MustCompile(`^not computed \(timed out after (1[34][0-9]|150)ms\)$`)
	for i, v := range got {
		if !want.MatchString(v.String()) {
			t.Errorf("line %d = %q", i, v)
		}
	}
}
