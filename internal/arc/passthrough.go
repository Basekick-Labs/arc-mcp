package arc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// Passthrough mode (#1): arc-mcp sends each request to Arc with the token of
// the MCP client that asked for it, instead of a token of its own.

// tokenKey carries the caller's token in a context.
type tokenKey struct{}

// WithToken returns ctx carrying the caller's Arc token, which a passthrough
// client sends with every request made with that context.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

func tokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(tokenKey{}).(string)
	return t
}

// passthroughState is a passthrough client's extra state.
type passthroughState struct {
	onRejected func(token string)
}

// EnablePassthrough switches the client to passthrough mode: every request
// carries the token from its context (WithToken), and a request without one
// is refused before it is sent. The client's own token, if any, is never
// used. onRejected, when not nil, is called with the token whenever Arc
// answers 401 to a request made with it, so a cache of verified tokens can
// drop it.
func (c *Client) EnablePassthrough(onRejected func(token string)) {
	c.token = ""
	c.passthrough = &passthroughState{onRejected: onRejected}
	next := c.httpClient.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.httpClient.Transport = rejectWatcher{next: next, onRejected: onRejected}
}

// Passthrough reports whether the client is in passthrough mode.
func (c *Client) Passthrough() bool { return c.passthrough != nil }

// rejectWatcher calls onRejected with the request's token when Arc answers
// 401.
type rejectWatcher struct {
	next       http.RoundTripper
	onRejected func(token string)
}

func (w rejectWatcher) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := w.next.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && w.onRejected != nil {
		if t := tokenFrom(req.Context()); t != "" {
			w.onRejected(t)
		}
	}
	return resp, err
}

// Caller is what Arc reports about a caller's token.
type Caller struct {
	// ID is Arc's token id, the identity Arc's own logs use.
	ID          int64
	Name        string
	Permissions []string
	// ExpiresAt is when the token expires; zero when it does not.
	ExpiresAt time.Time
}

// ErrCallerRejected is VerifyCaller's answer for a token Arc refuses.
var ErrCallerRejected = errors.New("arc rejected the token")

// ErrVerifyUnavailable is VerifyCaller's answer when Arc could not say
// whether the token is valid: unreachable, a server error, or an answer
// that is not Arc's verify response.
var ErrVerifyUnavailable = errors.New("arc could not verify the token")

// verifyTimeout bounds a token verification, whatever --timeout is.
const verifyTimeout = 5 * time.Second

// verifyClient is the HTTP client for verification: the client's
// transport (so tests' fakes apply), its own short timeout, and no
// redirects (a login page a proxy redirects to is not Arc's answer).
func (c *Client) verifyClient() *http.Client {
	return &http.Client{
		Transport:     c.httpClient.Transport,
		Timeout:       verifyTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// verifyAnswer is GET /api/v1/auth/verify's body.
type verifyAnswer struct {
	Valid     *bool `json:"valid"`
	TokenInfo *struct {
		ID          int64      `json:"id"`
		Name        string     `json:"name"`
		Permissions []string   `json:"permissions"`
		ExpiresAt   *time.Time `json:"expires_at"`
	} `json:"token_info"`
	Permissions []string `json:"permissions"`
}

// verify sends token to Arc's verify endpoint and returns the status and
// the decoded answer (nil when the body is not one).
func (c *Client) verify(ctx context.Context, token string) (int, *verifyAnswer, error) {
	verifyURL, err := c.buildURL("api", "v1", "auth", "verify")
	if err != nil {
		return 0, nil, fmt.Errorf("building the verify URL: %w", RedactError(err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("creating the verify request: %w", RedactError(err))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.verifyClient().Do(req)
	if err != nil {
		return 0, nil, RedactError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var ans verifyAnswer
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ans) != nil || ans.Valid == nil {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, &ans, nil
}

// VerifyCaller asks Arc whether token is valid. Only a 200 that says
// valid:true accepts it; a 401 or 403 is ErrCallerRejected; anything else
// (Arc unreachable, a 5xx, a 404 from an Arc that turned authentication
// off, an answer that is not Arc's) is ErrVerifyUnavailable, logged at
// most every 10s so a flood of bad requests cannot flood the log.
func (c *Client) VerifyCaller(ctx context.Context, token string) (Caller, error) {
	status, ans, err := c.verify(ctx, token)
	switch {
	case err != nil:
		logVerifyTrouble("Arc's verify endpoint is unreachable: %v", err)
		return Caller{}, ErrVerifyUnavailable
	case status == http.StatusOK && ans != nil && *ans.Valid && ans.TokenInfo != nil && ans.TokenInfo.ID > 0:
		caller := Caller{Permissions: ans.Permissions}
		if ti := ans.TokenInfo; ti != nil {
			caller.ID, caller.Name = ti.ID, ti.Name
			if len(caller.Permissions) == 0 {
				caller.Permissions = ti.Permissions
			}
			if ti.ExpiresAt != nil {
				caller.ExpiresAt = *ti.ExpiresAt
			}
		}
		return caller, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Caller{}, ErrCallerRejected
	case status == http.StatusNotFound:
		logVerifyTrouble("Arc's verify endpoint is gone (HTTP 404): Arc's authentication looks turned off; refusing callers until it is back on")
		return Caller{}, ErrVerifyUnavailable
	default:
		logVerifyTrouble("Arc's verify endpoint answered HTTP %d (Arc's verify response: %v); refusing callers until it answers as Arc does", status, ans != nil)
		return Caller{}, ErrVerifyUnavailable
	}
}

var verifyLog struct {
	sync.Mutex
	last time.Time
}

func logVerifyTrouble(format string, args ...any) {
	verifyLog.Lock()
	defer verifyLog.Unlock()
	if time.Since(verifyLog.last) < 10*time.Second {
		return
	}
	verifyLog.last = time.Now()
	log.Printf(format, args...)
}

// ErrAuthNotEnforced is ProbeAuthEnforced's answer when Arc does not check
// tokens: passthrough would forward tokens nobody verifies.
var ErrAuthNotEnforced = errors.New("arc does not enforce authentication")

// ProbeAuthEnforced checks that Arc enforces authentication, by verifying a
// random token: Arc must answer 401 with its own verify response
// (valid:false). A 404 means Arc runs with authentication off (its auth
// routes are not registered) and is ErrAuthNotEnforced. A 401 from a proxy
// in front of Arc, or any other answer, is an error too: it is not Arc
// saying the token is invalid. Unreachable is a plain error the caller can
// treat as a warning.
func (c *Client) ProbeAuthEnforced(ctx context.Context) error {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	status, ans, err := c.verify(ctx, "arc-mcp-probe-"+hex.EncodeToString(b))
	switch {
	case err != nil:
		return fmt.Errorf("reaching Arc's verify endpoint: %w", err)
	case status == http.StatusUnauthorized && ans != nil && !*ans.Valid:
		return nil
	case status == http.StatusNotFound:
		return ErrAuthNotEnforced
	case status == http.StatusOK && ans != nil && *ans.Valid:
		return fmt.Errorf("%w: it accepted a random token", ErrAuthNotEnforced)
	default:
		return fmt.Errorf("Arc's verify endpoint answered HTTP %d to a random token, not Arc's 401 (a proxy in front of Arc?)", status) //nolint:staticcheck // Arc is a name
	}
}
