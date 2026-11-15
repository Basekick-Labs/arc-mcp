package arc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GetJSON reads a read-only Arc observability endpoint and returns its JSON
// response unchanged. The only non-2xx status treated as a valid snapshot is
// 503 from /ready: readiness is a state report, not a transport failure.
//
// endpoint must be an application-owned absolute path, not user input. Query
// values are encoded by net/url so filters cannot alter the request path.
func (c *Client) GetJSON(ctx context.Context, endpoint string, query url.Values) (json.RawMessage, error) {
	if !strings.HasPrefix(endpoint, "/") || strings.ContainsAny(endpoint, "?#") {
		return nil, fmt.Errorf("invalid observability endpoint")
	}

	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing Arc URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
	u.RawPath = ""
	if query != nil {
		u.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating observability request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting Arc observability endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	readinessUnavailable := endpoint == "/ready" && resp.StatusCode == http.StatusServiceUnavailable
	if resp.StatusCode != http.StatusOK && !readinessUnavailable {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		apiErr := arcErrorFrom(endpoint, resp.StatusCode, snippet)
		if resp.StatusCode == http.StatusNotFound {
			apiErr.Kind = "unsupported"
		} else if endpoint == "/api/v1/logs" && resp.StatusCode == http.StatusForbidden {
			apiErr.Kind = "admin_required"
		} else if resp.StatusCode == http.StatusForbidden {
			apiErr.Kind = "forbidden"
		}
		return nil, apiErr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArcResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading Arc observability response: %w", err)
	}
	if len(body) > maxArcResponseBytes {
		return nil, &Error{Kind: "too_large", Detail: fmt.Sprintf("%s response exceeded %d bytes", endpoint, maxArcResponseBytes)}
	}
	if !json.Valid(body) {
		return nil, &Error{Kind: "parse", Detail: fmt.Sprintf("%s returned invalid JSON", endpoint)}
	}
	return json.RawMessage(body), nil
}
