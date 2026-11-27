package httpserve

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/basekick-labs/arc-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const secret = "s3cret-endpoint-token"

// fakeArc answers every query with one row holding a large integer.
func fakeArc(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"columns":["n"],"data":[[9007199254740993]],"row_count":1,"execution_time_ms":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mcpServer is arc-mcp's server with the data tools, against fake Arc.
func mcpServer(t *testing.T) *mcp.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "arc-mcp", Version: "test"}, nil)
	tools.RegisterAll(server, arc.NewClient(fakeArc(t).URL, "arc-token", 5*time.Second), tools.Groups{Data: true}, 500, 50_000)
	return server
}

// endpoint serves Handler on a loopback httptest server.
func endpoint(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(mcpServer(t), opts))
	t.Cleanup(srv.Close)
	return srv
}

// bearer adds an Authorization header to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// connect opens an MCP session to srv over Streamable HTTP.
func connect(t *testing.T, srv *httptest.Server, token string) (*mcp.ClientSession, error) {
	t.Helper()
	hc := srv.Client()
	if token != "" {
		hc = &http.Client{Transport: bearer{token, srv.Client().Transport}}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: srv.URL + Path, HTTPClient: hc, MaxRetries: -1}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

// A full session over HTTP: the same tools as stdio, and a query answered
// with its number exact.
func TestHTTPSession(t *testing.T) {
	srv := endpoint(t, Options{Verify: StaticToken(secret)})
	cs, err := connect(t, srv, secret)
	if err != nil {
		t.Fatal(err)
	}
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 5 {
		t.Errorf("%d tools over HTTP, want the 5 data tools", len(list.Tools))
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "query", Arguments: map[string]any{"database": "db", "sql": "SELECT n FROM t"}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError || !strings.Contains(text, "9007199254740993") {
		t.Errorf("isError=%v text=%q", res.IsError, text)
	}
}

func TestBearerRequired(t *testing.T) {
	srv := endpoint(t, Options{Verify: StaticToken(secret)})
	for name, token := range map[string]string{"none": "", "wrong": "nope", "prefix": secret[:5], "longer": secret + "x"} {
		if _, err := connect(t, srv, token); err == nil {
			t.Errorf("%s token: connected", name)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+Path, strings.NewReader(`{}`))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("status %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

func TestStaticToken(t *testing.T) {
	v := StaticToken(secret)
	if _, err := v(context.Background(), secret); err != nil {
		t.Error("the right token was refused")
	}
	// The header's extra spaces are not part of the token.
	srv := endpoint(t, Options{Verify: StaticToken(secret)})
	if _, err := connect(t, srv, " "+secret); err != nil {
		t.Errorf("Bearer with two spaces refused: %v", err)
	}
	for _, bad := range []string{"", "x", secret + " ", strings.ToUpper(secret)} {
		if _, err := v(context.Background(), bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHealthzAndOldPaths(t *testing.T) {
	srv := endpoint(t, Options{Verify: StaticToken(secret)})
	for path, want := range map[string]int{"/healthz": 200, "/sse": http.StatusGone, "/message": http.StatusGone} {
		resp, err := srv.Client().Do(get(t, srv.URL+path))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
		if want == http.StatusGone && !strings.Contains(string(body), Path) {
			t.Errorf("%s does not point at %s: %q", path, Path, body)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	srv := endpoint(t, Options{Verify: StaticToken(secret)})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+Path, bytes.NewReader(make([]byte, MaxBodyBytes+1)))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", resp.StatusCode)
	}
}

// The SDK's DNS-rebinding protection stays on: a request arriving on
// loopback with a foreign Host header is refused, unless AllowAnyHost.
func TestDNSRebindingProtection(t *testing.T) {
	for _, allow := range []bool{false, true} {
		srv := endpoint(t, Options{AllowAnyHost: allow, Verify: StaticToken(secret)})
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+Path, strings.NewReader(body))
		req.Host = "evil.example.com"
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		want := http.StatusForbidden
		if allow {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Errorf("AllowAnyHost=%v: status %d, want %d", allow, resp.StatusCode, want)
		}
	}
}

func TestListenGuard(t *testing.T) {
	tcp := func(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: 8080} }
	tests := []struct {
		addr    net.Addr
		opts    Options
		wantErr bool
	}{
		{tcp("127.0.0.1"), Options{}, false},
		{tcp("127.1.2.3"), Options{}, false},
		{tcp("::1"), Options{}, false},
		{tcp("0.0.0.0"), Options{}, true},
		{tcp("::"), Options{}, true},
		{tcp("192.168.1.10"), Options{}, true},
		{tcp("192.168.1.10"), Options{Verify: StaticToken(secret)}, false},
		{tcp("192.168.1.10"), Options{InsecureListen: true}, false},
		// Without the Host check, only a token stops DNS rebinding.
		{tcp("127.0.0.1"), Options{AllowAnyHost: true}, true},
		{tcp("127.0.0.1"), Options{AllowAnyHost: true, Verify: StaticToken(secret)}, false},
		{tcp("127.0.0.1"), Options{AllowAnyHost: true, InsecureListen: true}, false},
	}
	for _, tt := range tests {
		if err := checkListener(tt.addr, tt.opts); (err != nil) != tt.wantErr {
			t.Errorf("%v %+v: err %v, wantErr %v", tt.addr, tt.opts.InsecureListen, err, tt.wantErr)
		}
	}
	// Judged on the socket actually bound: localhost is loopback, an empty
	// host is every interface.
	ln, err := Listen(Options{Listen: "localhost:0"})
	if err != nil {
		t.Fatalf("localhost:0 refused: %v", err)
	}
	_ = ln.Close()
	if ln, err := Listen(Options{Listen: ":0"}); err == nil {
		_ = ln.Close()
		t.Error(":0 (every interface) accepted with no token")
	}
}

// At most n requests are served at once.
func TestLimitConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	h := limitConcurrency(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
	}), 2)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, Path, nil))
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if peak.Load() != 2 {
		t.Errorf("peak concurrency %d, want 2", peak.Load())
	}
}

// One line per request with the JSON-RPC method and tool, never the token.
func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	srv := endpoint(t, Options{Verify: StaticToken(secret)})
	cs, err := connect(t, srv, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "query", Arguments: map[string]any{"database": "db", "sql": "SELECT n FROM t"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "rpc=tools/call tool=query") || strings.Contains(out, secret) {
		t.Errorf("access log:\n%s", out)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Serve stops when its context is done, after the request in flight.
func TestServeShutsDown(t *testing.T) {
	arcSrv, gate := slowArc(t)
	ln, err := Listen(Options{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, Handler(mcpServerFor(t, arcSrv.URL), Options{}), Options{QueryTimeout: 5 * time.Second})
	}()
	callDone := make(chan string, 1)
	go func() {
		resp, err := http.DefaultClient.Do(callRequest(t, "http://"+ln.Addr().String()+Path, ""))
		if err != nil {
			callDone <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		callDone <- string(b)
	}()
	<-gate.started
	cancel()
	time.Sleep(100 * time.Millisecond)
	close(gate.release)
	if out := <-callDone; !strings.Contains(out, "9007199254740993") {
		t.Errorf("the call in flight did not finish: %q", out)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if resp, err := http.DefaultClient.Do(get(t, "http://"+ln.Addr().String()+"/healthz")); err == nil {
		_ = resp.Body.Close()
		t.Error("still serving after shutdown")
	}
}

// gate holds the slow fake Arc's queries until release is closed.
type gate struct {
	started        chan struct{}
	release        chan struct{}
	inFlight, peak atomic.Int32
}

// slowArc is a fake Arc whose queries wait for the gate, counting how many
// run at once.
func slowArc(t *testing.T) (*httptest.Server, *gate) {
	t.Helper()
	g := &gate{started: make(chan struct{}, 100), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := g.inFlight.Add(1)
		defer g.inFlight.Add(-1)
		for p := g.peak.Load(); n > p; p = g.peak.Load() {
			if g.peak.CompareAndSwap(p, n) {
				break
			}
		}
		g.started <- struct{}{}
		<-g.release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"columns":["n"],"data":[[9007199254740993]],"row_count":1,"execution_time_ms":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv, g
}

func mcpServerFor(t *testing.T, arcURL string) *mcp.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "arc-mcp", Version: "test"}, nil)
	tools.RegisterAll(server, arc.NewClient(arcURL, "arc-token", 5*time.Second), tools.Groups{Data: true}, 500, 50_000)
	return server
}

const callBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"database":"db","sql":"SELECT n FROM t"}}}`

// callRequest is a raw tools/call POST, as a client of the 2025-03-26
// protocol sends it (no MCP-Protocol-Version header).
func callRequest(t *testing.T, url, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(callBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// The cap holds on tool calls through the real handler: two calls with a
// cap of one never run two Arc queries at once.
func TestHandlerConcurrencyCap(t *testing.T) {
	arcSrv, g := slowArc(t)
	srv := httptest.NewServer(Handler(mcpServerFor(t, arcSrv.URL), Options{MaxConcurrent: 1}))
	t.Cleanup(srv.Close)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := srv.Client().Do(callRequest(t, srv.URL+Path, "")); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	<-g.started
	time.Sleep(200 * time.Millisecond)
	close(g.release)
	wg.Wait()
	if g.peak.Load() != 1 {
		t.Errorf("peak Arc queries %d with a cap of 1", g.peak.Load())
	}
}

// A JSON-RPC batch would run its calls in parallel past the cap: refused.
func TestBatchRefused(t *testing.T) {
	srv := endpoint(t, Options{MaxConcurrent: 1})
	for _, body := range []string{"[" + callBody + "," + callBody + "]", " \n[" + callBody + "]"} {
		req := callRequest(t, srv.URL+Path, "")
		req.Body = io.NopCloser(strings.NewReader(body))
		req.ContentLength = int64(len(body))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("batch: status %d, want 400", resp.StatusCode)
		}
	}
}

// Without the token, every method gets 401, and the body is not read: an
// oversized one gets 401, not 413.
func TestUnauthenticatedNeverReachesMCP(t *testing.T) {
	srv := endpoint(t, Options{Verify: StaticToken(secret)})
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPost, http.MethodPut} {
		req, _ := http.NewRequestWithContext(t.Context(), m, srv.URL+Path, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", m, resp.StatusCode)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+Path, bytes.NewReader(make([]byte, MaxBodyBytes+1)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("oversized unauthenticated body: status %d, want 401", resp.StatusCode)
	}
}

func TestLogField(t *testing.T) {
	for in, want := range map[string]string{
		"tools/call":             "tools/call",
		"/mcp":                   "/mcp",
		"q\nhttp POST /mcp 200":  `"q\nhttp POST /mcp 200"`,
		"a b":                    `"a b"`,
		strings.Repeat("x", 200): strings.Repeat("x", 128) + "...",
	} {
		if got := logField(in); got != want {
			t.Errorf("logField(%q) = %q, want %q", in, got, want)
		}
	}
}

// TLS files are loaded by Listen: a missing one is refused before serving,
// and a good pair serves HTTPS.
func TestListenTLS(t *testing.T) {
	if _, err := Listen(Options{Listen: "127.0.0.1:0", TLSCertFile: "/nonexistent.crt", TLSKeyFile: "/nonexistent.key"}); err == nil {
		t.Fatal("missing TLS files accepted")
	}
	certFile, keyFile := selfSigned(t)
	ln, err := Listen(Options{Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, Handler(mcpServer(t), Options{}), Options{}) }()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test certificate
	resp, err := client.Do(get(t, "https://"+ln.Addr().String()+"/healthz"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.TLS == nil || resp.StatusCode != 200 {
		t.Errorf("TLS %v, status %d", resp.TLS != nil, resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func selfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func get(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
