package callerauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/basekick-labs/arc-mcp/internal/httpserve"
	"github.com/basekick-labs/arc-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeArc knows tokens by name (token -> id). Verify answers as Arc does;
// a query answers with the token it was sent, as its one value, so a test
// can see which token reached Arc for which caller.
type fakeArc struct {
	mu         sync.Mutex
	tokens     map[string]int64
	perms      map[string][]string
	verifies   atomic.Int32
	queries    atomic.Int32
	enterprise atomic.Int32
	// verifyStatus, when set, is every verify call's answer.
	verifyStatus int
	verifyGate   chan struct{}
}

func (f *fakeArc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	id, ok := f.tokens[token]
	perms := f.perms[token]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/v1/auth/verify" {
		f.verifies.Add(1)
		if f.verifyGate != nil {
			<-f.verifyGate
		}
		if f.verifyStatus != 0 {
			w.WriteHeader(f.verifyStatus)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"valid":false,"error":"Invalid or expired token"}`)
			return
		}
		if perms == nil {
			perms = []string{"read"}
		}
		b, _ := json.Marshal(map[string]any{"valid": true, "token_info": map[string]any{"id": id, "name": "t", "permissions": perms}, "permissions": perms})
		_, _ = w.Write(b)
		return
	}
	if r.URL.Path == "/api/v1/cluster" {
		// arc_enterprise: a standalone Arc's answer, its reason naming the
		// token that reached it.
		f.enterprise.Add(1)
		b, _ := json.Marshal(map[string]any{"enabled": false, "mode": "standalone", "reason": "seen:" + token})
		_, _ = w.Write(b)
		return
	}
	f.queries.Add(1)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"success":false,"error":"Invalid or expired token"}`)
		return
	}
	b, _ := json.Marshal(map[string]any{"success": true, "columns": []string{"token"}, "data": [][]any{{"seen:" + token}}, "row_count": 1})
	_, _ = w.Write(b)
}

func (f *fakeArc) revoke(token string) {
	f.mu.Lock()
	delete(f.tokens, token)
	f.mu.Unlock()
}

// setup is arc-mcp in passthrough against fake: the client, the verifier,
// and the HTTP endpoint.
func setup(t *testing.T, fake *fakeArc) (*Verifier, *httptest.Server) {
	t.Helper()
	arcSrv := httptest.NewServer(fake)
	t.Cleanup(arcSrv.Close)
	client := arc.NewClient(arcSrv.URL, "", 5*time.Second)
	v := New(client)
	client.EnablePassthrough(v.Forget)
	server := mcp.NewServer(&mcp.Implementation{Name: "arc-mcp", Version: "test"}, nil)
	tools.RegisterAll(server, client, tools.Groups{Data: true, Enterprise: true}, 500, 50_000)
	server.AddReceivingMiddleware(Middleware)
	srv := httptest.NewServer(httpserve.Handler(server, httpserve.Options{Verify: v.Verify, MaxConcurrent: 8}))
	t.Cleanup(srv.Close)
	return v, srv
}

// call posts one tools/call of query with token and returns the status and
// the body.
func call(t *testing.T, srv *httptest.Server, token string) (int, string) {
	t.Helper()
	return callTool(t, srv, token, `{"name":"query","arguments":{"database":"db","sql":"SELECT token FROM t"}}`)
}

// callTool posts one tools/call with params and token.
func callTool(t *testing.T, srv *httptest.Server, token, params string) (int, string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + params + `}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+httpserve.Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Each caller's query reaches Arc with that caller's token: many callers at
// once, never one another's.
func TestEachCallerQueriesWithItsOwnToken(t *testing.T) {
	fake := &fakeArc{tokens: map[string]int64{}}
	for i := 0; i < 8; i++ {
		fake.tokens[fmt.Sprintf("tok-%d", i)] = int64(i + 1)
	}
	_, srv := setup(t, fake)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		for j := 0; j < 5; j++ {
			wg.Add(1)
			go func(tok string) {
				defer wg.Done()
				status, body := call(t, srv, tok)
				if status != 200 || !strings.Contains(body, "seen:"+tok+" ") && !strings.Contains(body, "seen:"+tok+"\"") {
					t.Errorf("%s: %d %s", tok, status, body)
				}
				for k := 0; k < 8; k++ {
					if other := fmt.Sprintf("seen:tok-%d", k); other != "seen:"+tok && strings.Contains(body, other+" ") {
						t.Errorf("%s saw %s", tok, other)
					}
				}
			}(fmt.Sprintf("tok-%d", i))
		}
	}
	wg.Wait()
}

// arc_enterprise in passthrough: each caller's read reaches Arc with that
// caller's token, and a caller without a valid one never reaches it.
func TestEnterpriseReadsWithEachCallersToken(t *testing.T) {
	fake := &fakeArc{tokens: map[string]int64{}}
	for i := 0; i < 4; i++ {
		fake.tokens[fmt.Sprintf("tok-%d", i)] = int64(i + 1)
	}
	_, srv := setup(t, fake)
	params := `{"name":"arc_enterprise","arguments":{"view":"cluster"}}`
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			status, body := callTool(t, srv, tok, params)
			if status != 200 || !strings.Contains(body, `seen:`+tok+`\"`) || !strings.Contains(body, `not_enabled`) {
				t.Errorf("%s: %d %s", tok, status, body)
			}
		}(fmt.Sprintf("tok-%d", i))
	}
	wg.Wait()
	before := fake.enterprise.Load()
	if status, _ := callTool(t, srv, "bad", params); status != http.StatusUnauthorized || fake.enterprise.Load() != before {
		t.Errorf("a bad token: status %d, %d reads reached Arc", status, fake.enterprise.Load()-before)
	}
}

func TestNoOrBadTokenNeverReachesArc(t *testing.T) {
	fake := &fakeArc{tokens: map[string]int64{"good": 1}}
	_, srv := setup(t, fake)
	for _, tok := range []string{"", "bad", "go od"} {
		if status, _ := call(t, srv, tok); status != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, status)
		}
	}
	if fake.queries.Load() != 0 {
		t.Errorf("%d queries reached Arc", fake.queries.Load())
	}
}

// Arc unable to verify is 503, not 401: the token may be fine.
func TestVerifyUnavailableIs503(t *testing.T) {
	for _, status := range []int{500, 404} {
		fake := &fakeArc{tokens: map[string]int64{"good": 1}, verifyStatus: status}
		_, srv := setup(t, fake)
		if got, _ := call(t, srv, "good"); got != http.StatusServiceUnavailable {
			t.Errorf("verify %d: status %d, want 503", status, got)
		}
	}
}

func TestCache(t *testing.T) {
	fake := &fakeArc{tokens: map[string]int64{"good": 1}}
	v, srv := setup(t, fake)
	now := time.Now()
	v.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if status, _ := call(t, srv, "good"); status != 200 {
			t.Fatalf("status %d", status)
		}
	}
	if n := fake.verifies.Load(); n != 1 {
		t.Errorf("%d verifications for three calls, want 1", n)
	}
	// A rejected token is refused again without asking Arc, for rejectTTL.
	call(t, srv, "bad")
	if status, _ := call(t, srv, "bad"); status != http.StatusUnauthorized {
		t.Errorf("rejected token: status %d", status)
	}
	if n := fake.verifies.Load(); n != 2 {
		t.Errorf("%d verifications, want 2", n)
	}
	// After the TTL, Arc is asked again.
	now = now.Add(cacheTTL + time.Second)
	call(t, srv, "good")
	if n := fake.verifies.Load(); n != 3 {
		t.Errorf("%d verifications after the TTL, want 3", n)
	}
	// A token Arc rejects on a query is dropped from the cache at once.
	fake.revoke("good")
	if status, _ := call(t, srv, "good"); status != 200 {
		t.Errorf("cached token: status %d (the tool call fails at Arc, the request does not)", status)
	}
	if status, _ := call(t, srv, "good"); status != http.StatusUnauthorized {
		t.Errorf("after Arc rejected it: status %d, want 401", status)
	}
}

func TestCacheHoldsNoTokenAndHonoursExpiry(t *testing.T) {
	v := New(arc.NewClient("http://127.0.0.1:1", "", time.Second))
	now := time.Now()
	v.remember(key("secret-token"), "arc-token:1", arc.Caller{ID: 1, ExpiresAt: now.Add(10 * time.Second)}, now, 0)
	for k, e := range v.cache {
		if bytes.Contains(k[:], []byte("secret-token")) {
			t.Error("the cache key holds the token")
		}
		if !e.until.Equal(now.Add(10 * time.Second)) {
			t.Errorf("cached until %v, want the token's expiry", e.until)
		}
	}
}

// More verifications in flight than maxVerifying: the rest get 503 at once.
func TestVerifySlots(t *testing.T) {
	fake := &fakeArc{tokens: map[string]int64{}, verifyGate: make(chan struct{})}
	v, _ := setup(t, fake)
	var wg sync.WaitGroup
	for i := 0; i < maxVerifying; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = v.Verify(context.Background(), fmt.Sprintf("t%d", i))
		}(i)
	}
	waitFor(t, func() bool { return fake.verifies.Load() >= maxVerifying })
	start := time.Now()
	if _, err := v.Verify(context.Background(), "one-more"); !errors.Is(err, httpserve.ErrUnavailable) {
		t.Errorf("verification past the slots: %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d < slotWait || d > slotWait+time.Second {
		t.Errorf("gave up after %s, want about %s", d, slotWait)
	}
	close(fake.verifyGate)
	wg.Wait()
}

// Parallel first requests with one token share one verification.
func TestSameTokenSharesAVerification(t *testing.T) {
	fake := &fakeArc{tokens: map[string]int64{"good": 1}, verifyGate: make(chan struct{})}
	v, _ := setup(t, fake)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(context.Background(), "good"); err != nil {
				failed.Add(1)
			}
		}()
	}
	for fake.verifies.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(fake.verifyGate)
	wg.Wait()
	if n := fake.verifies.Load(); n != 1 || failed.Load() != 0 {
		t.Errorf("%d verifications, %d failures, want 1 and 0", n, failed.Load())
	}
}

func TestOverScopedWarnedOncePerToken(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	fake := &fakeArc{tokens: map[string]int64{"admin": 9}, perms: map[string][]string{"admin": {"read", "admin"}}}
	v, _ := setup(t, fake)
	for i := 0; i < 3; i++ {
		v.Forget("admin")
		if id, err := v.Verify(context.Background(), "admin"); err != nil || id != "arc-token:9" {
			t.Fatalf("id %q err %v", id, err)
		}
	}
	if n := strings.Count(buf.String(), "caller arc-token:9 carries admin"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, buf.String())
	}
}

// The access log names the caller by Arc's token id, never the token.
func TestAccessLogNamesTheCaller(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	fake := &fakeArc{tokens: map[string]int64{"s3cret": 42}}
	_, srv := setup(t, fake)
	call(t, srv, "s3cret")
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "caller=arc-token:42") || strings.Contains(out, "s3cret") {
		t.Errorf("access log:\n%s", out)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Without an HTTP header (stdio), the middleware leaves the context alone.
func TestMiddlewareWithoutHeader(t *testing.T) {
	var got context.Context
	h := Middleware(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		got = ctx
		return nil, nil
	})
	ctx := context.Background()
	if _, err := h(ctx, "tools/call", &mcp.CallToolRequest{}); err != nil {
		t.Fatal(err)
	}
	if got != ctx {
		t.Error("the context changed")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

// Past maxWaiting verifications waiting for a slot, 503 at once.
func TestWaitingIsBounded(t *testing.T) {
	gate := make(chan struct{})
	v := New(arc.NewClient("http://127.0.0.1:1", "", time.Second))
	var calls atomic.Int32
	v.verifyCaller = func(context.Context, string) (arc.Caller, error) {
		calls.Add(1)
		<-gate
		return arc.Caller{ID: 1}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < maxVerifying+maxWaiting; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, _ = v.Verify(context.Background(), fmt.Sprintf("w%d", i)) }(i)
	}
	waitFor(t, func() bool { return int(calls.Load()) == maxVerifying && len(v.waiting) == maxWaiting })
	start := time.Now()
	if _, err := v.Verify(context.Background(), "one-too-many"); !errors.Is(err, httpserve.ErrUnavailable) || time.Since(start) > 100*time.Millisecond {
		t.Errorf("past the waiting bound: %v after %s, want ErrUnavailable at once", err, time.Since(start))
	}
	close(gate)
	wg.Wait()
}

// Tokens that differ only late are separate: the cache and the shared
// verification are keyed by the whole token.
func TestTokensAreKeyedWhole(t *testing.T) {
	gate := make(chan struct{})
	v := New(arc.NewClient("http://127.0.0.1:1", "", time.Second))
	v.verifyCaller = func(_ context.Context, token string) (arc.Caller, error) {
		<-gate
		switch token {
		case "token-aaaaaaaa-1":
			return arc.Caller{ID: 1}, nil
		case "token-aaaaaaaa-2":
			return arc.Caller{ID: 2}, nil
		}
		return arc.Caller{}, arc.ErrCallerRejected
	}
	toks := []string{"token-aaaaaaaa-1", "token-aaaaaaaa-2", "token-aaaaaaaa-3"}
	got := make([]string, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i, tok := range toks {
		wg.Add(1)
		go func(i int, tok string) { defer wg.Done(); got[i], errs[i] = v.Verify(context.Background(), tok) }(i, tok)
	}
	waitFor(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.flights) == 3 })
	close(gate)
	wg.Wait()
	if got[0] != "arc-token:1" || got[1] != "arc-token:2" || errs[2] == nil {
		t.Errorf("identities %q, errors %v", got, errs)
	}
	if id, _ := v.Verify(context.Background(), toks[1]); id != "arc-token:2" {
		t.Errorf("cached identity %q", id)
	}
	if _, err := v.Verify(context.Background(), toks[2]); err == nil {
		t.Error("the rejected token was accepted from the cache")
	}
}

// Requests waiting on a verification Arc rejects are all rejected.
func TestFollowersOfARejectionAreRejected(t *testing.T) {
	gate := make(chan struct{})
	v := New(arc.NewClient("http://127.0.0.1:1", "", time.Second))
	var calls atomic.Int32
	v.verifyCaller = func(context.Context, string) (arc.Caller, error) {
		calls.Add(1)
		<-gate
		return arc.Caller{}, arc.ErrCallerRejected
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(context.Background(), "bad"); err == nil {
				accepted.Add(1)
			}
		}()
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if accepted.Load() != 0 || calls.Load() != 1 {
		t.Errorf("%d accepted, %d calls", accepted.Load(), calls.Load())
	}
}

// A panic while verifying releases the flight: the next request is not
// stuck behind it.
func TestPanicReleasesTheFlight(t *testing.T) {
	v := New(arc.NewClient("http://127.0.0.1:1", "", time.Second))
	v.verifyCaller = func(context.Context, string) (arc.Caller, error) { panic("boom") }
	if _, err := v.Verify(context.Background(), "tok"); !errors.Is(err, httpserve.ErrUnavailable) {
		t.Errorf("err %v", err)
	}
	v.verifyCaller = func(context.Context, string) (arc.Caller, error) { return arc.Caller{ID: 3}, nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if id, err := v.Verify(ctx, "tok"); id != "arc-token:3" || err != nil {
		t.Errorf("after the panic: %q %v", id, err)
	}
}

// A Forget that arrives while the token is being verified wins: the result
// is not cached.
func TestForgetDuringVerification(t *testing.T) {
	gate := make(chan struct{})
	v := New(arc.NewClient("http://127.0.0.1:1", "", time.Second))
	var calls atomic.Int32
	v.verifyCaller = func(context.Context, string) (arc.Caller, error) {
		if calls.Add(1) == 1 {
			<-gate
		}
		return arc.Caller{ID: 1}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = v.Verify(context.Background(), "tok") }()
	waitFor(t, func() bool { return calls.Load() == 1 })
	v.Forget("tok")
	close(gate)
	<-done
	_, _ = v.Verify(context.Background(), "tok")
	if calls.Load() != 2 {
		t.Errorf("%d verifications, want 2 (the first result must not be cached)", calls.Load())
	}
}

// The token sent to Arc is the one the endpoint verified, however the
// header spelled the scheme and spaces.
func TestHeaderSpellingReachesArcNormalized(t *testing.T) {
	fake := &fakeArc{tokens: map[string]int64{"good": 1}}
	_, srv := setup(t, fake)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"database":"db","sql":"SELECT token FROM t"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+httpserve.Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "bearer   good  ")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "seen:good") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}
