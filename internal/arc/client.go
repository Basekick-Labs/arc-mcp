// Package arc is an HTTP client for Arc's REST API.
package arc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Error is a classified error returned by the Arc client. Kind is a short
// machine-readable label; use it in tool handlers to produce user-facing
// messages without leaking internal detail (stack traces, paths, SQL context)
// to the LLM.
type Error struct {
	Kind   string // see the Kind* constants
	Status int    // HTTP status when the error came from a response, 0 otherwise
	Detail string // full detail — log to stderr, never send to LLM
	// Message is text safe to show the model, set where it can act on it:
	// Arc's fixed validation message for a refused request (HTTP 400), its
	// governance reason for a refused query (HTTP 429), the fixable part of
	// a failed query (redacted, see queryErrorText), a fixed hint for a
	// query that ran out of memory, timed out or lost data mid-run, or the
	// whole answer for a listing request that timed out (KindTimeout). Empty
	// otherwise; UserMessage then gives the generic text.
	Message string
}

// The Kind values Error can carry. The three permission kinds are kept
// apart because they need different answers: a bad token, a token
// missing a permission outright, and a token restricted to particular
// databases are not the same problem, and only the last one is resolved
// by naming a database.
const (
	// KindAuth is HTTP 401 — the token is missing, invalid or expired.
	KindAuth = "auth"
	// KindForbidden is HTTP 403 from Arc's coarse permission check — the
	// token does not carry the permission the route requires at all.
	KindForbidden = "forbidden"
	// KindScoped is HTTP 403 from Arc's per-database permission check —
	// the token can read, but not the database it asked about. Arc
	// answers the list-everything route this way rather than returning a
	// filtered list, so a scoped caller has to name a database it is
	// granted. This is an expected state, not a fault, and retrying it
	// without changing the database will fail the same way.
	KindScoped = "scoped"
	// KindUnavailable is HTTP 403 when Arc could not load the token's
	// grants and failed closed. Nothing about the token or the database
	// caused it, so it must not be reported as a permission problem.
	KindUnavailable = "permissions_unavailable"
	// KindNotFound is HTTP 404 — no such database or measurement.
	KindNotFound = "not_found"

	// KindServer is any other non-2xx response from Arc.
	KindServer = "server"
	// KindNetwork is a failure to reach Arc at all.
	KindNetwork = "network"
	// KindTooLarge is a response bigger than arc-mcp will read.
	KindTooLarge = "too_large"
	// KindParse is a response arc-mcp could not decode.
	KindParse = "parse"
	// KindQuery is a query Arc rejected.
	KindQuery = "query"
	// KindTimeout is a request arc-mcp gave up on after its own --timeout.
	KindTimeout = "timeout"
)

func (e *Error) Error() string { return e.Kind + ": " + e.Detail }

// arcErrorFrom classifies a non-2xx HTTP response into an Error and logs
// the body (its first 4 KiB) to stderr. Callers get the Kind, and Message when
// the model can act on it.
func arcErrorFrom(op string, statusCode int, snippet []byte) *Error {
	logged := snippet
	if len(logged) > maxErrorBodyBytes {
		logged = logged[:maxErrorBodyBytes]
	}
	detail := fmt.Sprintf("%s: HTTP %d: %s", op, statusCode, strings.TrimSpace(string(logged)))
	log.Printf("arc error: %s", detail)
	kind := KindServer
	switch {
	case statusCode == 401:
		kind = KindAuth
	case statusCode == 403 && isPermissionDataUnavailable(snippet):
		kind = KindUnavailable
	case statusCode == 403 && isScopedDenial(snippet):
		kind = KindScoped
	case statusCode == 403:
		kind = KindForbidden
	case statusCode == 404:
		kind = KindNotFound
	}
	e := &Error{Kind: kind, Status: statusCode, Detail: detail}
	switch {
	case statusCode == http.StatusBadRequest:
		e.Message = refusalText(snippet)
	case statusCode == http.StatusTooManyRequests:
		// Arc's governance refusal: a fixed reason such as "Rate limit
		// exceeded: 10 queries per minute" or "Daily query quota exceeded".
		e.Message = refusalText(snippet)
	case op == "query" && statusCode == http.StatusInternalServerError:
		if isOutOfMemory(snippet) {
			e.Message = outOfMemoryHint
		} else {
			e.Message = queryErrorFromBody(snippet)
		}
	case op == "query" && statusCode == http.StatusGatewayTimeout:
		e.Message = timedOutHint
	case op == "query" && statusCode == http.StatusServiceUnavailable && strings.Contains(errorField(snippet), "retry the query"):
		// Arc's fixed answer when data the query read was removed while it
		// ran (retention, compaction, a delete): the one failure a retry fixes.
		e.Message = "data it read was removed while it ran (retention, compaction or a delete). Run it again once."
	}
	return e
}

// outOfMemoryHint is the hint for a query that ran out of memory in Arc.
const outOfMemoryHint = "Arc ran out of memory running it. Aggregate (time_bucket, GROUP BY), narrow the time range, or select fewer columns."

// OutOfMemory reports whether err is a query Arc ran out of memory running.
// Tools whose SQL the model does not write use it to drop the hint, which
// is advice for the model's own query.
func OutOfMemory(err error) bool {
	var ae *Error
	return errors.As(err, &ae) && ae.Kind == KindServer && ae.Status == http.StatusInternalServerError && ae.Message == outOfMemoryHint
}

// timedOutHint is the hint for a query that ran out of time, in Arc or in
// arc-mcp's own --timeout.
const timedOutHint = "it timed out. Aggregate (time_bucket, GROUP BY) or narrow the time range, then try again."

// refusalText is the first line of the error message in Arc's JSON error
// body, at most 300 runes; "" for a body that is not Arc's (a proxy's HTML
// page, say), which must not reach the model.
func refusalText(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	msg, _, _ := strings.Cut(strings.TrimSpace(parsed.Error), "\n")
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	return strings.TrimSpace(msg)
}

// Arc sends 403 for three different things and distinguishes them only
// by message, so the message is what these read. The shapes Arc's read
// middleware and handler gates produce:
//
//	no permission for read on database 'x'               -> KindScoped
//	access denied: no read permission for database 'x'   -> KindScoped
//	access denied: no read permission for db.measurement -> KindScoped
//	access denied: no read permission to list databases  -> KindScoped
//	permission data unavailable                          -> KindUnavailable
//	token does not have 'read' permission                -> KindForbidden
//	Permission denied: read required                     -> KindForbidden
//
// Matching is by stable fragment, so a change to the permission word or
// to surrounding punctuation does not reclassify one as another. An
// unrecognised 403 is KindForbidden on purpose: naming the token's
// permissions is safe to say about any refusal, whereas telling a model
// to ask for a database when the token simply cannot read sends the
// conversation the wrong way.

// errorMessage pulls Arc's message out of a JSON error body, falling
// back to the raw bytes for a non-JSON one.
func errorMessage(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return string(body)
}

// isScopedDenial recognises every body that means "your grants do not
// cover what you asked for".
func isScopedDenial(body []byte) bool {
	msg := strings.ToLower(strings.TrimSpace(errorMessage(body)))
	switch {
	case strings.HasPrefix(msg, "no permission for ") && strings.Contains(msg, " on database "):
		return true
	case strings.HasPrefix(msg, "access denied: no ") &&
		(strings.Contains(msg, "permission for ") || strings.Contains(msg, "permission to list databases")):
		return true
	default:
		return false
	}
}

// isPermissionDataUnavailable recognises Arc's fail-closed answer when
// it could not load the token's grants. Checked before isScopedDenial so
// a server fault is never reported as a scoping state.
func isPermissionDataUnavailable(body []byte) bool {
	return strings.Contains(strings.ToLower(errorMessage(body)), "permission data unavailable")
}

// Scoped reports whether err is Arc's per-database permission denial —
// "you may read, but not that database". Tool handlers use it to answer
// with guidance the model can act on instead of a tool error.
func Scoped(err error) bool {
	var ae *Error
	return errors.As(err, &ae) && ae.Kind == KindScoped
}

// UserMessage returns a short, safe error message suitable for the LLM.
// It never leaks internal detail (stack traces, paths, Arc query context,
// Arc's URL).
func UserMessage(err error) string {
	var ae *Error
	if errors.As(err, &ae) && ae != nil {
		switch ae.Kind {
		case KindAuth:
			return "Arc rejected the token — it is missing, invalid or expired. Ask the user to check the token configured for this Arc server."
		case KindForbidden:
			return "Arc refused the request: this token does not carry the permission the endpoint requires. A token with read permission is needed."
		case KindScoped:
			return "This Arc token can only read specific databases, and not the one asked for. Ask the user which database to use, then call the tool again with that database."
		case KindUnavailable:
			return "Arc could not read its own permission data and refused the request rather than guessing. This is a server-side fault, not a problem with the token or the database — tell the user to check the Arc server log."
		case KindNotFound:
			return "The requested database or measurement was not found in Arc."
		case KindTooLarge:
			return "Arc returned a response that was too large to process."
		case KindParse:
			return "Arc returned an unexpected response format."
		case KindQuery:
			if ae.Message != "" {
				return "The query failed: " + ae.Message
			}
			return "Arc rejected the query — check the SQL syntax."
		case KindTimeout:
			if ae.Message != "" {
				return ae.Message
			}
			return "The query failed: " + timedOutHint
		case KindNetwork:
			return "Arc could not be reached. Ask the user to check that the Arc server is running at the configured URL."
		case KindServer:
			switch {
			case ae.Message != "" && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusTooManyRequests):
				return "Arc refused the request: " + ae.Message
			case ae.Message != "":
				// Only the query path sets Message on other statuses.
				return "The query failed: " + ae.Message
			}
			return "Arc returned an error — see server logs for details."
		default:
			return "Arc returned an error — see server logs for details."
		}
	}
	// Not classified by the client. Its text is never shown: a Go HTTP
	// error carries the request URL, and so Arc's address.
	log.Printf("arc error: %v", err)
	return "Arc returned an error — see server logs for details."
}

// maxArcResponseBytes caps how much we read from Arc in a single response to
// protect the MCP process from OOM if Arc (or a MITM) returns a huge body.
const maxArcResponseBytes = 64 << 20 // 64 MiB

// maxErrorBodyBytes caps how much of a non-2xx response body we include in
// error messages logged to stderr.
const maxErrorBodyBytes = 4 << 10 // 4 KiB

// DatabaseInfo matches Arc's API response for a database.
type DatabaseInfo struct {
	Name             string `json:"name"`
	MeasurementCount int    `json:"measurement_count"`
}

// DatabaseListResponse matches Arc's GET /api/v1/databases response.
type DatabaseListResponse struct {
	Databases []DatabaseInfo `json:"databases"`
	Count     int            `json:"count"`
}

// MeasurementInfo matches Arc's API response for a measurement.
type MeasurementInfo struct {
	Name      string `json:"name"`
	FileCount int    `json:"file_count,omitempty"`
}

// MeasurementListResponse matches Arc's GET /api/v1/databases/:name/measurements response.
type MeasurementListResponse struct {
	Database     string            `json:"database"`
	Measurements []MeasurementInfo `json:"measurements"`
	Count        int               `json:"count"`
}

// QueryRequest is the body sent to POST /api/v1/query.
type QueryRequest struct {
	SQL string `json:"sql"`
}

// QueryResponse matches Arc's POST /api/v1/query response.
type QueryResponse struct {
	Success         bool            `json:"success"`
	Columns         []string        `json:"columns"`
	Data            [][]interface{} `json:"data"` // numbers are json.Number, see Query
	RowCount        int             `json:"row_count"`
	ExecutionTimeMs float64         `json:"execution_time_ms"`
	Timestamp       string          `json:"timestamp"`
	Error           string          `json:"error,omitempty"`
	// RowsCapped is set when Arc's own governance row cap (RowCap) cut the
	// result. Truncated is set when Arc stopped streaming partway; the data
	// is real but incomplete, and TruncationReason says why (logged; only
	// FixableTruncation's redacted form reaches the model). Both still come
	// with success:true.
	RowsCapped       bool   `json:"rows_capped,omitempty"`
	RowCap           int64  `json:"row_cap,omitempty"`
	Truncated        bool   `json:"truncated,omitempty"`
	TruncationReason string `json:"truncation_reason,omitempty"`
}

// TokenVerifyResponse matches Arc's GET /api/v1/auth/verify response.
type TokenVerifyResponse struct {
	Valid       bool     `json:"valid"`
	Permissions []string `json:"permissions"`
}

// Client is an HTTP client for the Arc REST API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new Arc API client. baseURL is Arc's base URL; see
// normalizeBaseURL for what is done to it.
func NewClient(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: normalizeBaseURL(baseURL),
		token:   token,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// NewClientWithTransport is NewClient with the HTTP transport given. Tests
// use it to reach a fake Arc under a host name that resolves nowhere, so they
// can check that the name never reaches the model.
func NewClientWithTransport(baseURL, token string, timeout time.Duration, transport http.RoundTripper) *Client {
	c := NewClient(baseURL, token, timeout)
	c.httpClient.Transport = transport
	return c
}

// Timeout is the limit on each request to Arc (arc-mcp's --timeout).
func (c *Client) Timeout() time.Duration { return c.httpClient.Timeout }

// Health checks if the Arc instance is reachable.
func (c *Client) Health(ctx context.Context) error {
	healthURL, err := c.buildURL("health")
	if err != nil {
		return fmt.Errorf("building the health URL: %w", RedactError(err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", RedactError(err))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to Arc at %s: %w", RedactURL(c.baseURL), RedactError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("arc health check failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// VerifyToken returns the permissions reported by Arc for the configured token.
func (c *Client) VerifyToken(ctx context.Context) ([]string, error) {
	verifyURL, err := c.buildURL("api", "v1", "auth", "verify")
	if err != nil {
		return nil, fmt.Errorf("building the token verification URL: %w", RedactError(err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", RedactError(err))
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("verifying token: %w", RedactError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, arcErrorFrom("verify token", resp.StatusCode, snippet)
	}

	var result TokenVerifyResponse
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding token verification response: %w", err)
	}
	if !result.Valid {
		return nil, &Error{Kind: KindAuth, Detail: "verify token: Arc reported an invalid token"}
	}
	return result.Permissions, nil
}

// ListDatabases returns all databases from Arc.
func (c *Client) ListDatabases(ctx context.Context) (*DatabaseListResponse, error) {
	databasesURL, err := c.buildURL("api", "v1", "databases")
	if err != nil {
		return nil, loggedError(KindNetwork, fmt.Sprintf("list databases: building the URL: %v", RedactError(err)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, databasesURL, nil)
	if err != nil {
		return nil, loggedError(KindNetwork, fmt.Sprintf("list databases: creating the request: %v", RedactError(err)))
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, c.listingTransportError("list databases", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, arcErrorFrom("list databases", resp.StatusCode, snippet)
	}

	var result DatabaseListResponse
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return nil, loggedError(KindParse, fmt.Sprintf("list databases: decoding response: %v", err))
	}
	return &result, nil
}

// ListMeasurements returns all measurements in a database.
func (c *Client) ListMeasurements(ctx context.Context, database string) (*MeasurementListResponse, error) {
	measurementsURL, err := c.buildURL("api", "v1", "databases", database, "measurements")
	if err != nil {
		return nil, loggedError(KindNetwork, fmt.Sprintf("list measurements: building the URL: %v", RedactError(err)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, measurementsURL, nil)
	if err != nil {
		return nil, loggedError(KindNetwork, fmt.Sprintf("list measurements: creating the request: %v", RedactError(err)))
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, c.listingTransportError("list measurements", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, arcErrorFrom("list measurements", resp.StatusCode, snippet)
	}

	var result MeasurementListResponse
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return nil, loggedError(KindParse, fmt.Sprintf("list measurements: decoding response: %v", err))
	}
	return &result, nil
}

// listingTimedOutText is the answer for a listing request arc-mcp gave up on
// after timeout (its --timeout; 0 when unset). The query's own timeout text
// advises changing the SQL, which does not apply to a listing.
func listingTimedOutText(timeout time.Duration) string {
	within := "in time"
	if timeout > 0 {
		within = "within " + timeout.String() + " (arc-mcp's --timeout)"
	}
	return "Arc did not answer " + within + ". Ask the user to check that the Arc server is running and not overloaded, then try again."
}

// listingTransportError is transportError for the database and measurement
// listings, with their own timeout text.
func (c *Client) listingTransportError(op string, err error) *Error {
	e := transportError(op, err)
	if e.Kind == KindTimeout {
		e.Message = listingTimedOutText(c.httpClient.Timeout)
	}
	return e
}

// loggedError logs detail, as every other client error is logged, and
// returns it as an *Error of kind, so that its text (which can carry Arc's
// URL) reaches the model only through UserMessage.
func loggedError(kind, detail string) *Error {
	log.Printf("arc error: %s", detail)
	return &Error{Kind: kind, Detail: detail}
}

// RedactURL is raw without its userinfo, query string and fragment: the
// scheme://host/path form arc-mcp logs. The query and the userinfo can carry
// credentials (Arc reads a token from a query parameter, and Go sends
// userinfo as Basic auth). A URL that does not parse is cut at its query or
// fragment, and everything between :// and its last @ is dropped (a slash in
// a password is one reason such a URL does not parse).
func RedactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.User = nil
		u.RawQuery, u.ForceQuery = "", false
		u.Fragment, u.RawFragment = "", ""
		return u.String()
	}
	s, _, _ := strings.Cut(raw, "?")
	s, _, _ = strings.Cut(s, "#")
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return s
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	return scheme + "://" + rest
}

// RedactError is err with the URL its *url.Error quotes reduced by
// RedactURL, for logging. An error with no *url.Error in it is returned as
// it is; for one that wraps a *url.Error, that error alone is returned, so
// no outer text quotes the URL.
func RedactError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return &url.Error{Op: ue.Op, URL: RedactURL(ue.URL), Err: ue.Err}
}

// normalizeBaseURL drops the trailing slashes from the path of Arc's base
// URL, so http://h:8000/ and http://h:8000/arc/ give /api/v1/... and
// /arc/api/v1/... rather than //api/v1/..., which Arc answers with 404. The
// path prefix and a query string are kept; an escaped slash (%2F) is part of
// the prefix, not a trailing slash. A URL that does not parse is kept as
// given: main validates it first, and buildURL reports it.
func normalizeBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return raw
	}
	u.Path, u.RawPath = path, escaped
	return u.String()
}

// buildURL safely composes the request URL by appending escaped path segments
// onto c.baseURL. This prevents path traversal or query/fragment injection via
// user-influenced segments (e.g., a malicious database name). Every request
// is built here, so the base URL's path prefix and query string are kept;
// c.baseURL has no trailing slash (NewClient), so none is doubled.
//
// Unlike path.Join, this never collapses ".." segments — url.PathEscape renders
// such content as literal characters in the path, so Arc receives exactly the
// segment the caller passed, encoded.
func (c *Client) buildURL(segments ...string) (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	// NewClient has trimmed it already; trimmed here too so that no other
	// way of setting c.baseURL can bring back a doubled slash.
	basePath := strings.TrimRight(u.EscapedPath(), "/")
	if u.Path, err = url.PathUnescape(basePath); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(basePath)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	u.RawPath = b.String()
	for _, s := range segments {
		u.Path += "/" + s
	}
	return u.String(), nil
}

// Query executes a SQL query against Arc.
func (c *Client) Query(ctx context.Context, database, sql string) (*QueryResponse, error) {
	body, err := json.Marshal(QueryRequest{SQL: sql})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	queryURL, err := c.buildURL("api", "v1", "query")
	if err != nil {
		return nil, loggedError(KindNetwork, fmt.Sprintf("query: building the URL: %v", RedactError(err)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, queryURL, bytes.NewReader(body))
	if err != nil {
		return nil, loggedError(KindNetwork, fmt.Sprintf("query: creating the request: %v", RedactError(err)))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-arc-database", database)
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, transportError("query", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(respBody, maxQueryErrorBodyBytes))
		return nil, arcErrorFrom("query", resp.StatusCode, snippet)
	}

	var result QueryResponse
	if err := decodeExact(respBody, &result); err != nil {
		detail := fmt.Sprintf("query: decoding response: %v", err)
		log.Printf("arc error: %s", detail)
		return nil, &Error{Kind: KindParse, Detail: detail}
	}

	if !result.Success {
		detail := fmt.Sprintf("query: Arc error: %s", result.Error)
		log.Printf("arc error: %s", detail)
		return nil, &Error{Kind: KindQuery, Detail: detail, Message: queryErrorText(result.Error)}
	}
	return &result, nil
}

// transportError classifies a request that got no answer from Arc: arc-mcp's
// own --timeout, or anything else that kept the request from reaching Arc.
// The raw error carries Arc's URL, so it goes to the log only, never to the
// model.
func transportError(op string, err error) *Error {
	detail := fmt.Sprintf("%s: %v", op, RedactError(err))
	log.Printf("arc error: %s", detail)
	var ue *url.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ue) && ue.Timeout() {
		return &Error{Kind: KindTimeout, Detail: detail}
	}
	return &Error{Kind: KindNetwork, Detail: detail}
}

// decodeExact decodes one JSON value from r into v, keeping every number as
// the exact text Arc sent (json.Number) wherever v holds interface{} values.
// Decoding into interface{} would otherwise make every number a float64:
// integers above 2^53 change value, and counts print as 1.783416e+06.
func decodeExact(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return dec.Decode(v)
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
