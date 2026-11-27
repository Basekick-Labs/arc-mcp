package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Synthetic Arc answers for arc_enterprise, shaped like what Arc main and
// v26.09.4 send (recorded live on 2026-11-27) but written by hand: other
// hosts, other ids, a shorter feature list.
const (
	entHealthLicensed = `{"license":{"tier":"enterprise","status":"active","source":"server","expires_at":"2027-06-01T00:00:00Z","days_remaining":186},"status":"ok","uptime_sec":3600}`
	entHealthOSS      = `{"status":"ok","uptime_sec":3600}`
	entClusterOff     = `{"enabled":false,"mode":"standalone","reason":"Clustering not enabled in configuration (cluster.enabled=false)","success":false}`
	entClusterOSS     = `{"enabled":false,"mode":"standalone","reason":"Enterprise license not configured","success":false}`
	entSchedulersOSS  = `{"cq_scheduler":{"enabled":false,"reason":"Enterprise license required"},"retention_scheduler":{"enabled":false,"reason":"Enterprise license required"}}`
	entClusterHealth  = `{"health_checker":{"check_interval_ms":5000,"running":true},"healthy":3,"total":3,"unhealthy":0}`
)

// entNode is one node as Arc lists it, with a counter past 2^53.
func entNode(id, role string) string {
	return fmt.Sprintf(`{"id":%q,"name":"node-%s","role":%q,"state":"healthy","writer_state":"","address":"10.9.0.%d:9100","api_address":"10.9.0.%d:8000","is_active_compactor":false,"stats":{"ingest_rate":9007199254740993}}`,
		id, id, role, len(id), len(id))
}

// entNodes is ids as Arc lists them, in the order given (Arc's is a map's).
func entNodes(ids ...string) string {
	nodes := make([]string, len(ids))
	for i, id := range ids {
		role := "writer"
		if strings.HasPrefix(id, "r") {
			role = "reader"
		}
		nodes[i] = entNode(id, role)
	}
	return "[" + strings.Join(nodes, ",") + "]"
}

// entStatus is a cluster status answered by local, listing ids.
func entStatus(local string, ids ...string) string {
	return fmt.Sprintf(`{"enabled":true,"mode":"cluster","running":true,"cluster_name":"qa","local_node_id":%q,"local_role":"writer","node_count":%d,"healthy_count":%d,"primary_writer_id":"w1","nodes":%s,"raft":{"leader_id":"w1","leader_addr":"10.9.0.2:9200"},"license":{"valid":true,"tier":"enterprise","features":["clustering","cq_scheduler","tiering"]}}`,
		local, len(ids), len(ids), entNodes(ids...))
}

// entSchedulers is a schedulers answer with the given CQ ids, in the order
// given.
func entSchedulers(ids ...int) string {
	jobs := make([]string, len(ids))
	for i, id := range ids {
		jobs[i] = fmt.Sprintf(`{"cq_id":%d,"cq_name":"cq_%d","interval":"1h0m0s"}`, id, id)
	}
	return fmt.Sprintf(`{"cq_scheduler":{"job_count":%d,"jobs":[%s],"license_valid":true,"running":true},"retention_scheduler":{"can_run":false,"gate_role":"reader","license_valid":true,"running":true,"schedule":"0 3 * * *"}}`,
		len(ids), strings.Join(jobs, ","))
}

// clusterArc answers every route as a cluster node w2 does.
func clusterArc() *opsArc {
	return &opsArc{routes: map[string]opsRoute{
		"/health":                {status: 200, body: entHealthLicensed},
		"/api/v1/cluster":        {status: 200, body: entStatus("w2", "w2", "r1", "w1")},
		"/api/v1/cluster/nodes":  {status: 200, body: `{"nodes":` + entNodes("w2", "r1", "w1") + `,"total":3}`},
		"/api/v1/cluster/local":  {status: 200, body: strings.TrimSuffix(entNode("w2", "writer"), "}") + `,"is_local":true}`},
		"/api/v1/cluster/health": {status: 200, body: entClusterHealth},
		"/api/v1/schedulers":     {status: 200, body: entSchedulers(3, 1, 2)},
	}}
}

// standaloneArc answers as a standalone Arc: clusterBody on every cluster
// route.
func standaloneArc(health, clusterBody, schedulers string) *opsArc {
	routes := map[string]opsRoute{"/health": {status: 200, body: health}, "/api/v1/schedulers": {status: 200, body: schedulers}}
	for _, r := range []string{"/api/v1/cluster", "/api/v1/cluster/nodes", "/api/v1/cluster/local", "/api/v1/cluster/health"} {
		routes[r] = opsRoute{status: 200, body: clusterBody}
	}
	return &opsArc{routes: routes}
}

func entSession(t *testing.T, fake *opsArc, o opsOpts) (*mcp.ClientSession, string) {
	t.Helper()
	o.groups = Groups{Enterprise: true}
	return opsSession(t, fake, o)
}

// callEnt calls arc_enterprise and, for a success, checks what every
// success must be: structured content valid under the output schema and the
// same value as the text.
func callEnt(t *testing.T, cs *mcp.ClientSession, args map[string]any) (EnterpriseResult, string, *mcp.CallToolResult) {
	t.Helper()
	text, res := callOps(t, cs, "arc_enterprise", args)
	if res.IsError {
		if res.StructuredContent != nil {
			t.Errorf("%v: an error carries structured content", args)
		}
		return EnterpriseResult{}, text, res
	}
	resolved, err := enterpriseOutputSchema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(res.StructuredContent); err != nil {
		t.Errorf("%v: structured content does not match the schema: %v", args, err)
	}
	var fromText any
	if err := json.Unmarshal([]byte(text), &fromText); err != nil || !reflect.DeepEqual(fromText, res.StructuredContent) {
		t.Errorf("%v: structured content is not the text's value (%v):\n%s", args, err, text)
	}
	var out EnterpriseResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out, text, res
}

// assertOnlyGets fails unless every request was a GET with the token.
func (f *opsArc) assertOnlyGets(t *testing.T) {
	t.Helper()
	f.assertToken(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.methods {
		if m != "GET" {
			t.Errorf("%s %s: arc_enterprise only reads", m, f.seen[i])
		}
	}
}

func nodeIDs(t *testing.T, data any) []string {
	t.Helper()
	var ids []string
	for _, n := range data.(map[string]any)["nodes"].([]any) {
		ids = append(ids, n.(map[string]any)["id"].(string))
	}
	return ids
}

// Every view on a cluster node: its route, its state, nodes sorted by id,
// and answered_by only where Arc's answer names the node.
func TestEnterprise_ClusterViews(t *testing.T) {
	for _, tt := range []struct {
		args       map[string]any
		requests   string
		answeredBy string
		check      func(t *testing.T, r EnterpriseResult, text string)
	}{
		{map[string]any{"view": "capabilities"}, "/health /api/v1/cluster /api/v1/schedulers", "", func(t *testing.T, r EnterpriseResult, _ string) {
			if r.State != "" || r.License.State != stateActive || r.Clustering.State != stateActive || r.Note != "" {
				t.Errorf("capabilities: %+v", r)
			}
			cl := r.Clustering.Data.(map[string]any)
			if cl["local_node_id"] != "w2" || cl["nodes"] != nil || cl["raft"] != nil {
				t.Errorf("clustering data %v: the summary, naming the node that answered", cl)
			}
			if f, _ := json.Marshal(cl["license"].(map[string]any)["features"]); string(f) != `["clustering","cq_scheduler","tiering"]` {
				t.Errorf("features %s", f)
			}
			jobs, _ := json.Marshal(r.CQScheduler.Data.(map[string]any)["jobs"])
			if !strings.Contains(string(jobs), `"cq_id":1,`) || strings.Index(string(jobs), `"cq_id":1,`) > strings.Index(string(jobs), `"cq_id":3,`) || r.CQScheduler.Truncated {
				t.Errorf("jobs %s: sorted by cq_id, all kept", jobs)
			}
			if r.RetentionScheduler.State != stateActive || r.RetentionScheduler.Data.(map[string]any)["can_run"] != false {
				t.Errorf("retention %+v", r.RetentionScheduler)
			}
		}},
		{map[string]any{"view": "cluster"}, "/api/v1/cluster", "w2", func(t *testing.T, r EnterpriseResult, text string) {
			if got := nodeIDs(t, r.Data); strings.Join(got, ",") != "r1,w1,w2" || *r.Returned != 3 || *r.Total != 3 || r.Truncated {
				t.Errorf("nodes %v returned %v total %v", got, *r.Returned, *r.Total)
			}
			if !strings.Contains(text, "9007199254740993") || !strings.Contains(text, `"leader_addr":"10.9.0.2:9200"`) {
				t.Errorf("Arc's status, numbers exact and addresses as Arc sent them:\n%s", text)
			}
		}},
		{map[string]any{"view": "nodes"}, "/api/v1/cluster/nodes", "", func(t *testing.T, r EnterpriseResult, _ string) {
			if got := nodeIDs(t, r.Data); strings.Join(got, ",") != "r1,w1,w2" || *r.Total != 3 {
				t.Errorf("nodes %v", got)
			}
		}},
		{map[string]any{"view": "nodes", "role": "writer", "state": "healthy"}, "/api/v1/cluster/nodes?role=writer&state=healthy", "", nil},
		{map[string]any{"view": "nodes", "role": "", "state": "", "node_id": ""}, "/api/v1/cluster/nodes", "", nil},
		{map[string]any{"view": "node", "node_id": "w1"}, "/api/v1/cluster/nodes", "", func(t *testing.T, r EnterpriseResult, _ string) {
			if r.Data.(map[string]any)["id"] != "w1" || r.Returned != nil {
				t.Errorf("node %+v", r)
			}
		}},
		{map[string]any{"view": "local_node"}, "/api/v1/cluster/local", "w2", nil},
		{map[string]any{"view": "cluster_health"}, "/api/v1/cluster/health", "", func(t *testing.T, r EnterpriseResult, _ string) {
			if fmt.Sprint(r.Data.(map[string]any)["total"]) != "3" {
				t.Errorf("health %+v", r)
			}
		}},
	} {
		fake := clusterArc()
		cs, addr := entSession(t, fake, opsOpts{})
		r, text, res := callEnt(t, cs, tt.args)
		if res.IsError {
			t.Errorf("%v: %s", tt.args, text)
			continue
		}
		if got := strings.Join(fake.requests(), " "); got != tt.requests {
			t.Errorf("%v: requested %q, want %q", tt.args, got, tt.requests)
		}
		if r.View != tt.args["view"] || r.AnsweredBy != tt.answeredBy || (tt.args["view"] != "capabilities" && r.State != stateActive) {
			t.Errorf("%v: view %q state %q answered_by %q, want answered_by %q", tt.args, r.View, r.State, r.AnsweredBy, tt.answeredBy)
		}
		if tt.check != nil {
			tt.check(t, r, text)
		}
		fake.assertOnlyGets(t)
		assertNoLeak(t, text, addr)
	}
}

// A node that is not in the list is a result naming what to do; a local
// answer without is_local names no node.
func TestEnterprise_NodeNotFoundAndUnnamedLocal(t *testing.T) {
	fake := clusterArc()
	fake.routes["/api/v1/cluster/local"] = opsRoute{status: 200, body: entNode("w2", "writer")}
	cs, _ := entSession(t, fake, opsOpts{})
	if r, text, _ := callEnt(t, cs, map[string]any{"view": "node", "node_id": "w9"}); r.State != stateNotFound || !strings.Contains(r.Reason, `"w9"`) || r.Data != nil {
		t.Errorf("unknown node: %s", text)
	}
	if r, text, _ := callEnt(t, cs, map[string]any{"view": "local_node"}); r.AnsweredBy != "" || r.State != stateActive {
		t.Errorf("local answer without is_local: %s", text)
	}
}

// A standalone Arc answers every cluster view with its reason; Arc's
// reasons on main and on v26.09.4 map to the same states.
func TestEnterprise_StandaloneStates(t *testing.T) {
	oss := standaloneArc(entHealthOSS, entClusterOSS, entSchedulersOSS)
	cs, _ := entSession(t, oss, opsOpts{})
	r, text, _ := callEnt(t, cs, map[string]any{"view": "capabilities"})
	if r.License.State != stateNotLicensed || !strings.Contains(r.License.Reason, "none of the areas below is licensed") ||
		r.Clustering.State != stateNotLicensed || r.Clustering.Reason != "Enterprise license not configured" ||
		r.CQScheduler.State != stateNotLicensed || r.RetentionScheduler.Reason != "Enterprise license required" || r.Note != "" {
		t.Errorf("OSS capabilities: %s", text)
	}
	for _, view := range []string{"cluster", "nodes", "local_node", "cluster_health"} {
		if r, text, _ := callEnt(t, cs, map[string]any{"view": view}); r.State != stateNotLicensed || r.Reason != "Enterprise license not configured" || r.Data != nil || r.AnsweredBy != "" {
			t.Errorf("OSS %s: %s", view, text)
		}
	}
	if r, text, _ := callEnt(t, cs, map[string]any{"view": "node", "node_id": "w1"}); r.State != stateNotLicensed {
		t.Errorf("OSS node: %s", text)
	}

	ent := standaloneArc(entHealthLicensed, entClusterOff, entSchedulers(2, 1))
	cs, _ = entSession(t, ent, opsOpts{})
	r, text, _ = callEnt(t, cs, map[string]any{"view": "capabilities"})
	if r.License.State != stateActive || r.Clustering.State != stateNotEnabled || r.CQScheduler.State != stateActive ||
		!strings.Contains(r.Note, "only in cluster mode") {
		t.Errorf("Enterprise standalone capabilities: %s", text)
	}

	for reason, want := range map[string]string{
		"Enterprise license not configured":                               stateNotLicensed,
		"Enterprise license not valid":                                    stateNotLicensed,
		"License does not include clustering feature":                     stateNotLicensed,
		"Clustering not enabled in configuration (cluster.enabled=false)": stateNotEnabled,
		"Enterprise license required":                                     stateNotLicensed,
		"License tier does not include CQ scheduler feature":              stateNotLicensed,
		"CQ feature not enabled (continuous_query.enabled=false)":         stateNotEnabled, // v26.09.4
		"Retention feature not enabled (retention.enabled=false), or the scheduler failed to start (see the startup log)": stateNotEnabled, // main
	} {
		if got, gotReason, ok := disabledState(map[string]any{"enabled": false, "reason": reason}); !ok || got != want || gotReason != reason {
			t.Errorf("%q: %s, want %s", reason, got, want)
		}
	}
	// A reason is quoted from Arc at most 300 runes long.
	if _, got, _ := disabledState(map[string]any{"enabled": false, "reason": strings.Repeat("é", 400)}); got != strings.Repeat("é", 300)+"…" {
		t.Errorf("long reason kept as %d runes", len([]rune(got)))
	}
}

// An Arc before 26.09.1 reports no licence in /health even with one in
// force: the other areas show it, and the licence reads active, not absent.
func TestEnterprise_LicenceNotInHealth(t *testing.T) {
	for _, tt := range []struct {
		name               string
		cluster, scheduler string
		want               string
	}{
		{"cluster mode", entStatus("w2", "w2"), entSchedulersOSS, stateActive},
		{"standalone, clustering off in the configuration", entClusterOff, entSchedulersOSS, stateActive},
		{"a licence without the features", strings.Replace(entClusterOff, "Clustering not enabled in configuration (cluster.enabled=false)", "License does not include clustering feature", 1),
			strings.ReplaceAll(entSchedulersOSS, "Enterprise license required", "License tier does not include CQ scheduler feature"), stateActive},
		{"a scheduler running", entClusterOSS, entSchedulers(1), stateActive},
		{"no licence", entClusterOSS, entSchedulersOSS, stateNotLicensed},
	} {
		cs, _ := entSession(t, standaloneArc(entHealthOSS, tt.cluster, tt.scheduler), opsOpts{})
		r, text, _ := callEnt(t, cs, map[string]any{"view": "capabilities"})
		if r.License.State != tt.want || r.License.Data != nil || (tt.want == stateActive && !strings.Contains(r.License.Reason, "26.09.1")) {
			t.Errorf("%s: %s", tt.name, text)
		}
	}
}

// No route here answers 403 for a licence (Arc checks none at request time
// on them), so a licence that lapsed after boot shows only in the bodies.
func TestEnterprise_LicenceStatesInTheBody(t *testing.T) {
	for _, tt := range []struct{ health, want string }{
		{`{"license":{"tier":"oss","status":"unlicensed","source":"none"},"status":"ok"}`, stateNotLicensed},
		{`{"license":{"tier":"enterprise","status":"expired","source":"cache"},"status":"ok"}`, stateNotLicensed},
		{`{"license":{"tier":"enterprise","status":"grace_period","grace_day":2},"status":"ok"}`, stateActive},
		{`{"license":{"tier":"enterprise","status":"writes_stopped"},"status":"ok"}`, stateActive},
	} {
		fake := clusterArc()
		fake.routes["/health"] = opsRoute{status: 200, body: tt.health}
		fake.routes["/api/v1/cluster"] = opsRoute{status: 200, body: strings.Replace(entStatus("w2", "w2"), `"valid":true`, `"valid":false`, 1)}
		fake.routes["/api/v1/schedulers"] = opsRoute{status: 200, body: `{"cq_scheduler":{"job_count":0,"jobs":[],"license_valid":false,"running":true},"retention_scheduler":{"license_valid":true,"running":false}}`}
		cs, _ := entSession(t, fake, opsOpts{})
		r, text, _ := callEnt(t, cs, map[string]any{"view": "capabilities"})
		if r.License.State != tt.want || r.License.Data == nil {
			t.Errorf("%s: licence %+v", tt.health, r.License)
		}
		if r.Clustering.State != stateActive || !strings.Contains(text, `"valid":false`) {
			t.Errorf("clustering keeps running and shows Arc's valid:false: %s", text)
		}
		if r.CQScheduler.State != stateNotLicensed || r.RetentionScheduler.State != stateNotEnabled || r.RetentionScheduler.Reason == "" {
			t.Errorf("schedulers: %s", text)
		}
	}
}

// Arc's own 404 for a route it does not serve (an Arc before 26.02.1) is a
// state; any other 404 is a wrong URL, including Arc naming a path prefix a
// proxy forwarded.
func TestEnterprise_NotServed(t *testing.T) {
	old := &opsArc{routes: map[string]opsRoute{"/health": {status: 200, body: entHealthOSS}}} // every other path: Cannot GET <path>
	cs, _ := entSession(t, old, opsOpts{})
	r, text, _ := callEnt(t, cs, map[string]any{"view": "capabilities"})
	if r.Clustering.State != stateNotServed || r.CQScheduler.State != stateNotServed || r.RetentionScheduler.State != stateNotServed || !strings.Contains(r.Clustering.Reason, "26.02.1") {
		t.Errorf("old Arc capabilities: %s", text)
	}
	for _, view := range []string{"cluster", "nodes", "local_node", "cluster_health"} {
		if r, text, _ := callEnt(t, cs, map[string]any{"view": view}); r.State != stateNotServed || !strings.Contains(r.Reason, viewRoutes[view]) {
			t.Errorf("old Arc %s: %s", view, text)
		}
	}

	for name, o := range map[string]struct {
		base  string
		route string
		body  string
		want  string
	}{
		"proxy 404":                       {"", "/api/v1/cluster", `<html>Not Found</html>`, "Arc answered HTTP 404 for /api/v1/cluster"},
		"another JSON 404":                {"", "/api/v1/cluster", `{"error":"Not Found"}`, "Arc answered HTTP 404 for /api/v1/cluster"},
		"prefix forwarded to Arc":         {"/arc", "/arc/api/v1/cluster", `{"error":"Cannot GET /arc/api/v1/cluster"}`, "Arc answered HTTP 404 for /api/v1/cluster"},
		"prefix stripped, route not here": {"/arc", "/arc/api/v1/cluster", `{"error":"Cannot GET /api/v1/cluster"}`, ""},
	} {
		fake := &opsArc{routes: map[string]opsRoute{o.route: {status: 404, body: o.body}}}
		cs, _ := entSession(t, fake, opsOpts{baseURL: "http://" + fakeHost + ":8000" + o.base})
		r, text, res := callEnt(t, cs, map[string]any{"view": "cluster"})
		switch {
		case o.want == "" && (res.IsError || r.State != stateNotServed):
			t.Errorf("%s: %s, want not_served", name, text)
		case o.want != "" && (!res.IsError || !strings.Contains(text, o.want)):
			t.Errorf("%s: %s, want an error containing %q", name, text, o.want)
		}
	}
}

// Failures are tool errors with fixed texts: Arc's or a proxy's body is
// never quoted, and nothing says "database" or "read permission".
func TestEnterprise_ArcErrors(t *testing.T) {
	for _, tt := range []struct {
		name  string
		route opsRoute
		want  string
	}{
		{"401", opsRoute{status: 401, body: `{"error":"Invalid or expired token"}`}, "Arc rejected the token"},
		{"403", opsRoute{status: 403, body: `{"error":"forbidden by gateway 10.0.0.7"}`}, "Arc refused /api/v1/schedulers (HTTP 403)"},
		{"403 permission data", opsRoute{status: 403, body: `{"error":"permission data unavailable"}`}, "could not read its own permission data"},
		{"429", opsRoute{status: 429, body: `{"error":"upstream 10.0.0.7 is rate limiting you"}`}, "/api/v1/schedulers was rate limited (HTTP 429)"},
		{"400", opsRoute{status: 400, body: `{"error":"gateway 10.0.0.7 rejected the request"}`}, "/api/v1/schedulers was refused (HTTP 400)"},
		{"500", opsRoute{status: 500, body: `{"error":"boom at /var/lib/arc"}`}, "Arc returned an error"},
		{"array", opsRoute{status: 200, body: `[1,2]`}, "unexpected response format"},
		{"not JSON", opsRoute{status: 200, body: proxyPage}, "unexpected response format"},
		{"not Arc's shape", opsRoute{status: 200, body: `{"cq_scheduler":"on"}`}, "unexpected response format"},
	} {
		fake := clusterArc()
		fake.routes["/api/v1/schedulers"] = tt.route
		cs, _ := entSession(t, fake, opsOpts{})
		_, text, res := callEnt(t, cs, map[string]any{"view": "capabilities"})
		if !res.IsError || !strings.Contains(text, tt.want) {
			t.Errorf("%s: %q, want an error containing %q", tt.name, text, tt.want)
		}
		for _, leak := range []string{"10.0.0.7", "/var/lib", "database", "read permission", "boom"} {
			// Arc's fail-closed text keeps its shared wording ("not a problem
			// with the token or the database"), as arc_metrics does.
			if strings.Contains(text, leak) && (leak != "database" || tt.name != "403 permission data") {
				t.Errorf("%s: answer carries %q: %s", tt.name, leak, text)
			}
		}
	}
	for name, body := range map[string]string{"status without enabled": `{"nodes":[]}`, "nodes without a list": `{"total":0}`} {
		fake := clusterArc()
		fake.routes["/api/v1/cluster"] = opsRoute{status: 200, body: body}
		fake.routes["/api/v1/cluster/nodes"] = opsRoute{status: 200, body: body}
		cs, _ := entSession(t, fake, opsOpts{})
		view := map[bool]string{true: "cluster", false: "nodes"}[strings.HasPrefix(name, "status")]
		if _, text, res := callEnt(t, cs, map[string]any{"view": view}); !res.IsError || !strings.Contains(text, "unexpected response format") {
			t.Errorf("%s: %s", name, text)
		}
	}
}

// One deadline covers every request of a call; an Arc that does not answer
// is an error naming no address.
func TestEnterprise_TimeoutAndArcDown(t *testing.T) {
	fake := clusterArc()
	fake.routes["/health"] = opsRoute{status: 200, body: entHealthLicensed, delay: 150 * time.Millisecond}
	fake.routes["/api/v1/cluster"] = opsRoute{status: 200, body: entStatus("w2", "w2"), delay: 150 * time.Millisecond}
	cs, _ := entSession(t, fake, opsOpts{timeout: 250 * time.Millisecond})
	if _, text, res := callEnt(t, cs, map[string]any{"view": "capabilities"}); !res.IsError || !strings.Contains(text, "Arc did not answer /api/v1/cluster within 250ms") {
		t.Errorf("shared deadline: %s", text)
	}
	cs, _ = entSession(t, clusterArc(), opsOpts{down: true})
	if _, text, res := callEnt(t, cs, map[string]any{"view": "nodes"}); !res.IsError || !strings.Contains(text, "Arc could not be reached") {
		t.Errorf("Arc down: %s", text)
	}
}

// Lists lose whole items from the end to fit --max-response-size, and say
// so; a status that does not fit even without its nodes is refused.
func TestEnterprise_SizeBudget(t *testing.T) {
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("w%02d", 40-i) // reverse order: the answer sorts
	}
	fake := clusterArc()
	fake.routes["/api/v1/cluster"] = opsRoute{status: 200, body: entStatus("w07", ids...)}
	fake.routes["/api/v1/cluster/nodes"] = opsRoute{status: 200, body: `{"nodes":` + entNodes(ids...) + `,"total":40}`}
	jobs := make([]int, 30)
	for i := range jobs {
		jobs[i] = 30 - i
	}
	fake.routes["/api/v1/schedulers"] = opsRoute{status: 200, body: entSchedulers(jobs...)}

	for _, view := range []string{"cluster", "nodes"} {
		cs, _ := entSession(t, fake, opsOpts{maxChars: 4000})
		r, text, res := callEnt(t, cs, map[string]any{"view": view})
		got := nodeIDs(t, r.Data)
		if res.IsError || len(text) > 4000 || !r.Truncated || *r.Total != 40 || *r.Returned != len(got) || len(got) == 0 || len(got) >= 40 || got[0] != "w01" {
			t.Errorf("%s at 4000: %d chars, returned %v of %v, first %v", view, len(text), len(got), r.Total, got)
		}
		cs, _ = entSession(t, fake, opsOpts{unlimited: true})
		if r, _, _ := callEnt(t, cs, map[string]any{"view": view}); *r.Returned != 40 || r.Truncated {
			t.Errorf("%s unlimited: returned %v", view, *r.Returned)
		}
	}

	cs, _ := entSession(t, fake, opsOpts{})
	r, _, _ := callEnt(t, cs, map[string]any{"view": "capabilities"})
	cq := r.CQScheduler.Data.(map[string]any)
	if kept := cq["jobs"].([]any); len(kept) != 20 || !r.CQScheduler.Truncated || fmt.Sprint(cq["job_count"]) != "30" ||
		fmt.Sprint(kept[0].(map[string]any)["cq_id"]) != "1" || fmt.Sprint(kept[19].(map[string]any)["cq_id"]) != "20" {
		t.Errorf("jobs: %d kept, truncated %v, job_count %v, %v", len(kept), r.CQScheduler.Truncated, cq["job_count"], kept)
	}

	fake.routes["/api/v1/cluster"] = opsRoute{status: 200, body: strings.Replace(entStatus("w07", "w01"), `"raft":{`, `"raft":{"big":"`+strings.Repeat("x", 5000)+`",`, 1)}
	cs, _ = entSession(t, fake, opsOpts{maxChars: 4000})
	if _, text, res := callEnt(t, cs, map[string]any{"view": "cluster"}); !res.IsError || !strings.Contains(text, "does not fit in arc-mcp's --max-response-size (4000 characters)") {
		t.Errorf("oversized status: %s", text)
	}
}

// fit keeps the largest number of items that fits, not merely one that
// does: checked against every count.
func TestEnterprise_FitIsMaximal(t *testing.T) {
	item := strings.Repeat("x", 100)
	set := func(r *EnterpriseResult, k int) { r.Data = repeatItems(item, k) }
	for _, n := range []int{0, 1, 7, 64} {
		for _, maxChars := range []int{20, 150, 333, 1000, 10_000} {
			want := -1
			for k := 0; k <= n; k++ {
				if text, _ := compactJSON(EnterpriseResult{View: "nodes", Data: repeatItems(item, k)}); len(text) <= maxChars {
					want = k
				}
			}
			r, text, failed := fit(EnterpriseResult{View: "nodes"}, n, set, maxChars)
			got := -1
			if failed == "" {
				got = len(r.Data.([]string))
			}
			if got != want || len(text) > maxChars {
				t.Errorf("n=%d max=%d: kept %d (%d chars, %q), want %d", n, maxChars, got, len(text), failed, want)
			}
		}
	}
}

func repeatItems(s string, k int) []string {
	out := make([]string, k)
	for i := range out {
		out[i] = s
	}
	return out
}

// The schema states the views, filters and bounds, so a client can check
// them before calling (#37).
func TestEnterprise_InputSchema(t *testing.T) {
	cs, _ := entSession(t, clusterArc(), opsOpts{})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(res.Tools) != 1 {
		t.Fatalf("tools %v, %v", res, err)
	}
	schema, _ := json.Marshal(res.Tools[0].InputSchema)
	for _, want := range []string{
		`"enum":["capabilities","cluster","nodes","node","local_node","cluster_health"]`,
		`"enum":["","writer","reader","compactor","standalone"]`,
		`"enum":["","healthy","unhealthy","dead","unknown","joining","leaving"]`,
		`"maxLength":256`, `"pattern":"^[!-~]*$"`, `"required":["view"]`, `"additionalProperties":false`,
	} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("input schema lacks %s:\n%s", want, schema)
		}
	}
	if a := res.Tools[0].Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("annotations %+v", a)
	}
	var out jsonschema.Schema
	b, _ := json.Marshal(res.Tools[0].OutputSchema)
	if err := json.Unmarshal(b, &out); err != nil || out.Properties["answered_by"] == nil {
		t.Fatalf("output schema %s (%v)", b, err)
	}
	// The areas' shape is stated once, under $defs, and still enforced.
	resolved, err := out.Resolve(nil)
	if err != nil || out.Properties["license"].Ref != "#/$defs/area" {
		t.Fatalf("output schema: %v %s", err, b)
	}
	if resolved.Validate(map[string]any{"view": "capabilities", "license": map[string]any{"reason": "no state"}}) == nil {
		t.Error("an area without a state validates")
	}
}

// Arguments a view does not take, or out of bounds, are refused before
// anything reaches Arc.
func TestEnterprise_ArgumentsRefusedBeforeArc(t *testing.T) {
	for _, tt := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"view": "node"}, "Invalid argument: view node needs node_id"},
		{map[string]any{"view": "nodes", "node_id": "w1"}, "Invalid argument: node_id applies only to view node"},
		{map[string]any{"view": "cluster", "role": "writer"}, "Invalid argument: role and state apply only to view nodes"},
		{map[string]any{"view": "capabilities", "state": "dead"}, "Invalid argument: role and state apply only to view nodes"},
		// The SDK's schema check, before the handler.
		{map[string]any{"view": "status"}, "enum: status does not equal any of"},
		{map[string]any{"view": "nodes", "role": "leader"}, "enum: leader does not equal any of"},
		{map[string]any{"view": "node", "node_id": "w 1"}, "validating /properties/node_id: pattern"},
		{map[string]any{"view": "node", "node_id": strings.Repeat("a", 257)}, "validating /properties/node_id: maxLength"},
		{map[string]any{}, `missing properties: ["view"]`},
		{map[string]any{"view": "nodes", "limit": 5}, `unexpected additional properties ["limit"]`},
	} {
		fake := clusterArc()
		cs, _ := entSession(t, fake, opsOpts{})
		_, text, res := callEnt(t, cs, tt.args)
		if !res.IsError || !strings.Contains(text, tt.want) {
			t.Errorf("%v: %q, want an error containing %q", tt.args, text, tt.want)
		}
		if got := fake.requests(); len(got) != 0 {
			t.Errorf("%v: sent %v to Arc", tt.args, got)
		}
	}
	// The handler's own check, for a caller that bypasses the schema.
	for _, a := range []EnterpriseArgs{{View: "x"}, {View: "node", NodeID: "w\x001"}, {View: "node", NodeID: strings.Repeat("a", 257)}, {View: "nodes", Role: "x"}, {View: "nodes", State: "x"}} {
		if checkEnterpriseArgs(a) == "" {
			t.Errorf("%+v accepted", a)
		}
	}
}
