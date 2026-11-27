package arc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// GetJSON reads a read-only Arc endpoint and returns its JSON response
// unchanged. A 503 from /ready is a valid readiness snapshot, not a failure.
// The endpoint must be an application-owned path; query values are encoded.
func (c *Client) GetJSON(ctx context.Context, endpoint string, query url.Values) (json.RawMessage, error) {
	if !strings.HasPrefix(endpoint, "/") || strings.ContainsAny(endpoint, "?#") {
		return nil, fmt.Errorf("invalid Arc endpoint")
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
		return nil, fmt.Errorf("creating Arc request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, observabilityRequestError("requesting "+endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	readinessUnavailable := endpoint == "/ready" && resp.StatusCode == http.StatusServiceUnavailable
	if resp.StatusCode != http.StatusOK && !readinessUnavailable {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		apiErr := arcErrorFrom(endpoint, resp.StatusCode, snippet)
		if resp.StatusCode == http.StatusNotFound {
			apiErr.Kind = KindUnsupported
		}
		return nil, apiErr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArcResponseBytes+1))
	if err != nil {
		return nil, observabilityRequestError("reading "+endpoint+" response", err)
	}
	if len(body) > maxArcResponseBytes {
		return nil, &Error{Kind: KindTooLarge, Detail: fmt.Sprintf("%s response exceeded %d bytes", endpoint, maxArcResponseBytes)}
	}
	if !json.Valid(body) {
		return nil, &Error{Kind: KindParse, Detail: fmt.Sprintf("%s returned invalid JSON", endpoint)}
	}
	return json.RawMessage(body), nil
}

func observabilityRequestError(operation string, err error) *Error {
	detail := fmt.Sprintf("%s: %v", operation, err)
	log.Printf("arc error: %s", detail)
	var urlErr *url.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &urlErr) && urlErr.Timeout() {
		return &Error{Kind: KindTimeout, Detail: detail}
	}
	return &Error{Kind: KindNetwork, Detail: detail}
}
