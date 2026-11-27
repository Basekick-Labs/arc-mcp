package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeHost is the Arc host the ops tests give arc-mcp. It resolves nowhere:
// the client's dialer routes every connection to the fake. No answer may
// carry it, nor the address the fake listens on.
const fakeHost = "arc-ops-fake-7c1e.example"

// Synthetic Arc answers, shaped like Arc's but written by hand.
const (
	healthBody   = `{"status":"ok","time":"2026-11-27T04:30:43Z","uptime":"1h0m0.5s","uptime_sec":3600.5}`
	readyBody    = `{"status":"ready","time":"2026-11-27T04:30:43Z","uptime_sec":3600.5}`
	notReadyBody = `{"reason":"server is starting up or shutting down; load balancer should not route traffic here","status":"not_ready","time":"2026-11-27T04:30:43Z"}`
	// Integers past 2^53 and a decimal float64 cannot hold: both must reach
	// the model as Arc wrote them.
	countersBody  = `{"ingest_bytes_total":9007199254740993,"query_avg_ms":0.1000000000000000055511151231257827,"uptime_seconds":3600}`
	endpointsBody = `{"http":{"requests_total":1200,"errors_total":3},"ingest":{"records_total":50000}}`
	poolBody      = `{"pool":{"open":4,"in_use":1,"idle":3}}`
	memoryBody    = `{"heap":{"alloc_bytes":104857600},"gc":{"num_gc":12}}`
	proxyPage     = `<html><body><h1>502 Bad Gateway</h1></body></html>`
)

// opsRoute is the fake Arc's answer for one path.
type opsRoute struct {
	status int
	body   string
	hang   bool          // wait for the client to give up instead of answering
	delay  time.Duration // answer this late, or not at all if the client gives up first
}

// opsArc answers each path from routes, 404 for any other, and records the
// request URI (path and query) of every request.
type opsArc struct {
	mu     sync.Mutex
	routes map[string]opsRoute
	seen   []string
}

func (f *opsArc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.URL.RequestURI())
	route, ok := f.routes[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		route = opsRoute{status: http.StatusNotFound, body: `{"error":"Cannot GET ` + r.URL.Path + `"}`}
	}
	if route.hang {
		<-r.Context().Done()
		return
	}
	if route.delay > 0 {
		select {
		case <-time.After(route.delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_, _ = io.WriteString(w, route.body)
}

func (f *opsArc) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// healthyArc answers both probes and every metrics family.
func healthyArc() *opsArc {
	return &opsArc{routes: map[string]opsRoute{
		"/health":                                {status: 200, body: healthBody},
		"/ready":                                 {status: 200, body: readyBody},
		"/api/v1/metrics":                        {status: 200, body: countersBody},
		"/api/v1/metrics/endpoints":              {status: 200, body: endpointsBody},
		"/api/v1/metrics/query-pool":             {status: 200, body: poolBody},
		"/api/v1/metrics/memory":                 {status: 200, body: memoryBody},
		"/api/v1/metrics/timeseries/system":      {status: 200, body: historyBody(8)},
		"/api/v1/metrics/timeseries/api":         {status: 200, body: historyBody(8)},
		"/api/v1/metrics/timeseries/application": {status: 200, body: historyBody(8)},
	}}
}

// syncBuffer collects what the server's logging transport writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type opsOpts struct {
	baseURL   string        // default "http://" + fakeHost + ":8000"
	timeout   time.Duration // default 5s
	maxChars  int           // default 50,000
	unlimited bool          // --max-response-size 0
	down      bool          // nothing listens: every connection is refused
	wire      *syncBuffer   // when set, gets the server's JSON-RPC traffic
}

// opsSession registers the ops tools against arcHandler, reached as
// fakeHost, and returns a client session over a real in-memory MCP transport
// and the address that must never appear in an answer.
func opsSession(t *testing.T, arcHandler http.Handler, o opsOpts) (*mcp.ClientSession, string) {
	t.Helper()
	srv := httptest.NewServer(arcHandler)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	if o.down {
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = l.Addr().String()
		_ = l.Close()
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	base := o.baseURL
	if base == "" {
		base = "http://" + fakeHost + ":8000"
	}
	timeout := o.timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	maxChars := o.maxChars
	if maxChars == 0 {
		maxChars = 50_000
	}
	if o.unlimited {
		maxChars = 0
	}
	client := arc.NewClientWithTransport(base, "tok", timeout, transport)
	server := mcp.NewServer(&mcp.Implementation{Name: "arc-mcp-test", Version: "test"}, nil)
	RegisterAll(server, client, Groups{Ops: true}, 500, maxChars)

	serverT, clientT := mcp.NewInMemoryTransports()
	var st mcp.Transport = serverT
	if o.wire != nil {
		st = &mcp.LoggingTransport{Transport: serverT, Writer: o.wire}
	}
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil).Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, addr
}

// callOps calls an ops tool and checks every answer, success or error, for
// Arc's address (the fake listens on loopback) and for the raw-error
// fall-through of arc.UserMessage.
func callOps(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, *mcp.CallToolResult) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s, %v): %v", tool, args, err)
	}
	text := resultText(t, res)
	assertNoLeak(t, text, "127.0.0.1")
	assertNoLeak(t, text, "[::1]")
	return text, res
}

// assertNoLeak fails when text carries Arc's host or addr, or the raw-error
// fall-through of arc.UserMessage.
func assertNoLeak(t *testing.T, text, addr string) {
	t.Helper()
	for _, leak := range []string{fakeHost, addr, "Arc error:", "dial tcp"} {
		if strings.Contains(text, leak) {
			t.Errorf("answer carries %q:\n%s", leak, text)
		}
	}
}

// historyBody is an Arc time-series answer with n points 5 s apart, oldest
// first, timestamps in a -03:00 offset with nanoseconds as Arc writes them.
func historyBody(n int) string {
	zone := time.FixedZone("", -3*3600)
	start := time.Date(2026, 11, 27, 1, 0, 0, 0, zone)
	points := make([]string, n)
	for i := range points {
		ts := start.Add(time.Duration(i)*5*time.Second + time.Duration(i%7)*123456789).Format(time.RFC3339Nano)
		points[i] = fmt.Sprintf(`{"timestamp":%q,"values":{"cpu_percent":%d.25,"requests_total":%d,"label":"p%d"}}`, ts, i%100, 1_000_000+i*37, i)
	}
	data := "null"
	if n > 0 {
		data = "[" + strings.Join(points, ",") + "]"
	}
	return fmt.Sprintf(`{"timestamp":"2026-11-27T04:30:43Z","type":"api","duration_minutes":10,"points_count":%d,"data":%s}`, n, data)
}

// decodeHistory reads an arc_metrics history answer back.
func decodeHistory(t *testing.T, text string) HistoryResult {
	t.Helper()
	var res struct {
		Family  string        `json:"family"`
		History HistoryResult `json:"history"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("history answer is not JSON: %v\n%s", err, text)
	}
	return res.History
}

// pointLabels is the "label" value of each point, in order.
func pointLabels(t *testing.T, h HistoryResult) []string {
	t.Helper()
	labels := make([]string, len(h.Points))
	for i, p := range h.Points {
		labels[i] = p.(map[string]any)["values"].(map[string]any)["label"].(string)
	}
	return labels
}

func toolNames(t *testing.T, groups Groups) []string {
	t.Helper()
	cs := newSession(t, http.NotFoundHandler(), func(server *mcp.Server, client *arc.Client) {
		RegisterAll(server, client, groups, 500, 50_000)
	})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestRegisterAll_Groups(t *testing.T) {
	data := "describe_measurement,get_sample_data,list_databases,list_measurements,query"
	for _, tt := range []struct {
		groups Groups
		want   string
	}{
		{Groups{Data: true}, data},
		{Groups{Ops: true}, "arc_health,arc_metrics"},
		{Groups{Data: true, Ops: true}, "arc_health,arc_metrics," + data},
	} {
		if got := strings.Join(toolNames(t, tt.groups), ","); got != tt.want {
			t.Errorf("%s: tools %s, want %s", tt.groups, got, tt.want)
		}
	}
}

// The default group lists exactly the tools arc-mcp listed before groups
// existed, byte for byte: testdata/tools-list-data.json is c5f3fd5's
// tools/list, rendered the same way. Turning ops on adds two tools and
// changes none of the five.
func TestRegisterAll_DefaultToolsListIsUnchanged(t *testing.T) {
	want, err := os.ReadFile("testdata/tools-list-data.json")
	if err != nil {
		t.Fatal(err)
	}
	list := func(groups Groups) []*mcp.Tool {
		cs := newSession(t, http.NotFoundHandler(), func(server *mcp.Server, client *arc.Client) {
			RegisterAll(server, client, groups, 500, 50_000)
		})
		res, err := cs.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Tools
	}
	render := func(tools []*mcp.Tool) string {
		b, err := json.MarshalIndent(tools, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	if got := render(list(Groups{Data: true})); got != string(want) {
		t.Errorf("default tools/list changed:\n%s", got)
	}
	var data []*mcp.Tool
	for _, tool := range list(Groups{Data: true, Ops: true}) {
		if !strings.HasPrefix(tool.Name, "arc_") {
			data = append(data, tool)
		}
	}
	if got := render(data); got != string(want) {
		t.Errorf("with ops on, the data tools changed:\n%s", got)
	}
}

// The family list is an enum in the schema, so a client shows the choices
// and the SDK refuses anything else; minutes has no default, which the SDK
// would apply to snapshot families too.
func TestMetrics_InputSchema(t *testing.T) {
	cs, _ := opsSession(t, healthyArc(), opsOpts{})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	for _, tool := range res.Tools {
		if tool.Name == "arc_metrics" {
			schema, _ = tool.InputSchema.(map[string]any)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	family, _ := props["family"].(map[string]any)
	enum, _ := json.Marshal(family["enum"])
	if want := `["endpoints","query_pool","memory","counters","system_history","application_history","api_history"]`; string(enum) != want {
		t.Errorf("family enum = %s, want %s", enum, want)
	}
	minutes, _ := props["minutes"].(map[string]any)
	if _, ok := minutes["default"]; ok || minutes["type"] != "integer" {
		t.Errorf("minutes schema = %v: an integer, with no default", minutes)
	}
	if req, _ := json.Marshal(schema["required"]); string(req) != `["family"]` || schema["additionalProperties"] != false {
		t.Errorf("schema = %v", schema)
	}
}

// Each family reads exactly its own route; history families send the
// look-back, defaulting to 10 minutes, and snapshot families send nothing.
func TestMetrics_EachFamilyReadsItsRoute(t *testing.T) {
	tests := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"family": "endpoints"}, "/api/v1/metrics/endpoints"},
		{map[string]any{"family": "query_pool"}, "/api/v1/metrics/query-pool"},
		{map[string]any{"family": "memory"}, "/api/v1/metrics/memory"},
		{map[string]any{"family": "counters"}, "/api/v1/metrics"},
		{map[string]any{"family": "counters", "minutes": 0}, "/api/v1/metrics"},
		{map[string]any{"family": "system_history"}, "/api/v1/metrics/timeseries/system?duration_minutes=10"},
		{map[string]any{"family": "application_history", "minutes": 1}, "/api/v1/metrics/timeseries/application?duration_minutes=1"},
		{map[string]any{"family": "api_history", "minutes": 1440}, "/api/v1/metrics/timeseries/api?duration_minutes=1440"},
	}
	for _, tt := range tests {
		fake := healthyArc()
		cs, addr := opsSession(t, fake, opsOpts{})
		text, res := callOps(t, cs, "arc_metrics", tt.args)
		if res.IsError {
			t.Errorf("%v: error %q", tt.args, text)
		}
		if got := strings.Join(fake.requests(), " "); got != tt.want {
			t.Errorf("%v: requested %q, want %q", tt.args, got, tt.want)
		}
		if family := tt.args["family"].(string); !strings.HasPrefix(text, `{"family":"`+family+`",`) {
			t.Errorf("%v: answer %s", tt.args, text)
		}
		assertNoLeak(t, text, addr)
	}
}

// Arguments the tool cannot use are refused before anything reaches Arc,
// labelled as arc-mcp's own refusal, never as Arc's.
func TestMetrics_ArgumentsRefusedBeforeArc(t *testing.T) {
	tests := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"family": "memory", "minutes": 5}, "Invalid argument: minutes applies only to system_history, application_history and api_history"},
		{map[string]any{"family": "api_history", "minutes": -1}, "Invalid argument: minutes must be between 1 and 1440"},
		{map[string]any{"family": "api_history", "minutes": 1441}, "Invalid argument: minutes must be between 1 and 1440"},
		// The SDK's schema check, before the handler.
		{map[string]any{"family": "bogus"}, `enum: bogus does not equal any of: [endpoints query_pool memory counters system_history application_history api_history]`},
		{map[string]any{"family": "COUNTERS"}, "enum: COUNTERS does not equal any of"},
		{map[string]any{}, `missing properties: ["family"]`},
		{map[string]any{"family": "api_history", "minutes": 2.5}, `want "integer"`},
		{map[string]any{"family": "api_history", "minutez": 5}, `unexpected additional properties ["minutez"]`},
	}
	for _, tt := range tests {
		fake := healthyArc()
		cs, _ := opsSession(t, fake, opsOpts{})
		text, res := callOps(t, cs, "arc_metrics", tt.args)
		if !res.IsError || !strings.Contains(text, tt.want) || strings.Contains(text, "Arc error") {
			t.Errorf("%v: isErr=%v %q, want an error containing %q", tt.args, res.IsError, text, tt.want)
		}
		if got := fake.requests(); len(got) != 0 {
			t.Errorf("%v: sent %v to Arc", tt.args, got)
		}
		if res.StructuredContent != nil {
			t.Errorf("%v: an error carries structured content", tt.args)
		}
	}
}

// Arc's refusals get the shared texts where those fit, and texts of their
// own where the shared ones talk about databases or queries. Statuses here
// are synthetic.
func TestMetrics_ArcErrors(t *testing.T) {
	tests := []struct {
		name   string
		route  opsRoute
		want   string
		reject []string
	}{
		{"404 is a wrong URL, not a missing database", opsRoute{status: 404, body: `{"error":"Cannot GET /api/v1/metrics/memory"}`},
			"Arc answered HTTP 404 for the memory metrics", []string{"database", "measurement"}},
		{"401", opsRoute{status: 401, body: `{}`},
			"Arc rejected the token", nil},
		{"403", opsRoute{status: 403, body: `{}`},
			"does not carry the permission the endpoint requires", nil},
		{"403 shaped like a per-database refusal", opsRoute{status: 403, body: `{"error":"no permission for read on database 'x'"}`},
			"does not carry the permission the endpoint requires", []string{"specific databases", "database"}},
		{"429 carries Arc's reason", opsRoute{status: 429, body: `{"error":"Rate limit exceeded: 10 queries per minute"}`},
			"Arc refused the request: Rate limit exceeded", nil},
		{"500 stays generic", opsRoute{status: 500, body: `{"error":"open /var/lib/arc/secret: permission denied"}`},
			"Arc returned an error — see server logs", []string{"/var/lib"}},
		{"502 from a proxy", opsRoute{status: 502, body: proxyPage},
			"Arc returned an error — see server logs", []string{"<html>"}},
		{"200 that is not JSON", opsRoute{status: 200, body: proxyPage},
			"Arc returned an unexpected response format", []string{"<html>"}},
		{"200 that is not an object", opsRoute{status: 200, body: `[1,2,3]`},
			"Arc returned an unexpected response format", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &opsArc{routes: map[string]opsRoute{"/api/v1/metrics/memory": tt.route}}
			cs, addr := opsSession(t, fake, opsOpts{})
			text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "memory"})
			if !res.IsError || !strings.Contains(text, tt.want) {
				t.Errorf("isErr=%v %q, want %q", res.IsError, text, tt.want)
			}
			for _, r := range tt.reject {
				if strings.Contains(text, r) {
					t.Errorf("answer says %q: %s", r, text)
				}
			}
			assertNoLeak(t, text, addr)
		})
	}
}

// A history answer that is not Arc's shape is refused like any other.
func TestMetrics_HistoryNotArcsShape(t *testing.T) {
	for _, body := range []string{`{"points_count":1}`, `{"data":{"a":1}}`, `[]`} {
		fake := &opsArc{routes: map[string]opsRoute{"/api/v1/metrics/timeseries/api": {status: 200, body: body}}}
		cs, _ := opsSession(t, fake, opsOpts{})
		text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "api_history"})
		if !res.IsError || !strings.Contains(text, "unexpected response format") {
			t.Errorf("%s: isErr=%v %q", body, res.IsError, text)
		}
	}
}

// Running out of time is about Arc, not about a query to rewrite; neither
// it nor an Arc that cannot be reached says where Arc is.
func TestMetrics_TimeoutAndArcDown(t *testing.T) {
	fake := &opsArc{routes: map[string]opsRoute{"/api/v1/metrics/endpoints": {hang: true}}}
	cs, addr := opsSession(t, fake, opsOpts{timeout: 200 * time.Millisecond})
	text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "endpoints"})
	if !res.IsError || !strings.Contains(text, "Arc did not answer within 200ms (arc-mcp's --timeout)") {
		t.Errorf("timeout: isErr=%v %q", res.IsError, text)
	}
	for _, advice := range []string{"time_bucket", "query", "time range"} {
		if strings.Contains(text, advice) {
			t.Errorf("timeout answer gives query advice %q: %s", advice, text)
		}
	}
	assertNoLeak(t, text, addr)

	cs, addr = opsSession(t, healthyArc(), opsOpts{down: true})
	for _, family := range []string{"memory", "api_history"} {
		text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": family})
		if !res.IsError || !strings.Contains(text, "Arc could not be reached") {
			t.Errorf("%s with Arc down: isErr=%v %q", family, res.IsError, text)
		}
		assertNoLeak(t, text, addr)
	}
}

// Arc's numbers reach the model as Arc wrote them, in the text and in the
// structured content on the wire (a Go client would decode the latter to
// float64 itself; that is the client's choice, not what is sent).
func TestMetrics_NumbersStayExact(t *testing.T) {
	wire := &syncBuffer{}
	cs, _ := opsSession(t, healthyArc(), opsOpts{wire: wire})
	text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "counters"})
	if res.IsError {
		t.Fatal(text)
	}
	if want := `{"family":"counters","response":` + countersBody + `}`; text != want {
		t.Errorf("text = %s\nwant %s", text, want)
	}
	// The logging transport writes its line after delivering the message,
	// so the call can return first.
	var sent string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if sent = wire.String(); strings.Contains(sent, `"structuredContent"`) {
			break
		}
	}
	for _, n := range []string{`"ingest_bytes_total":9007199254740993`, `"query_avg_ms":0.1000000000000000055511151231257827`} {
		if !strings.Contains(sent, `"structuredContent":{"family":"counters","response":{`) || strings.Count(sent, n) < 1 {
			t.Errorf("structured content on the wire lacks %s:\n%s", n, sent)
		}
	}
}

// The configured URL may end in a slash or carry a path prefix (a proxy
// serving Arc under /arc): routes are appended to it, once.
func TestOps_BaseURLWithSlashOrPrefix(t *testing.T) {
	for _, tt := range []struct{ base, prefix string }{
		{"http://" + fakeHost + ":8000/", ""},
		{"https://" + fakeHost + "/arc", "/arc"},
		{"https://" + fakeHost + "/arc/", "/arc"},
	} {
		fake := &opsArc{routes: map[string]opsRoute{
			tt.prefix + "/health":                        {status: 200, body: healthBody},
			tt.prefix + "/ready":                         {status: 200, body: readyBody},
			tt.prefix + "/api/v1/metrics/memory":         {status: 200, body: memoryBody},
			tt.prefix + "/api/v1/metrics/timeseries/api": {status: 200, body: historyBody(3)},
		}}
		base := strings.Replace(tt.base, "https://", "http://", 1) // the fake speaks plain HTTP
		cs, _ := opsSession(t, fake, opsOpts{baseURL: base})
		for _, call := range []struct {
			tool string
			args map[string]any
		}{{"arc_health", nil}, {"arc_metrics", map[string]any{"family": "memory"}}, {"arc_metrics", map[string]any{"family": "api_history"}}} {
			if text, res := callOps(t, cs, call.tool, call.args); res.IsError || strings.Contains(text, `"error"`) {
				t.Errorf("%s %s %v: %s", tt.base, call.tool, call.args, text)
			}
		}
		want := tt.prefix + "/health " + tt.prefix + "/ready " + tt.prefix + "/api/v1/metrics/memory " + tt.prefix + "/api/v1/metrics/timeseries/api?duration_minutes=10"
		if got := strings.Join(fake.requests(), " "); got != want {
			t.Errorf("%s: requested %q, want %q", tt.base, got, want)
		}
	}
}

// A snapshot that does not fit --max-response-size is refused whole, naming
// the setting, rather than cut into invalid JSON; 0 means no limit.
func TestMetrics_SnapshotAndTheSizeCap(t *testing.T) {
	cs, _ := opsSession(t, healthyArc(), opsOpts{maxChars: 60})
	text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "counters"})
	if !res.IsError || !strings.Contains(text, "Arc's counters answer does not fit in arc-mcp's --max-response-size (60 characters)") {
		t.Errorf("isErr=%v %q", res.IsError, text)
	}

	big := map[string]any{}
	for i := range 5000 {
		big[fmt.Sprintf("counter_%04d_total", i)] = i
	}
	body, _ := json.Marshal(big)
	fake := &opsArc{routes: map[string]opsRoute{"/api/v1/metrics": {status: 200, body: string(body)}}}
	cs, _ = opsSession(t, fake, opsOpts{unlimited: true})
	text, res = callOps(t, cs, "arc_metrics", map[string]any{"family": "counters"})
	if res.IsError || len(text) < len(body) || !json.Valid([]byte(text)) {
		t.Errorf("unlimited: isErr=%v, %d characters for a %d-byte answer", res.IsError, len(text), len(body))
	}
}

// History comes back sampled from the newest point: every k-th point
// counting back, the newest always kept, k the smallest step that leaves at
// most historyPointBudget points; from/to are the kept points' own
// timestamps, as Arc wrote them.
func TestHistory_SampledFromTheNewest(t *testing.T) {
	for _, n := range []int{1, 7, historyPointBudget, historyPointBudget + 1, 120, 360, 1801} {
		fake := &opsArc{routes: map[string]opsRoute{"/api/v1/metrics/timeseries/system": {status: 200, body: historyBody(n)}}}
		cs, addr := opsSession(t, fake, opsOpts{unlimited: true})
		text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "system_history", "minutes": 1440})
		if res.IsError {
			t.Fatalf("n=%d: %s", n, text)
		}
		assertNoLeak(t, text, addr)
		h := decodeHistory(t, text)
		k := (n + historyPointBudget - 1) / historyPointBudget
		if h.TotalPoints != n || h.SampledEvery != k {
			t.Errorf("n=%d: total %d, sampled_every %d; want %d, %d", n, h.TotalPoints, h.SampledEvery, n, k)
		}
		labels := pointLabels(t, h)
		if len(labels) > historyPointBudget {
			t.Errorf("n=%d: %d points, over the budget of %d", n, len(labels), historyPointBudget)
		}
		for i, label := range labels {
			// Kept point i, counting from the end, is Arc's point
			// n-1-(len-1-i)*k: the newest, then every k-th before it.
			if want := fmt.Sprintf("p%d", n-1-(len(labels)-1-i)*k); label != want {
				t.Errorf("n=%d: point %d is %s, want %s", n, i, label, want)
				break
			}
		}
		first := h.Points[0].(map[string]any)["timestamp"]
		last := h.Points[len(h.Points)-1].(map[string]any)["timestamp"]
		if h.From != first || h.To != last {
			t.Errorf("n=%d: from/to %s/%s, want the kept points' %v/%v", n, h.From, h.To, first, last)
		}
	}
}

// A window with no points yet (Arc sends null) is an answer, not an error.
func TestHistory_EmptyWindow(t *testing.T) {
	fake := &opsArc{routes: map[string]opsRoute{"/api/v1/metrics/timeseries/api": {status: 200, body: historyBody(0)}}}
	cs, _ := opsSession(t, fake, opsOpts{})
	text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "api_history"})
	if want := `{"family":"api_history","history":{"total_points":0,"sampled_every":1,"points":[]}}`; res.IsError || text != want {
		t.Errorf("isErr=%v %s, want %s", res.IsError, text, want)
	}
}

// exactPoints decodes historyBody(n)'s points the way GetJSON does.
func exactPoints(t testing.TB, n int) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(historyBody(n)))
	dec.UseNumber()
	var env map[string]any
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	points, _ := env["data"].([]any)
	return points
}

// renderEvery is the answer keeping every k-th of points from the newest,
// built independently of sampleHistory, as the oracle for its size search.
func renderEvery(t *testing.T, points []any, k int) string {
	t.Helper()
	n := len(points)
	var kept []any
	for i := (n - 1) % k; i < n; i += k {
		kept = append(kept, points[i])
	}
	ts := func(p any) string { return p.(map[string]any)["timestamp"].(string) }
	text, err := compactJSON(MetricsResult{Family: "api_history", History: &HistoryResult{
		From: ts(kept[0]), To: ts(kept[len(kept)-1]), TotalPoints: n, SampledEvery: k, Points: kept,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// Under --max-response-size the answer never goes over, and it is as full
// as the cap allows: the next smaller step would not have fitted.
func TestHistory_CapHoldsAndFills(t *testing.T) {
	for _, n := range []int{1, 2, 20, 21, 361} {
		points := exactPoints(t, n)
		base := (n + historyPointBudget - 1) / historyPointBudget
		tried := 0
		for maxChars := 100; maxChars <= 12_000; maxChars += 7 {
			res, text, err := sampleHistory("api_history", points, maxChars)
			if err != nil {
				if !strings.Contains(err.Error(), "does not fit") {
					t.Fatalf("n=%d cap=%d: %v", n, maxChars, err)
				}
				// Not even the newest point alone fits.
				if one := renderEvery(t, points, n); len(one) <= maxChars {
					t.Errorf("n=%d cap=%d: refused, but the newest point alone is %d characters", n, maxChars, len(one))
				}
				continue
			}
			tried++
			k := res.History.SampledEvery
			if len(text) > maxChars {
				t.Errorf("n=%d cap=%d: %d characters", n, maxChars, len(text))
			}
			if text != renderEvery(t, points, k) {
				t.Errorf("n=%d cap=%d: answer is not every %d-th point from the newest", n, maxChars, k)
			}
			if k > base && k > 1 {
				if prev := renderEvery(t, points, k-1); len(prev) <= maxChars {
					t.Errorf("n=%d cap=%d: chose k=%d, but k=%d fits (%d characters)", n, maxChars, k, k-1, len(prev))
				}
			}
		}
		if tried == 0 {
			t.Errorf("n=%d: no cap in the range produced an answer", n)
		}
	}
}

// Through the tool: the final text never exceeds the cap, and a cap below
// one point is refused naming the setting.
func TestHistory_CapThroughTheTool(t *testing.T) {
	for _, maxChars := range []int{120, 400, 2000, 5000} {
		fake := &opsArc{routes: map[string]opsRoute{"/api/v1/metrics/timeseries/application": {status: 200, body: historyBody(360)}}}
		cs, _ := opsSession(t, fake, opsOpts{maxChars: maxChars})
		text, res := callOps(t, cs, "arc_metrics", map[string]any{"family": "application_history", "minutes": 30})
		switch {
		case res.IsError:
			if !strings.Contains(text, "Even the newest point of Arc's application_history answer does not fit in arc-mcp's --max-response-size") {
				t.Errorf("cap=%d: %s", maxChars, text)
			}
		case len(text) > maxChars:
			t.Errorf("cap=%d: %d characters", maxChars, len(text))
		default:
			if h := decodeHistory(t, text); pointLabels(t, h)[len(h.Points)-1] != "p359" {
				t.Errorf("cap=%d: the newest point was dropped", maxChars)
			}
		}
	}
}

func TestHealth_HealthyAndReady(t *testing.T) {
	fake := healthyArc()
	cs, addr := opsSession(t, fake, opsOpts{})
	text, res := callOps(t, cs, "arc_health", nil)
	want := `{"health":{"http_status":200,"body":` + healthBody + `},"ready":{"http_status":200,"body":` + readyBody + `}}`
	if res.IsError || text != want {
		t.Errorf("isErr=%v\n got %s\nwant %s", res.IsError, text, want)
	}
	if res.StructuredContent == nil {
		t.Error("no structured content")
	}
	if got := strings.Join(fake.requests(), " "); got != "/health /ready" {
		t.Errorf("probed %q", got)
	}
	assertNoLeak(t, text, addr)
}

// Every way a probe can come back has one fixed shape; only Arc's own JSON
// answer (200 or 503) is shown, and none of them is a tool error once
// /health has answered.
func TestHealth_ReadyStates(t *testing.T) {
	tests := []struct {
		name  string
		ready opsRoute
		want  string
	}{
		{"503 is an answer", opsRoute{status: 503, body: notReadyBody}, `"ready":{"http_status":503,"body":` + notReadyBody + `}`},
		{"404", opsRoute{status: 404, body: `{"error":"Cannot GET /ready"}`}, `"ready":{"http_status":404,"error":"not found: the configured URL may not be Arc's base URL"}`},
		{"502 proxy page", opsRoute{status: 502, body: proxyPage}, `"ready":{"http_status":502,"error":"server error"}`},
		{"500 with detail", opsRoute{status: 500, body: `{"error":"open /var/lib/arc/x: permission denied"}`}, `"ready":{"http_status":500,"error":"server error"}`},
		{"429", opsRoute{status: 429, body: `{"error":"slow down"}`}, `"ready":{"http_status":429,"error":"rate limited"}`},
		{"401 from a proxy", opsRoute{status: 401, body: `{}`}, `"ready":{"http_status":401,"error":"refused"}`},
		{"302", opsRoute{status: 302, body: ``}, `"ready":{"http_status":302,"error":"unexpected status"}`},
		{"200 that is not JSON", opsRoute{status: 200, body: proxyPage}, `"ready":{"http_status":200,"error":"not Arc's answer: the body is not a JSON object"}`},
		{"503 that is not JSON", opsRoute{status: 503, body: "Service Unavailable"}, `"ready":{"http_status":503,"error":"not Arc's answer: the body is not a JSON object"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &opsArc{routes: map[string]opsRoute{"/health": {status: 200, body: healthBody}, "/ready": tt.ready}}
			cs, addr := opsSession(t, fake, opsOpts{})
			text, res := callOps(t, cs, "arc_health", nil)
			if res.IsError || !strings.HasSuffix(text, tt.want+"}") {
				t.Errorf("isErr=%v\n got %s\nwant …%s}", res.IsError, text, tt.want)
			}
			for _, leak := range []string{"/var/lib", "<html>", "slow down"} {
				if strings.Contains(text, leak) {
					t.Errorf("answer shows %q", leak)
				}
			}
			assertNoLeak(t, text, addr)
		})
	}
}

// /ready running out of the shared deadline after /health answered is a
// state of that probe, not a failure of the tool.
func TestHealth_ReadyTimesOutAfterHealth(t *testing.T) {
	fake := &opsArc{routes: map[string]opsRoute{"/health": {status: 200, body: healthBody}, "/ready": {hang: true}}}
	cs, addr := opsSession(t, fake, opsOpts{timeout: 300 * time.Millisecond})
	text, res := callOps(t, cs, "arc_health", nil)
	if res.IsError || !strings.HasSuffix(text, `"ready":{"error":"timed out"}}`) {
		t.Errorf("isErr=%v %s", res.IsError, text)
	}
	assertNoLeak(t, text, addr)
}

// The two probes share one --timeout: a slow /health leaves /ready only what
// remains, so the tool never takes longer than the timeout.
func TestHealth_OneDeadlineForBothProbes(t *testing.T) {
	fake := &opsArc{routes: map[string]opsRoute{
		"/health": {status: 200, body: healthBody, delay: 500 * time.Millisecond},
		"/ready":  {status: 200, body: readyBody, delay: 500 * time.Millisecond},
	}}
	// /health answers with 300 ms of the 800 ms left, which /ready (500 ms)
	// cannot use up; with a deadline per probe, both would answer.
	cs, _ := opsSession(t, fake, opsOpts{timeout: 800 * time.Millisecond})
	start := time.Now()
	text, res := callOps(t, cs, "arc_health", nil)
	if res.IsError || !strings.HasPrefix(text, `{"health":{"http_status":200,`) || !strings.HasSuffix(text, `"ready":{"error":"timed out"}}`) {
		t.Errorf("isErr=%v %s", res.IsError, text)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s for an 800ms timeout", took)
	}
}

// /health answering anything at all is reported; only no answer is a tool
// error, and that says neither where Arc is nor anything about queries.
func TestHealth_NoAnswer(t *testing.T) {
	cs, addr := opsSession(t, healthyArc(), opsOpts{down: true})
	text, res := callOps(t, cs, "arc_health", nil)
	if !res.IsError || text != "Arc could not be reached. Ask the user to check that the Arc server is running at the configured URL." {
		t.Errorf("down: isErr=%v %q", res.IsError, text)
	}
	assertNoLeak(t, text, addr)

	fake := &opsArc{routes: map[string]opsRoute{"/health": {hang: true}, "/ready": {status: 200, body: readyBody}}}
	cs, addr = opsSession(t, fake, opsOpts{timeout: 200 * time.Millisecond})
	text, res = callOps(t, cs, "arc_health", nil)
	if !res.IsError || !strings.HasPrefix(text, "Arc did not answer /health within 200ms (arc-mcp's --timeout).") || strings.Contains(text, "time_bucket") {
		t.Errorf("hang: isErr=%v %q", res.IsError, text)
	}
	assertNoLeak(t, text, addr)
	if got := fake.requests(); len(got) != 1 {
		t.Errorf("probed %v after /health gave no answer", got)
	}

	fake = &opsArc{routes: map[string]opsRoute{"/health": {status: 502, body: proxyPage}, "/ready": {status: 502, body: proxyPage}}}
	cs, _ = opsSession(t, fake, opsOpts{})
	text, res = callOps(t, cs, "arc_health", nil)
	if want := `{"health":{"http_status":502,"error":"server error"},"ready":{"http_status":502,"error":"server error"}}`; res.IsError || text != want {
		t.Errorf("proxy: isErr=%v %s", res.IsError, text)
	}
}

// Under a small --max-response-size the statuses survive and Arc's bodies
// go; below even that, the tool says so.
func TestHealth_SizeCap(t *testing.T) {
	cs, _ := opsSession(t, healthyArc(), opsOpts{maxChars: 200})
	text, res := callOps(t, cs, "arc_health", nil)
	want := `{"health":{"http_status":200,"error":"answer larger than --max-response-size allows"},"ready":{"http_status":200,"error":"answer larger than --max-response-size allows"}}`
	if res.IsError || text != want || len(text) > 200 {
		t.Errorf("isErr=%v %s", res.IsError, text)
	}
	cs, _ = opsSession(t, healthyArc(), opsOpts{maxChars: 50})
	if text, res := callOps(t, cs, "arc_health", nil); !res.IsError || !strings.Contains(text, "Arc's health answer does not fit") {
		t.Errorf("cap 50: isErr=%v %s", res.IsError, text)
	}
}

func BenchmarkHistoryResult(b *testing.B) {
	for _, n := range []int{360, 17_280, 86_400} {
		points := exactPoints(b, n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				if _, _, err := sampleHistory("api_history", points, 50_000); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
