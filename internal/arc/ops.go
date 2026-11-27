package arc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// opsURL builds the URL of one of Arc's fixed operations routes on c's base
// URL, so a trailing slash or a path prefix in the configured URL (a proxy
// serving Arc under /arc, say) is kept rather than doubled or dropped. path
// is a constant of arc-mcp's, never model input.
func (c *Client) opsURL(path string, query url.Values) (string, error) {
	u, err := c.buildURL(strings.Split(strings.TrimPrefix(path, "/"), "/")...)
	if err != nil {
		return "", err
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u, nil
}

// opsGet sends a GET for one of Arc's fixed operations routes. A request that
// gets no answer comes back as a classified *Error (KindTimeout or
// KindNetwork) whose text holds no URL.
func (c *Client) opsGet(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	u, err := c.opsURL(path, query)
	if err != nil {
		return nil, &Error{Kind: KindNetwork, Detail: fmt.Sprintf("GET %s: building the URL: %v", path, err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &Error{Kind: KindNetwork, Detail: fmt.Sprintf("GET %s: creating the request: %v", path, err)}
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, transportError("GET "+path, err)
	}
	return resp, nil
}

// GetJSON reads one of Arc's fixed JSON routes (the /api/v1/metrics family)
// and returns its 200 answer decoded with exact numbers (see decodeExact):
// map[string]any for an object, []any for an array, json.Number for every
// number.
//
// Every failure is a bare *Error, never wrapped, so callers can classify it:
// a non-2xx answer as arcErrorFrom does, a body over the 64 MiB cap as
// KindTooLarge, and a body that is not JSON as KindParse.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values) (any, error) {
	resp, err := c.opsGet(ctx, path, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxArcResponseBytes+1)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
		return nil, arcErrorFrom("GET "+path, resp.StatusCode, snippet)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, transportError("GET "+path, err)
	}
	if len(raw) > maxArcResponseBytes {
		return nil, &Error{Kind: KindTooLarge, Detail: fmt.Sprintf("GET %s: answer over %d bytes", path, maxArcResponseBytes)}
	}
	var v any
	if err := decodeExact(bytes.NewReader(raw), &v); err != nil {
		return nil, &Error{Kind: KindParse, Detail: fmt.Sprintf("GET %s: decoding the answer: %v", path, err)}
	}
	return v, nil
}

// maxProbeBodyBytes caps how much of a /health or /ready answer is read.
// Arc's answers are a few hundred bytes; anything near this is not Arc's.
const maxProbeBodyBytes = 1 << 20 // 1 MiB

// Probe sends a GET for one of Arc's public probe routes (/health, /ready)
// and returns whatever HTTP answer came back: the status, and the body when
// it is a JSON object (numbers exact), nil otherwise — a proxy's error page,
// say, which must not reach the model. Any status is an answer here, a 503
// from /ready included: the caller decides what it means.
//
// err is set only when no answer came back (a bare *Error of KindTimeout or
// KindNetwork, its text free of the URL), or when the body is larger than
// any answer of Arc's (KindTooLarge).
func (c *Client) Probe(ctx context.Context, path string) (status int, body map[string]any, err error) {
	resp, err := c.opsGet(ctx, path, nil)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBodyBytes+1))
	if err != nil {
		return 0, nil, transportError("GET "+path, err)
	}
	if len(raw) > maxProbeBodyBytes {
		return resp.StatusCode, nil, &Error{Kind: KindTooLarge, Status: resp.StatusCode, Detail: fmt.Sprintf("GET %s: answer over %d bytes", path, maxProbeBodyBytes)}
	}
	var v any
	if decodeExact(bytes.NewReader(raw), &v) == nil {
		body, _ = v.(map[string]any)
	}
	return resp.StatusCode, body, nil
}
