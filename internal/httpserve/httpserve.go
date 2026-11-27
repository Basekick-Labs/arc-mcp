// Package httpserve serves an MCP server over Streamable HTTP: the MCP
// endpoint at /mcp, a liveness endpoint at /healthz, and the guards a
// network-facing endpoint needs (inbound bearer token, listen-address check,
// concurrency cap, body limit, access log, timeouts, graceful shutdown).
package httpserve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Path is where the MCP endpoint is served.
const Path = "/mcp"

// MaxBodyBytes caps a request body. arc-mcp's SQL reader refuses a query over
// 1 MiB anyway; the SDK reads the whole body into memory, so the cap is what
// bounds that.
const MaxBodyBytes = 2 << 20

// Options configures the HTTP endpoint.
type Options struct {
	// Listen is the host:port to listen on.
	Listen string
	// Verify authenticates the bearer token of every /mcp request; nil means
	// no inbound authentication. StaticToken builds the shared-secret one.
	Verify Verifier
	// InsecureListen allows a non-loopback listen address with no Verify.
	InsecureListen bool
	// TLSCertFile and TLSKeyFile, when both set, serve HTTPS.
	TLSCertFile, TLSKeyFile string
	// AllowAnyHost disables the SDK's DNS-rebinding protection (requests that
	// arrive on a loopback address with a non-localhost Host header), for a
	// TLS proxy on the same host that forwards the public Host header.
	AllowAnyHost bool
	// MaxConcurrent caps the /mcp requests served at once; 0 means no cap.
	MaxConcurrent int
	// QueryTimeout is arc-mcp's --timeout, which sizes the write timeout and
	// the shutdown grace period; 0 means queries have no deadline.
	QueryTimeout time.Duration
}

// Verifier authenticates a bearer token. It returns nil when the token is
// accepted, with an identity for the access log ("" when there is none);
// ErrUnavailable when it cannot tell right now (the client gets 503, not
// 401); any other error refuses the token.
type Verifier func(ctx context.Context, token string) (identity string, err error)

// ErrUnavailable is a Verifier's answer when it cannot check the token now.
var ErrUnavailable = errors.New("token verification unavailable")

// errInvalidToken is a Verifier's refusal.
var errInvalidToken = errors.New("invalid token")

// BearerToken is the token of an "Authorization: Bearer <token>" header:
// the scheme matched without regard to case, the token trimmed. A token
// with anything but visible ASCII is no token (Arc could not be sent it).
// requireBearer and the passthrough middleware both read it here, so the
// token forwarded is exactly the one verified.
func BearerToken(h http.Header) (string, bool) {
	scheme, token, ok := strings.Cut(h.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	for i := 0; i < len(token); i++ {
		if c := token[i]; c <= ' ' || c >= 0x7f {
			return "", false
		}
	}
	return token, true
}

// StaticToken is a Verifier for one shared secret. The comparison is on
// SHA-256 digests in constant time, so neither the content nor the length of
// the secret leaks through timing.
func StaticToken(secret string) Verifier {
	want := sha256.Sum256([]byte(secret))
	return func(_ context.Context, token string) (string, error) {
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return "", errInvalidToken
		}
		return "", nil
	}
}

// Listen opens the listener and checks it: a listener that is not on a
// loopback address needs an inbound token unless InsecureListen is set. The
// check is on the address actually bound, so a host name, "", 0.0.0.0, ::
// and IPv4-mapped forms are all judged the same way. With TLS files, the
// certificate is loaded here and the listener serves TLS, so a missing or
// bad file is refused before arc-mcp starts serving.
func Listen(opts Options) (net.Listener, error) {
	var tlsConfig *tls.Config
	if opts.TLSCertFile != "" || opts.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading the TLS certificate: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", opts.Listen)
	if err != nil {
		return nil, err
	}
	if err := checkListener(ln.Addr(), opts); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}
	return ln, nil
}

func checkListener(addr net.Addr, opts Options) error {
	tcp, ok := addr.(*net.TCPAddr)
	loopback := ok && tcp.IP.IsLoopback()
	switch {
	case loopback && opts.AllowAnyHost && opts.Verify == nil && !opts.InsecureListen:
		// Without the Host check and without a token, a web page that
		// rebinds its name to 127.0.0.1 is same-origin and gets through.
		return errors.New("refusing --allow-any-host without an inbound token: any web page open on this machine could reach the endpoint through DNS rebinding. " +
			"Set ARC_MCP_HTTP_TOKEN_FILE (or ARC_MCP_HTTP_TOKEN), or pass --insecure-listen")
	case loopback:
		return nil
	case opts.Verify == nil && !opts.InsecureListen:
		return fmt.Errorf("refusing to listen on %s without an inbound token: anyone who can reach it could query Arc with arc-mcp's token. "+
			"Set ARC_MCP_HTTP_TOKEN_FILE (or ARC_MCP_HTTP_TOKEN), listen on 127.0.0.1, or pass --insecure-listen", addr)
	case opts.Verify == nil:
		log.Printf("WARNING: --insecure-listen: %s accepts requests from anyone who can reach it, with no token", addr)
	case opts.TLSCertFile == "":
		log.Printf("WARNING: %s is not loopback and serves plain HTTP: the inbound token travels in cleartext unless TLS terminates in front of arc-mcp", addr)
	}
	return nil
}

// Handler is the HTTP handler: /mcp, /healthz, and a pointer to /mcp on the
// paths supergateway used.
//
// The MCP handler is stateless with JSON responses: arc-mcp keeps no
// per-session state, so any replica can answer any request, and every tool
// call is one request and one response. In that mode the server cannot send
// requests to the client, and notifications outside a request are dropped;
// no arc-mcp tool uses either. A tool that needs them must revisit this.
func Handler(server *mcp.Server, opts Options) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: opts.AllowAnyHost,
	})

	// Outermost first: authenticate, then read and check the body, then
	// wait for a slot, so neither an unauthenticated client nor a slow
	// upload holds one.
	var h http.Handler = mcpHandler
	if opts.MaxConcurrent > 0 {
		h = limitConcurrency(h, opts.MaxConcurrent)
	}
	h = rpcInfo(h)
	if opts.Verify != nil {
		h = requireBearer(h, opts.Verify)
	}

	mux := http.NewServeMux()
	mux.Handle(Path, h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Liveness only: it does not call Arc (arc_health does, in the ops
		// group), so a slow Arc does not get arc-mcp restarted.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	for _, old := range []string{"/sse", "/message"} {
		mux.HandleFunc(old, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "arc-mcp serves MCP over Streamable HTTP at "+Path+"; the HTTP+SSE endpoints of the old gateway image are gone.", http.StatusGone)
		})
	}
	return accessLog(mux)
}

// Serve runs the HTTP server on ln until ctx is done, then shuts it down,
// giving in-flight requests as long as a query may run.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, opts Options) error {
	// describe_measurement runs several queries under one --timeout budget;
	// leave room for that and for writing the answer.
	var writeTimeout time.Duration
	grace := 30 * time.Second
	if opts.QueryTimeout > 0 {
		writeTimeout = 4*opts.QueryTimeout + 30*time.Second
		grace = 4*opts.QueryTimeout + 5*time.Second
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.Default(),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutting down: waiting up to %s for requests in flight", grace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: requests still running after %s were cut off", grace)
		_ = srv.Close()
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// requireBearer refuses a request without an accepted bearer token: 401 with
// WWW-Authenticate, or 503 when the verifier cannot tell. Neither the header
// nor the verifier's error reaches the response or the log.
func requireBearer(next http.Handler, verify Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := BearerToken(r.Header)
		var identity string
		err := errInvalidToken
		if ok {
			identity, err = verify(r.Context(), token)
		}
		switch {
		case errors.Is(err, ErrUnavailable):
			w.Header().Set("Retry-After", "5")
			http.Error(w, "the token cannot be verified right now; try again shortly", http.StatusServiceUnavailable)
			return
		case err != nil:
			w.Header().Set("WWW-Authenticate", `Bearer realm="arc-mcp"`)
			http.Error(w, "unauthorized: a valid bearer token is required", http.StatusUnauthorized)
			return
		}
		if info, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
			info.caller = identity
		}
		next.ServeHTTP(w, r)
	})
}

// limitConcurrency serves at most n requests at once; the rest wait for a
// slot, or give up when the client does. Over stdio there was one client;
// over HTTP every holder of the token can fan out queries against Arc.
func limitConcurrency(next http.Handler, n int) http.Handler {
	slots := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		case <-r.Context().Done():
			http.Error(w, "server busy", http.StatusServiceUnavailable)
		}
	})
}

// reqInfoKey carries the JSON-RPC method and tool name from rpcInfo to the
// access log.
type reqInfoKey struct{}

type reqInfo struct {
	method, tool, caller string
}

// rpcInfo caps the body, refuses JSON-RPC batches, and notes the JSON-RPC
// method and tool name for the access log, then hands the body on
// unchanged. With an inbound token it runs after authentication, so an
// unauthenticated body is never read.
//
// Batches: the SDK still accepts them from clients of protocol versions
// before 2025-06-18 and runs their calls in parallel, so one request could
// run any number of Arc queries past the concurrency cap. MCP has dropped
// batching, and arc-mcp's stateless one-call-per-request mode has no use
// for it.
func rpcInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "request body unreadable", http.StatusBadRequest)
			}
			return
		}
		if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
			http.Error(w, "JSON-RPC batches are not supported: send one message per request", http.StatusBadRequest)
			return
		}
		if info, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if json.Unmarshal(body, &msg) == nil {
				info.method, info.tool = msg.Method, msg.Params.Name
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the status written.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog logs one line per request: method, path, status, duration, peer,
// and for MCP requests the JSON-RPC method and tool. With one shared token
// the peer is the only identity there is: Arc sees every query as arc-mcp's
// token. Never the Authorization header, never the arguments.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		info := &reqInfo{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info)))
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		// Everything the client controls is quoted and cut, so it cannot
		// forge a log line or flood the log.
		line := fmt.Sprintf("http %s %s %d %s peer=%s", logField(r.Method), logField(r.URL.Path), sw.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
		if info.method != "" {
			line += " rpc=" + logField(info.method)
		}
		if info.tool != "" {
			line += " tool=" + logField(info.tool)
		}
		if info.caller != "" {
			line += " caller=" + logField(info.caller)
		}
		log.Print(line) //nolint:gosec // #nosec G706 -- client values pass through logField
	})
}

// logField is a client-supplied value as it goes in the access log: cut to
// 128 bytes, and quoted when it holds anything but plain printable ASCII
// (a newline would otherwise start a forged line).
func logField(s string) string {
	if len(s) > 128 {
		s = s[:128] + "..."
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '"' {
			return strconv.Quote(s)
		}
	}
	return s
}
