// Package arc is an HTTP client for Arc's REST API.
package arc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ArcError is a classified error returned by the Arc client. Kind is a short
// machine-readable label; use it in tool handlers to produce user-facing
// messages without leaking internal detail (stack traces, paths, SQL context)
// to the LLM.
type ArcError struct {
	Kind   string // "auth", "not_found", "server", "network", "too_large", "parse", "query"
	Detail string // full detail — log to stderr, never send to LLM
}

func (e *ArcError) Error() string { return e.Kind + ": " + e.Detail }

// arcErrorFrom classifies a non-2xx HTTP response into an ArcError and logs
// the full body snippet to stderr. Callers receive only the Kind.
func arcErrorFrom(op string, statusCode int, snippet []byte) *ArcError {
	detail := fmt.Sprintf("%s: HTTP %d: %s", op, statusCode, strings.TrimSpace(string(snippet)))
	log.Printf("arc error: %s", detail)
	kind := "server"
	switch {
	case statusCode == 401 || statusCode == 403:
		kind = "auth"
	case statusCode == 404:
		kind = "not_found"
	}
	return &ArcError{Kind: kind, Detail: detail}
}

// UserMessage returns a short, safe error message suitable for the LLM.
// It never leaks internal detail (stack traces, paths, Arc query context).
func UserMessage(err error) string {
	var ae *ArcError
	if asArcError(err, &ae) { //nolint:errorlint
		switch ae.Kind {
		case "auth":
			return "Arc authentication failed — check the API token."
		case "not_found":
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

// asArcError is a helper that avoids importing errors in this package.
func asArcError(err error, target **ArcError) bool {
	if err == nil {
		return false
	}
	if ae, ok := err.(*ArcError); ok {
		*target = ae
		return true
	}
	return false
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
		return nil, &ArcError{Kind: "parse", Detail: detail}
	}

	if !result.Success {
		detail := fmt.Sprintf("query: Arc error: %s", result.Error)
		log.Printf("arc error: %s", detail)
		return nil, &ArcError{Kind: "query", Detail: detail}
	}
	return &result, nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
