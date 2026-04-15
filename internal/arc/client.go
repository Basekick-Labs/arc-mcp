package arc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Arc health check failed: HTTP %d", resp.StatusCode)
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
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, fmt.Errorf("listing databases: HTTP %d: %s", resp.StatusCode, string(snippet))
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
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, fmt.Errorf("listing measurements: HTTP %d: %s", resp.StatusCode, string(snippet))
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
	defer resp.Body.Close()
	respBody := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(respBody, maxErrorBodyBytes))
		return nil, fmt.Errorf("query failed: HTTP %d: %s", resp.StatusCode, string(snippet))
	}

	var result QueryResponse
	if err := json.NewDecoder(respBody).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if !result.Success {
		return nil, fmt.Errorf("query error: %s", result.Error)
	}
	return &result, nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
