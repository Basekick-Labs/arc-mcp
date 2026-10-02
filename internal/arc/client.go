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
)

func (e *Error) Error() string { return e.Kind + ": " + e.Detail }

// arcErrorFrom classifies a non-2xx HTTP response into an Error and logs
// the full body snippet to stderr. Callers receive only the Kind.
func arcErrorFrom(op string, statusCode int, snippet []byte) *Error {
	detail := fmt.Sprintf("%s: HTTP %d: %s", op, statusCode, strings.TrimSpace(string(snippet)))
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
	return &Error{Kind: kind, Status: statusCode, Detail: detail}
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
// It never leaks internal detail (stack traces, paths, Arc query context).
func UserMessage(err error) string {
	var ae *Error
	if ae, _ = err.(*Error); ae != nil { //nolint:errorlint
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
		case "too_large":
			return "Arc returned a response that was too large to process."
		case "parse":
			return "Arc returned an unexpected response format."
		case "query":
			return "Arc rejected the query — check the SQL syntax."
		default:
			return "Arc returned an error — see server logs for details."
		}
	}
	return fmt.Sprintf("Arc error: %v", err)
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
	Data            [][]interface{} `json:"data"`
	RowCount        int             `json:"row_count"`
	ExecutionTimeMs float64         `json:"execution_time_ms"`
	Timestamp       string          `json:"timestamp"`
	Error           string          `json:"error,omitempty"`
}

// Client is an HTTP client for the Arc REST API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new Arc API client.
func NewClient(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Health checks if the Arc instance is reachable.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to Arc at %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("arc health check failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ListDatabases returns all databases from Arc.
func (c *Client) ListDatabases(ctx context.Context) (*DatabaseListResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/databases", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, arcErrorFrom("list databases", resp.StatusCode, snippet)
	}

	var result DatabaseListResponse
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// ListMeasurements returns all measurements in a database.
func (c *Client) ListMeasurements(ctx context.Context, database string) (*MeasurementListResponse, error) {
	measurementsURL, err := c.buildURL("api", "v1", "databases", database, "measurements")
	if err != nil {
		return nil, fmt.Errorf("building measurements URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, measurementsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing measurements: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, arcErrorFrom("list measurements", resp.StatusCode, snippet)
	}

	var result MeasurementListResponse
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// buildURL safely composes the request URL by appending escaped path segments
// onto c.baseURL. This prevents path traversal or query/fragment injection via
// user-influenced segments (e.g., a malicious database name).
//
// Unlike path.Join, this never collapses ".." segments — url.PathEscape renders
// such content as literal characters in the path, so Arc receives exactly the
// segment the caller passed, encoded.
func (c *Client) buildURL(segments ...string) (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(u.Path, "/")
	var b strings.Builder
	b.WriteString(basePath)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	u.RawPath = b.String()
	u.Path = basePath
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-arc-database", database)
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(respBody, maxErrorBodyBytes))
		return nil, arcErrorFrom("query", resp.StatusCode, snippet)
	}

	var result QueryResponse
	if err := json.NewDecoder(respBody).Decode(&result); err != nil {
		detail := fmt.Sprintf("query: decoding response: %v", err)
		log.Printf("arc error: %s", detail)
		return nil, &Error{Kind: "parse", Detail: detail}
	}

	if !result.Success {
		detail := fmt.Sprintf("query: Arc error: %s", result.Error)
		log.Printf("arc error: %s", detail)
		return nil, &Error{Kind: "query", Detail: detail}
	}
	return &result, nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
