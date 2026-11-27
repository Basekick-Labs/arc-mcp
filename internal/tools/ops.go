package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The operations tools (group ops, opt-in through --tools): arc_health and
// arc_metrics. They report on the Arc server itself, never on data stored in
// it, through Arc's fixed read-only routes. Every answer is compact JSON,
// attached as structured content too, with no output schema yet (#20 adds
// one for every tool). Their error texts are their own: the data tools'
// texts talk about SQL, databases and measurements.

// ProbeResult is one arc_health probe: the HTTP status Arc answered with and
// Arc's answer, or why there is none.
type ProbeResult struct {
	// HTTPStatus is the answer's status; absent when no answer came back.
	HTTPStatus int `json:"http_status,omitempty"`
	// Body is Arc's answer, numbers exact: set for a JSON object answered
	// with 200 or 503.
	Body any `json:"body,omitempty"`
	// Error says why there is no Body, in a fixed sentence of arc-mcp's.
	Error string `json:"error,omitempty"`
}

// HealthResult is arc_health's answer.
type HealthResult struct {
	Health ProbeResult `json:"health"`
	Ready  ProbeResult `json:"ready"`
}

// MetricsResult is arc_metrics' answer.
type MetricsResult struct {
	Family string `json:"family"`
	// Response is Arc's answer for a snapshot family, numbers exact.
	Response any `json:"response,omitempty"`
	// History is a history family's points.
	History *HistoryResult `json:"history,omitempty"`
}

// HistoryResult is the points of a history family.
type HistoryResult struct {
	// From and To are the timestamps of the oldest and the newest point
	// returned, as Arc wrote them: the window the points cover, which can
	// be shorter than the minutes asked for. Absent when there are none.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// TotalPoints is how many points Arc returned for the window.
	TotalPoints int `json:"total_points"`
	// SampledEvery is k: the points are every k-th of Arc's, counting back
	// from the newest, which is always kept. 1 means all of them.
	SampledEvery int `json:"sampled_every"`
	// Points are the points kept, oldest first, numbers exact.
	Points []any `json:"points"`
}

// metricsFamily is one arc_metrics family and the fixed Arc route behind it.
type metricsFamily struct {
	name    string
	path    string
	history bool
}

// metricsFamilies are arc_metrics' families, in the order the tool lists
// them. Arc has served every route here since 25.12.1.
var metricsFamilies = []metricsFamily{
	{"endpoints", "/api/v1/metrics/endpoints", false},
	{"query_pool", "/api/v1/metrics/query-pool", false},
	{"memory", "/api/v1/metrics/memory", false},
	{"counters", "/api/v1/metrics", false},
	{"system_history", "/api/v1/metrics/timeseries/system", true},
	{"application_history", "/api/v1/metrics/timeseries/application", true},
	{"api_history", "/api/v1/metrics/timeseries/api", true},
}

const (
	// defaultHistoryMinutes is the look-back when minutes is absent, the
	// same for every history family so their points line up.
	defaultHistoryMinutes = 10
	// maxHistoryMinutes is Arc's own bound on duration_minutes; outside
	// 1..1440 Arc silently uses 30.
	maxHistoryMinutes = 1440
	// historyPointBudget is the most points a history answer carries, the
	// same for every family so they are sampled at the same instants. Sized
	// from the widest point measured (Arc main's application family, about
	// 1.2 KB): even that family stays well under the default
	// --max-response-size.
	historyPointBudget = 20
)

func familyByName(name string) (metricsFamily, bool) {
	for _, f := range metricsFamilies {
		if f.name == name {
			return f, true
		}
	}
	return metricsFamily{}, false
}

// familyNames lists the families with history set as given.
func familyNames(history ...bool) []string {
	var names []string
	for _, f := range metricsFamilies {
		if len(history) == 0 || f.history == history[0] {
			names = append(names, f.name)
		}
	}
	return names
}

// orList renders names as "a, b and c".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// MetricsArgs is the input for arc_metrics.
type MetricsArgs struct {
	Family  string `json:"family"`
	Minutes int    `json:"minutes,omitempty"`
}

// metricsInputSchema is arc_metrics' input schema, written out because the
// family enum cannot be said in a struct tag. It is what inference from
// MetricsArgs gives, plus the enum. minutes has no default on purpose: the
// SDK would apply it to every call, snapshot families included, and those
// refuse minutes.
func metricsInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"family": map[string]any{
				"type":        "string",
				"description": "Which metrics: " + orList(familyNames()) + ".",
				"enum":        familyNames(),
			},
			"minutes": map[string]any{
				"type":        "integer",
				"description": "History families only: the look-back in minutes, 1 to 1440 (default " + strconv.Itoa(defaultHistoryMinutes) + ").",
			},
		},
		"required":             []string{"family"},
		"additionalProperties": false,
	}
}

// HealthArgs is the input for arc_health (no arguments).
type HealthArgs struct{}

// RegisterHealth registers arc_health.
func RegisterHealth(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "arc_health",
		Title:       "Check Arc health",
		Annotations: readOnly("Check Arc health"),
		Description: "Check whether the Arc server is alive (/health) and ready for traffic (/ready). " +
			"Returns each probe's HTTP status and Arc's answer; a not-ready answer (HTTP 503, with Arc's reason) is a result to report, not a failure. " +
			"Arc answers these probes without checking the token, so a healthy answer says nothing about whether the token works. " +
			"Describes only the Arc node at the configured URL.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ HealthArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		// One deadline for both probes, as describe_measurement does for
		// its statistics: a slow /health leaves /ready what remains.
		if t := client.Timeout(); t > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, t)
			defer cancel()
		}
		status, body, err := client.Probe(ctx, "/health")
		if err != nil && status == 0 {
			// No answer at all: there is nothing to report but that.
			return opsError(unreachableText("/health", client.Timeout(), err)), nil, nil
		}
		res := HealthResult{Health: probeResult(status, body, err)}
		res.Ready = probeResult(client.Probe(ctx, "/ready"))

		text, err := compactJSON(res)
		if err == nil && maxResponseChars > 0 && len(text) > maxResponseChars {
			// Only Arc's bodies are unbounded; drop them, keep the statuses.
			for _, p := range []*ProbeResult{&res.Health, &res.Ready} {
				if p.Body != nil {
					p.Body, p.Error = nil, "answer larger than --max-response-size allows"
				}
			}
			text, err = compactJSON(res)
		}
		if err != nil {
			log.Printf("arc_health: rendering the answer: %v", err)
			return opsError("arc-mcp could not render Arc's health answer; see the arc-mcp log."), nil, nil
		}
		if maxResponseChars > 0 && len(text) > maxResponseChars {
			return opsError(tooSmallText("Arc's health answer", maxResponseChars)), nil, nil
		}
		return opsResult(res, text), nil, nil
	})
}

// probeResult turns one probe's outcome into its fixed shape. Any HTTP answer
// is a state worth reporting; only Arc's own JSON reaches the model.
func probeResult(status int, body map[string]any, err error) ProbeResult {
	var ae *arc.Error
	switch {
	case err != nil && status == 0 && errors.As(err, &ae) && ae.Kind == arc.KindTimeout:
		return ProbeResult{Error: "timed out"}
	case err != nil && status == 0:
		return ProbeResult{Error: "not reached"}
	case err != nil:
		return ProbeResult{HTTPStatus: status, Error: "answer too large to be Arc's"}
	case status != 200 && status != 503:
		return ProbeResult{HTTPStatus: status, Error: statusText(status)}
	case body == nil:
		return ProbeResult{HTTPStatus: status, Error: "not Arc's answer: the body is not a JSON object"}
	}
	return ProbeResult{HTTPStatus: status, Body: body}
}

// statusText is the fixed sentence for a probe answered with an unexpected
// status. Arc's body is never shown for these.
func statusText(status int) string {
	switch {
	case status == 404:
		return "not found: the configured URL may not be Arc's base URL"
	case status == 401 || status == 403:
		return "refused"
	case status == 429:
		return "rate limited"
	case status >= 500:
		return "server error"
	}
	return "unexpected status"
}

// RegisterMetrics registers arc_metrics.
func RegisterMetrics(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "arc_metrics",
		Title:       "Read Arc metrics",
		Annotations: readOnly("Read Arc metrics"),
		Description: "Read the Arc server's own metrics (about Arc itself, not data stored in it). family: " +
			"endpoints (HTTP, ingest, query, buffer, storage, compaction and auth activity; start here), " +
			"query_pool (the DuckDB connection pool), memory (Go runtime and garbage collector), counters (every counter Arc keeps; the largest answer), " +
			"or system_history, application_history and api_history (points Arc records every few seconds). " +
			"minutes applies to the history families only: the look-back, 1 to 1440, default " + strconv.Itoa(defaultHistoryMinutes) + ". " +
			"Arc keeps 30 minutes of history unless metrics.timeseries_retention_minutes is raised, and at most " + strconv.Itoa(historyPointBudget) + " points come back: " +
			"sampled_every says which were kept (every k-th, the newest always), and from/to give the window they actually cover. " +
			"Describes only the Arc node at the configured URL.",
		InputSchema: metricsInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args MetricsArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		// The schema's enum already refuses an unknown family; this keeps
		// the handler safe on its own.
		fam, ok := familyByName(args.Family)
		if !ok {
			return invalidArgument("family must be one of " + orList(familyNames())), nil, nil
		}
		var query url.Values
		switch {
		case !fam.history && args.Minutes != 0:
			return invalidArgument("minutes applies only to " + orList(familyNames(true))), nil, nil
		case fam.history:
			minutes := args.Minutes
			if minutes == 0 {
				minutes = defaultHistoryMinutes
			}
			if minutes < 1 || minutes > maxHistoryMinutes {
				return invalidArgument("minutes must be between 1 and " + strconv.Itoa(maxHistoryMinutes)), nil, nil
			}
			query = url.Values{"duration_minutes": {strconv.Itoa(minutes)}}
		}

		v, err := client.GetJSON(ctx, fam.path, query)
		if err != nil {
			return opsError(metricsErrorText(fam.name, client.Timeout(), err)), nil, nil
		}
		var res MetricsResult
		var text string
		if fam.history {
			res, text, err = historyResult(fam.name, v, maxResponseChars)
		} else {
			res, text, err = snapshotResult(fam.name, v, maxResponseChars)
		}
		if err != nil {
			return opsError(err.Error()), nil, nil
		}
		return opsResult(res, text), nil, nil
	})
}

// errNotArc is the answer for a 200 whose body is not the shape Arc sends.
var errNotArc = errors.New(arc.UserMessage(&arc.Error{Kind: arc.KindParse}))

func snapshotResult(name string, v any, maxChars int) (MetricsResult, string, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return MetricsResult{}, "", errNotArc
	}
	res := MetricsResult{Family: name, Response: obj}
	text, err := compactJSON(res)
	if err != nil {
		log.Printf("arc_metrics %s: rendering the answer: %v", name, err)
		return MetricsResult{}, "", errNotArc
	}
	if maxChars > 0 && len(text) > maxChars {
		return MetricsResult{}, "", errors.New(tooSmallText("Arc's "+name+" answer", maxChars))
	}
	return res, text, nil
}

// historyResult reads the points out of Arc's time-series answer and
// samples them (see sampleHistory).
func historyResult(name string, v any, maxChars int) (MetricsResult, string, error) {
	env, ok := v.(map[string]any)
	if !ok {
		return MetricsResult{}, "", errNotArc
	}
	raw, ok := env["data"]
	if !ok {
		return MetricsResult{}, "", errNotArc
	}
	var points []any
	if raw != nil { // Arc sends null for a window with no points
		if points, ok = raw.([]any); !ok {
			return MetricsResult{}, "", errNotArc
		}
	}
	return sampleHistory(name, points, maxChars)
}

// sampleHistory keeps every k-th of points (oldest first, as Arc sends them)
// counting back from the newest, with k the smallest step that leaves at
// most historyPointBudget points and, when maxChars > 0, an answer of at most
// maxChars. Each point is rendered once, so the search costs about n·ln(n)
// additions for n points, not a render per candidate.
func sampleHistory(name string, points []any, maxChars int) (MetricsResult, string, error) {
	n := len(points)

	// result is the answer keeping every k-th point; with points false it
	// carries none, for sizing.
	result := func(k int, withPoints bool) MetricsResult {
		h := &HistoryResult{TotalPoints: n, SampledEvery: k, Points: []any{}}
		if n > 0 {
			oldest := (n - 1) % k
			h.From, h.To = timestampOf(points[oldest]), timestampOf(points[n-1])
			if withPoints {
				for i := oldest; i < n; i += k {
					h.Points = append(h.Points, points[i])
				}
			}
		}
		return MetricsResult{Family: name, History: h}
	}

	k := 1
	if n > historyPointBudget {
		k = (n + historyPointBudget - 1) / historyPointBudget
	}
	if maxChars > 0 {
		sizes := make([]int, n)
		for i, p := range points {
			s, err := compactJSON(p)
			if err != nil {
				log.Printf("arc_metrics %s: rendering a point: %v", name, err)
				return MetricsResult{}, "", errNotArc
			}
			sizes[i] = len(s)
		}
		for ; ; k++ {
			if n > 0 && k > n {
				return MetricsResult{}, "", errors.New(tooSmallText("Even the newest point of Arc's "+name+" answer", maxChars))
			}
			empty, err := compactJSON(result(k, false))
			if err != nil {
				log.Printf("arc_metrics %s: rendering the answer: %v", name, err)
				return MetricsResult{}, "", errNotArc
			}
			size, kept := len(empty), 0
			for i := n - 1; i >= 0; i -= k {
				size += sizes[i]
				kept++
			}
			if kept > 1 {
				size += kept - 1 // the commas between points
			}
			if size <= maxChars {
				break
			}
			if n == 0 {
				return MetricsResult{}, "", errors.New(tooSmallText("Arc's "+name+" answer", maxChars))
			}
		}
	}
	res := result(k, true)
	text, err := compactJSON(res)
	if err != nil {
		log.Printf("arc_metrics %s: rendering the answer: %v", name, err)
		return MetricsResult{}, "", errNotArc
	}
	return res, text, nil
}

// timestampOf is a point's timestamp as Arc wrote it, "" when it has none.
func timestampOf(point any) string {
	if m, ok := point.(map[string]any); ok {
		if ts, ok := m["timestamp"].(string); ok {
			return ts
		}
	}
	return ""
}

// compactJSON renders v as compact JSON without HTML escaping: the text the
// model reads, and what every size limit here measures.
func compactJSON(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// metricsErrorText is the model's text for a failed metrics read. Only an
// unwrapped *arc.Error reaches arc.UserMessage, whose fall-through prints the
// error itself (Arc's URL included); the kinds whose shared text talks about
// queries or databases get text of their own.
func metricsErrorText(name string, timeout time.Duration, err error) string {
	var ae *arc.Error
	if !errors.As(err, &ae) {
		log.Printf("arc_metrics %s: %v", name, err)
		return "arc-mcp could not read Arc's " + name + " metrics; see the arc-mcp log."
	}
	switch ae.Kind {
	case arc.KindTimeout:
		return "Arc did not answer " + within(timeout) + ". Ask the user to check that the Arc server is not overloaded, then try again."
	case arc.KindNotFound:
		return "Arc answered HTTP 404 for the " + name + " metrics, so the configured Arc URL most likely does not point at Arc's API " +
			"(a path prefix, or a proxy in front of it). Ask the user to check it."
	case arc.KindScoped:
		// A per-database refusal makes no sense for server metrics: say
		// what is true of any 403.
		return arc.UserMessage(&arc.Error{Kind: arc.KindForbidden})
	}
	return arc.UserMessage(ae)
}

// unreachableText is the model's text when probing path got no answer.
func unreachableText(path string, timeout time.Duration, err error) string {
	var ae *arc.Error
	if errors.As(err, &ae) && ae.Kind == arc.KindTimeout {
		return "Arc did not answer " + path + " " + within(timeout) + ". Ask the user to check that the Arc server is running and not overloaded."
	}
	return arc.UserMessage(&arc.Error{Kind: arc.KindNetwork})
}

func within(timeout time.Duration) string {
	if timeout <= 0 {
		return "in time"
	}
	return "within " + timeout.String() + " (arc-mcp's --timeout)"
}

// tooSmallText is the answer when even the smallest answer arc-mcp can give
// is over --max-response-size; what names it, as in "Arc's memory answer".
func tooSmallText(what string, maxChars int) string {
	return fmt.Sprintf("%s does not fit in arc-mcp's --max-response-size (%d characters). Ask the user to raise it.", what, maxChars)
}

func invalidArgument(msg string) *mcp.CallToolResult {
	return opsError("Invalid argument: " + msg)
}

func opsError(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

// opsResult is a successful answer: text is v's compact JSON, and v itself is
// the structured content, the same capped value, never Arc's raw body.
func opsResult(v any, text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: v}
}
