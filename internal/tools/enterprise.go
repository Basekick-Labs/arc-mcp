package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// arc_enterprise (group enterprise, opt-in through --tools) reports Arc's
// Enterprise features: the licence, clustering and the two schedulers. Arc
// checks no permission on the routes it reads, so any valid token, the
// read-only one arc-mcp asks for included, reads them. An answer describes
// the node that answered it, and answered_by names that node only when
// Arc's own answer does: a second request to ask would, behind a load
// balancer, be answered by whichever node it reached.

// The routes read here. Arc has served all of them since 26.02.1; /health
// has reported a configured licence since 26.09.1.
const (
	routeHealth        = "/health"
	routeCluster       = "/api/v1/cluster"
	routeNodes         = "/api/v1/cluster/nodes"
	routeLocalNode     = "/api/v1/cluster/local"
	routeClusterHealth = "/api/v1/cluster/health"
	routeSchedulers    = "/api/v1/schedulers"
)

// enterpriseViews are the views in the order the tool lists them;
// viewRoutes is the one route behind each view but capabilities.
var (
	enterpriseViews = []string{"capabilities", "cluster", "nodes", "node", "local_node", "cluster_health"}
	viewRoutes      = map[string]string{
		"cluster":        routeCluster,
		"nodes":          routeNodes,
		"node":           routeNodes,
		"local_node":     routeLocalNode,
		"cluster_health": routeClusterHealth,
	}
)

// The states an answer reports: results, not errors.
const (
	stateActive      = "active"
	stateNotEnabled  = "not_enabled"
	stateNotLicensed = "not_licensed"
	stateNotServed   = "not_served"
	stateNotFound    = "not_found"
	// stateUnavailable is a capabilities area arc-mcp could not read; the
	// other areas still answer, as each arc_health probe does.
	stateUnavailable = "unavailable"
	// stateUnknown is a licence status arc-mcp does not know.
	stateUnknown = "unknown"
)

// Arc's filters for /api/v1/cluster/nodes (validRoles and validStates in
// Arc's internal/api/cluster.go).
var (
	nodeRoles  = []string{"writer", "reader", "compactor", "standalone"}
	nodeStates = []string{"healthy", "unhealthy", "dead", "unknown", "joining", "leaving"}
)

const (
	// maxNodeIDLen is Arc's own bound on a node id (maxNodeIDLength).
	maxNodeIDLen = 256
	// maxSchedulerJobs is the most CQ jobs capabilities lists. Arc's list
	// has no bound; its job_count stays the total.
	maxSchedulerJobs = 20
	// maxReasonRunes caps a reason quoted from Arc's answer.
	maxReasonRunes = 300
)

// EnterpriseArgs is the input for arc_enterprise.
type EnterpriseArgs struct {
	View   string `json:"view"`
	NodeID string `json:"node_id,omitempty"`
	Role   string `json:"role,omitempty"`
	State  string `json:"state,omitempty"`
}

// EnterpriseArea is one part of the capabilities view.
type EnterpriseArea struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Data   any    `json:"data,omitempty"`
}

// EnterpriseResult is arc_enterprise's answer: state, reason, answered_by
// and data for every view but capabilities, which answers per area. Only the
// descriptions the field names do not already say are in the schema: it is
// part of every tools/list.
type EnterpriseResult struct {
	View               string          `json:"view"`
	State              string          `json:"state,omitempty" jsonschema:"active, not_enabled, not_licensed, not_served (this Arc lacks the route) or not_found (node). capabilities has one per area instead, which can also be unavailable (not read: see reason) or, for the licence, unknown."`
	Reason             string          `json:"reason,omitempty"`
	AnsweredBy         string          `json:"answered_by,omitempty" jsonschema:"The answering node's id, when Arc's answer names it (cluster, local_node)."`
	Data               any             `json:"data,omitempty" jsonschema:"Arc's answer, numbers exact."`
	Returned           *int            `json:"returned,omitempty" jsonschema:"nodes: the nodes in data, of total."`
	Total              *int            `json:"total,omitempty"`
	Truncated          bool            `json:"truncated,omitempty" jsonschema:"Cut to fit the size limit: fewer nodes (nodes), parts left out (cluster, see omitted) or fewer CQ jobs (capabilities)."`
	Omitted            []string        `json:"omitted,omitempty" jsonschema:"cluster: status parts left out to fit, largest first."`
	License            *EnterpriseArea `json:"license,omitempty"`
	Clustering         *EnterpriseArea `json:"clustering,omitempty"`
	CQScheduler        *EnterpriseArea `json:"cq_scheduler,omitempty"`
	RetentionScheduler *EnterpriseArea `json:"retention_scheduler,omitempty"`
	Note               string          `json:"note,omitempty"`
}

var enterpriseOutputSchema = mustSchema[EnterpriseResult]()

// enterpriseInputSchema is arc_enterprise's input schema, written out for
// its enums and bounds (#37). "" stands for "not given" in node_id, role and
// state, as clients that send every field send it.
func enterpriseInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"view": map[string]any{
				"type":        "string",
				"description": "What to read: " + orList(enterpriseViews) + ".",
				"enum":        enterpriseViews,
			},
			"node_id": map[string]any{
				"type":        "string",
				"description": "View node only: the node's id, as view nodes lists it.",
				"maxLength":   maxNodeIDLen,
				"pattern":     "^[!-~]*$",
			},
			"role": map[string]any{
				"type":        "string",
				"description": "View nodes only: list only nodes of this role (empty: every role).",
				"enum":        append([]string{""}, nodeRoles...),
			},
			"state": map[string]any{
				"type":        "string",
				"description": "View nodes only: list only nodes in this state (empty: every state).",
				"enum":        append([]string{""}, nodeStates...),
			},
		},
		"required":             []string{"view"},
		"additionalProperties": false,
	}
}

// checkEnterpriseArgs is the handler's own check of what the schema says,
// plus which view takes which argument; "" when args are valid.
func checkEnterpriseArgs(a EnterpriseArgs) string {
	if _, ok := viewRoutes[a.View]; !ok && a.View != "capabilities" {
		return "view must be one of " + orList(enterpriseViews)
	}
	switch {
	case a.View == "node" && a.NodeID == "":
		return "view node needs node_id"
	case a.View != "node" && a.NodeID != "":
		return "node_id applies only to view node"
	case len(a.NodeID) > maxNodeIDLen || strings.ContainsFunc(a.NodeID, func(r rune) bool { return r <= ' ' || r > '~' }):
		return "node_id must be at most 256 visible ASCII characters"
	case a.View != "nodes" && (a.Role != "" || a.State != ""):
		return "role and state apply only to view nodes"
	case a.Role != "" && !slices.Contains(nodeRoles, a.Role):
		return "role must be one of " + orList(nodeRoles)
	case a.State != "" && !slices.Contains(nodeStates, a.State):
		return "state must be one of " + orList(nodeStates)
	}
	return ""
}

// RegisterEnterprise registers arc_enterprise.
func RegisterEnterprise(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:         "arc_enterprise",
		Title:        "Read Arc Enterprise status",
		OutputSchema: enterpriseOutputSchema,
		Annotations:  readOnly("Read Arc Enterprise status"),
		Description: "Read the state of Arc's Enterprise features (about Arc itself, not data stored in it). view: " +
			"capabilities (start here: the licence, clustering with the licence's features in cluster mode, and the CQ and retention schedulers), " +
			"cluster (the cluster status), nodes (every node; role and state filter it), node (one node, by node_id), " +
			"local_node (the node that answered) or cluster_health. " +
			"Each answer, and each capabilities area, has a state: active, not_enabled, not_licensed or not_served (this Arc predates the route); these are results to report, not failures. " +
			"A capabilities area arc-mcp could not read is unavailable, with the reason. " +
			"Answers come from the Arc node at the configured URL, and answered_by names it when Arc's answer does (cluster and local_node); behind a load balancer, calls may reach different nodes. " +
			"Node entries carry each node's addresses as Arc reports them.",
		InputSchema: enterpriseInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args EnterpriseArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		if msg := checkEnterpriseArgs(args); msg != "" {
			return invalidArgument(msg), nil, nil
		}
		// One deadline for every request of the call, as arc_health has.
		if t := client.Timeout(); t > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, t)
			defer cancel()
		}
		var (
			res          EnterpriseResult
			text, failed string
		)
		if args.View == "capabilities" {
			res, text, failed = capabilities(ctx, client, maxResponseChars)
		} else {
			res, text, failed = clusterView(ctx, client, args, maxResponseChars)
		}
		if failed != "" {
			return opsError(failed), nil, nil
		}
		return opsResult(res, text), nil, nil
	})
}

// readObject GETs route and returns Arc's JSON object. notServed is set
// instead for Arc's own 404 for the route; failed is the model's text for
// any other failure.
func readObject(ctx context.Context, client *arc.Client, route string, query url.Values) (obj map[string]any, notServed bool, failed string) {
	v, err := client.GetJSON(ctx, route, query)
	switch {
	case arc.NotServed(err):
		return nil, true, ""
	case err != nil:
		return nil, false, enterpriseErrorText(client, route, err)
	}
	if obj, ok := v.(map[string]any); ok {
		return obj, false, ""
	}
	return nil, false, errNotArc.Error()
}

func notServedText(route string) string {
	return "This Arc does not serve " + route + " (Arc serves it since 26.02.1)."
}

// disabledState reads Arc's {"enabled":false,"reason":...} answer, given
// by every cluster route on a standalone Arc and by a scheduler that is not
// running; ok is false for any other object. Arc names a licence in each
// licence reason ("Enterprise license not valid", "License tier does not
// include ...") and in no other.
func disabledState(obj map[string]any) (state, reason string, ok bool) {
	if enabled, isBool := obj["enabled"].(bool); !isBool || enabled {
		return "", "", false
	}
	reason, _ = obj["reason"].(string)
	reason = strings.TrimSpace(reason)
	if r := []rune(reason); len(r) > maxReasonRunes {
		reason = string(r[:maxReasonRunes]) + "…"
	}
	if strings.Contains(strings.ToLower(reason), "license") {
		return stateNotLicensed, reason, true
	}
	return stateNotEnabled, reason, true
}

// clusterView answers every view but capabilities from its one route.
func clusterView(ctx context.Context, client *arc.Client, args EnterpriseArgs, maxChars int) (EnterpriseResult, string, string) {
	res := EnterpriseResult{View: args.View}
	route := viewRoutes[args.View]
	query := url.Values{}
	if args.Role != "" {
		query.Set("role", args.Role)
	}
	if args.State != "" {
		query.Set("state", args.State)
	}
	obj, notServed, failed := readObject(ctx, client, route, query)
	if failed != "" {
		return res, "", failed
	}
	if notServed {
		res.State, res.Reason = stateNotServed, notServedText(route)
		return fit(res, 0, nil, maxChars)
	}
	if state, reason, ok := disabledState(obj); ok {
		res.State, res.Reason = state, reason
		return fit(res, 0, nil, maxChars)
	}
	res.State = stateActive
	switch args.View {
	case "cluster":
		if enabled, _ := obj["enabled"].(bool); !enabled {
			return res, "", errNotArc.Error()
		}
		res.AnsweredBy, _ = obj["local_node_id"].(string)
		return fitStatus(res, obj, maxChars)
	case "nodes":
		return fitNodes(res, obj, maxChars)
	case "node":
		nodes, ok := obj["nodes"].([]any)
		if !ok {
			return res, "", errNotArc.Error()
		}
		for _, n := range nodes {
			if m, ok := n.(map[string]any); ok && m["id"] == args.NodeID {
				res.Data = m
				return fit(res, 0, nil, maxChars)
			}
		}
		res.State = stateNotFound
		res.Reason = "No node with id " + strconv.Quote(args.NodeID) + " is in the answering node's view of the cluster; view nodes lists them."
		return fit(res, 0, nil, maxChars)
	case "local_node":
		if local, _ := obj["is_local"].(bool); local {
			res.AnsweredBy, _ = obj["id"].(string)
		}
	}
	res.Data = obj
	return fit(res, 0, nil, maxChars)
}

// fitNodes answers with obj, its nodes list sorted by id and cut, whole
// nodes from the end, to fit maxChars.
func fitNodes(res EnterpriseResult, obj map[string]any, maxChars int) (EnterpriseResult, string, string) {
	nodes, ok := obj["nodes"].([]any)
	if !ok {
		return res, "", errNotArc.Error()
	}
	nodes = sortedNodes(nodes)
	total := len(nodes)
	return fit(res, total, func(r *EnterpriseResult, k int) {
		data := cloneMap(obj)
		data["nodes"] = nodes[:k]
		r.Data, r.Returned, r.Total, r.Truncated = data, &k, &total, k < total
	}, maxChars)
}

// sortedNodes is nodes sorted by id: Arc builds its node lists from a map,
// so their order changes from call to call.
func sortedNodes(nodes []any) []any {
	nodes = slices.Clone(nodes)
	slices.SortStableFunc(nodes, func(a, b any) int { return strings.Compare(stringField(a, "id"), stringField(b, "id")) })
	return nodes
}

// statusCore is what the cluster view always keeps of Arc's status: the
// answering node and the cluster's size, health and primary writer.
var statusCore = map[string]bool{
	"enabled": true, "mode": true, "running": true, "cluster_name": true, "local_node_id": true, "local_role": true,
	"node_count": true, "healthy_count": true, "writers": true, "readers": true, "compactors": true,
	"primary_writer_id": true, "read_ready": true, "read_not_ready_reason": true,
}

// fitStatus answers with Arc's cluster status, its nodes sorted by id. One
// that does not fit maxChars loses its largest parts outside statusCore, one
// by one, each named in omitted. The node list (which view nodes also gives),
// Raft and replication detail are the parts that grow with the cluster, so a
// cluster of any size answers at the smallest limit.
func fitStatus(res EnterpriseResult, obj map[string]any, maxChars int) (EnterpriseResult, string, string) {
	data := cloneMap(obj)
	if nodes, ok := data["nodes"].([]any); ok {
		data["nodes"] = sortedNodes(nodes)
	}
	type part struct {
		key  string
		size int
	}
	var parts []part
	for k, v := range data {
		if statusCore[k] {
			continue
		}
		text, err := compactJSON(v)
		if err != nil {
			log.Printf("arc_enterprise cluster: rendering the answer: %v", err)
			return res, "", errNotArc.Error()
		}
		parts = append(parts, part{k, len(text)})
	}
	slices.SortFunc(parts, func(a, b part) int {
		if a.size != b.size {
			return b.size - a.size
		}
		return strings.Compare(a.key, b.key)
	})
	// fit keeps the most it can: here, the most of the smallest parts.
	return fit(res, len(parts), func(r *EnterpriseResult, kept int) {
		d := cloneMap(data)
		var omitted []string
		for _, p := range parts[:len(parts)-kept] {
			delete(d, p.key)
			omitted = append(omitted, p.key)
		}
		r.Data, r.Omitted, r.Truncated = d, omitted, len(omitted) > 0
	}, maxChars)
}

// capabilities reads the licence (/health), clustering (/api/v1/cluster)
// and the schedulers (/api/v1/schedulers): three requests, which behind a
// load balancer may reach three nodes, so only the clustering part, whose
// answer names its node, says which. Only a failed /health fails the call:
// a later read that fails leaves its area unavailable and the others
// answered, as arc_health reports each probe.
func capabilities(ctx context.Context, client *arc.Client, maxChars int) (EnterpriseResult, string, string) {
	res := EnterpriseResult{View: "capabilities"}
	v, err := client.GetJSON(ctx, routeHealth, nil)
	if err != nil {
		return res, "", enterpriseErrorText(client, routeHealth, err)
	}
	health, ok := v.(map[string]any)
	if !ok {
		return res, "", errNotArc.Error()
	}
	res.License = licenseArea(health["license"])

	cl, notServed, failed := readObject(ctx, client, routeCluster, nil)
	res.Clustering = clusteringArea(cl, notServed, failed)

	sch, notServed, failed := readObject(ctx, client, routeSchedulers, nil)
	var jobs []any
	switch {
	case failed != "":
		res.CQScheduler = &EnterpriseArea{State: stateUnavailable, Reason: failed}
		res.RetentionScheduler = &EnterpriseArea{State: stateUnavailable, Reason: failed}
	case notServed:
		res.CQScheduler = &EnterpriseArea{State: stateNotServed, Reason: notServedText(routeSchedulers)}
		res.RetentionScheduler = &EnterpriseArea{State: stateNotServed, Reason: notServedText(routeSchedulers)}
	default:
		res.CQScheduler, jobs = schedulerArea(sch["cq_scheduler"])
		res.RetentionScheduler, _ = schedulerArea(sch["retention_scheduler"])
	}
	if _, reported := health["license"]; !reported {
		res.License = unreportedLicence(res)
	}
	// Only when Arc said it runs standalone: an unread or unserved
	// clustering area says nothing about the mode.
	if standalone := res.Clustering.State == stateNotEnabled || res.Clustering.State == stateNotLicensed; res.License.State == stateActive && standalone {
		res.Note = "Arc lists the licence's features only in cluster mode."
	}
	cq := *res.CQScheduler
	cqData, ok := cq.Data.(map[string]any)
	if !ok || jobs == nil {
		return fit(res, 0, nil, maxChars)
	}
	return fit(res, min(len(jobs), maxSchedulerJobs), func(r *EnterpriseResult, k int) {
		data := cloneMap(cqData)
		data["jobs"] = jobs[:k]
		area := cq
		area.Data = data
		r.CQScheduler, r.Truncated = &area, k < len(jobs)
	}, maxChars)
}

// licenseArea reads /health's licence. Arc reports one there whenever a
// licence is configured, and the statuses are a closed set: main's phases
// (active, grace_period, writes_stopped, expired), unlicensed for a licence
// configured but not in force, and read_only, which the licence server sent
// up to v26.09.4. Any other is unknown, never active.
func licenseArea(v any) *EnterpriseArea {
	lic, ok := v.(map[string]any)
	if !ok {
		return &EnterpriseArea{State: stateNotLicensed, Reason: "Arc reports no licence, and none of the areas below is licensed."}
	}
	area := &EnterpriseArea{State: stateActive, Data: lic}
	switch status, _ := lic["status"].(string); status {
	case "active":
	case "grace_period":
		area.Reason = "The licence has expired and is in its grace period: its features stay on until the grace period ends (writes_stop_at and cutoff, where Arc reports them, say when)."
	case "writes_stopped":
		area.Reason = "The licence is in the last days of its grace period: Arc refuses ingestion until it is renewed, and its other features stay on until cutoff."
	case "read_only":
		area.State, area.Reason = stateNotLicensed, "Arc reports the licence as read_only: it is no longer valid, so its features are off."
	case "expired":
		area.State, area.Reason = stateNotLicensed, "The licence has expired."
	case "unlicensed":
		area.State, area.Reason = stateNotLicensed, "A licence is configured, but Arc runs without it; Arc's log says why."
	default:
		if r := []rune(status); len(r) > 64 {
			status = string(r[:64]) + "…"
		}
		area.State, area.Reason = stateUnknown, "Arc reports licence status "+strconv.Quote(status)+", which arc-mcp does not know; data is Arc's answer."
	}
	return area
}

// unreportedLicence is the licence of an Arc whose /health does not report
// one: none configured, or an Arc before 26.09.1. In cluster mode the
// status carries the licence's validity (Arc has sent it since 26.02.1), so
// a licence that lapsed after startup is not read as active; otherwise an
// area that is licensed shows one in force.
func unreportedLicence(res EnterpriseResult) *EnterpriseArea {
	const lead = "This Arc's /health does not report its licence (Arc does since 26.09.1); "
	if cl, ok := res.Clustering.Data.(map[string]any); ok && res.Clustering.State == stateActive {
		if lic, ok := cl["license"].(map[string]any); ok {
			if valid, ok := lic["valid"].(bool); ok {
				data := map[string]any{"valid": valid, "tier": lic["tier"]}
				if !valid {
					return &EnterpriseArea{State: stateNotLicensed, Reason: lead + "the cluster status reports it as not valid.", Data: data}
				}
				return &EnterpriseArea{State: stateActive, Reason: lead + "this is the cluster status's.", Data: data}
			}
		}
	}
	for _, a := range []struct {
		name string
		area *EnterpriseArea
	}{{"clustering", res.Clustering}, {"cq_scheduler", res.CQScheduler}, {"retention_scheduler", res.RetentionScheduler}} {
		if licensed(a.area) {
			return &EnterpriseArea{State: stateActive, Reason: lead + a.name + " shows one in force."}
		}
	}
	return res.License
}

// licensed reports whether an area shows a valid licence: Arc runs it, has
// it off only in its configuration, or says the licence lacks its feature
// ("License does not include ...", "License tier does not include ...").
func licensed(a *EnterpriseArea) bool {
	return a.State == stateActive || a.State == stateNotEnabled || strings.HasPrefix(a.Reason, "License ")
}

// clusterKeys are the parts of Arc's cluster status capabilities keeps:
// the answering node, the cluster's size and the licence with its features.
var clusterKeys = []string{"local_node_id", "local_role", "cluster_name", "node_count", "healthy_count", "primary_writer_id", "license"}

// clusteringArea is capabilities' clustering part: unavailable when the
// read failed (failed) or obj is not Arc's cluster status.
func clusteringArea(obj map[string]any, notServed bool, failed string) *EnterpriseArea {
	switch {
	case failed != "":
		return &EnterpriseArea{State: stateUnavailable, Reason: failed}
	case notServed:
		return &EnterpriseArea{State: stateNotServed, Reason: notServedText(routeCluster)}
	}
	if state, reason, ok := disabledState(obj); ok {
		return &EnterpriseArea{State: state, Reason: reason}
	}
	if enabled, _ := obj["enabled"].(bool); !enabled {
		return &EnterpriseArea{State: stateUnavailable, Reason: errNotArc.Error()}
	}
	data := map[string]any{}
	for _, k := range clusterKeys {
		if v, ok := obj[k]; ok {
			data[k] = v
		}
	}
	return &EnterpriseArea{State: stateActive, Data: data}
}

// schedulerArea reads one scheduler of /api/v1/schedulers, and returns its
// CQ jobs sorted by cq_id (Arc builds the list from a map); unavailable when
// v is not Arc's answer.
func schedulerArea(v any) (*EnterpriseArea, []any) {
	m, ok := v.(map[string]any)
	if !ok {
		return &EnterpriseArea{State: stateUnavailable, Reason: errNotArc.Error()}, nil
	}
	if state, reason, ok := disabledState(m); ok {
		return &EnterpriseArea{State: state, Reason: reason}, nil
	}
	area := &EnterpriseArea{State: stateActive, Data: m}
	if valid, ok := m["license_valid"].(bool); ok && !valid {
		area.State, area.Reason = stateNotLicensed, "Arc reports license_valid false: the licence no longer includes this scheduler."
	} else if running, ok := m["running"].(bool); ok && !running {
		area.State, area.Reason = stateNotEnabled, "Arc reports the scheduler is not running."
	}
	jobs, _ := m["jobs"].([]any)
	jobs = slices.Clone(jobs)
	slices.SortStableFunc(jobs, func(a, b any) int { return compareNumbers(field(a, "cq_id"), field(b, "cq_id")) })
	return area, jobs
}

// fit renders res with set applied for the largest k <= n whose text fits
// maxChars (all n when maxChars is 0): lists lose whole items from the end.
// failed is set when not even k = 0 fits.
func fit(res EnterpriseResult, n int, set func(r *EnterpriseResult, k int), maxChars int) (EnterpriseResult, string, string) {
	render := func(k int) (EnterpriseResult, string, bool) {
		r := res
		if set != nil {
			set(&r, k)
		}
		text, err := compactJSON(r)
		if err != nil {
			log.Printf("arc_enterprise %s: rendering the answer: %v", res.View, err)
			return r, "", false
		}
		return r, text, maxChars <= 0 || len(text) <= maxChars
	}
	if r, text, ok := render(n); ok {
		return r, text, ""
	} else if text == "" {
		return r, "", errNotArc.Error()
	}
	if _, _, ok := render(0); !ok || n == 0 {
		return res, "", tooSmallText("Arc's "+res.View+" answer", maxChars)
	}
	// render(lo) fits and render(hi+1) does not: the largest k that fits.
	lo, hi := 0, n-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if _, _, ok := render(mid); ok {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	r, text, _ := render(lo)
	return r, text, ""
}

func cloneMap(m map[string]any) map[string]any {
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func field(v any, key string) any {
	if m, ok := v.(map[string]any); ok {
		return m[key]
	}
	return nil
}

func stringField(v any, key string) string {
	s, _ := field(v, key).(string)
	return s
}

// compareNumbers orders two of Arc's numbers (json.Number); anything else
// sorts after them, in the order given.
func compareNumbers(a, b any) int {
	x, errA := toInt(a)
	y, errB := toInt(b)
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return 1
	case errB != nil:
		return -1
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

func toInt(v any) (int64, error) {
	if n, ok := v.(json.Number); ok {
		return n.Int64()
	}
	return 0, errors.New("not an integer")
}

// enterpriseErrorText is the model's text for a failed read of route. Only a
// classified *arc.Error reaches client.UserMessage; the kinds whose shared
// text talks about databases, queries or read permission get text of their
// own, and so do 400 and 429, which Arc does not send on these routes.
func enterpriseErrorText(client *arc.Client, route string, err error) string {
	var ae *arc.Error
	if !errors.As(err, &ae) {
		log.Printf("arc_enterprise %s: %v", route, err)
		return "arc-mcp could not read Arc's " + route + " answer; see the arc-mcp log."
	}
	switch ae.Kind {
	case arc.KindTimeout:
		return "Arc did not answer " + route + " " + within(client.Timeout()) + ". Ask the user to check that the Arc server is not overloaded, then try again."
	case arc.KindNotFound:
		return "Arc answered HTTP 404 for " + route + ", so the configured Arc URL most likely does not point at Arc's API " +
			"(a path prefix, or a proxy in front of it). Ask the user to check it."
	case arc.KindForbidden, arc.KindScoped:
		return "Arc refused " + route + " (HTTP 403). Arc lets any valid token read it, so something other than the token's permissions refused it (a proxy in front of Arc, say). Ask the user to check."
	}
	switch ae.Status {
	case http.StatusTooManyRequests:
		return "The request for " + route + " was rate limited (HTTP 429). Try again later."
	case http.StatusBadRequest:
		return "The request for " + route + " was refused (HTTP 400); the answer is in the arc-mcp log."
	}
	return client.UserMessage(ae)
}
