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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
// accepted. #1 (forwarding the caller's token to Arc) can plug in here.
type Verifier func(ctx context.Context, token string) error

// errInvalidToken is a Verifier's refusal.
var errInvalidToken = errors.New("invalid token")

// StaticToken is a Verifier for one shared secret. The comparison is on
// SHA-256 digests in constant time, so neither the content nor the length of
// the secret leaks through timing.
func StaticToken(secret string) Verifier {
	want := sha256.Sum256([]byte(secret))
	return func(_ context.Context, token string) error {
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return errInvalidToken
		}
		return nil
	}
}

// Listen opens the listener and checks it: a listener that is not on a
// loopback address needs an inbound token unless InsecureListen is set. The
// check is on the address actually bound, so a host name, "", 0.0.0.0, ::
// and IPv4-mapped forms are all judged the same way.
func Listen(opts Options) (net.Listener, error) {
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, err
	}
	if err := checkListener(ln.Addr(), opts); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func checkListener(addr net.Addr, opts Options) error {
	tcp, ok := addr.(*net.TCPAddr)
	loopback := ok && tcp.IP.IsLoopback()
	switch {
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

	var h http.Handler = mcpHandler
	h = rpcInfo(h)
	if opts.MaxConcurrent > 0 {
		h = limitConcurrency(h, opts.MaxConcurrent)
	}
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
	go func() {
		if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
			errc <- srv.ServeTLS(ln, opts.TLSCertFile, opts.TLSKeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutting down: waiting up to %s for requests in flight", grace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// requireBearer refuses a request without an accepted bearer token: 401 with
// WWW-Authenticate. Neither the header nor the verifier's error reaches the
// response or the log.
func requireBearer(next http.Handler, verify Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || verify(r.Context(), strings.TrimSpace(token)) != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="arc-mcp"`)
			http.Error(w, "unauthorized: a valid bearer token is required", http.StatusUnauthorized)
			return
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
	method, tool string
}

// rpcInfo caps the body and notes the JSON-RPC method and tool name for the
// access log, then hands the body on unchanged. It runs after authentication,
// so an unauthenticated body is never read.
func rpcInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
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
		line := fmt.Sprintf("http %s %s %d %s peer=%s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
		if info.method != "" {
			line += " rpc=" + info.method
		}
		if info.tool != "" {
			line += " tool=" + info.tool
		}
		log.Print(line)
	})
}
