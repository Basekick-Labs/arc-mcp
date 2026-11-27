// Command arc-mcp is an MCP server that gives LLM clients read-only access to
// Arc, the open-source time-series lakehouse, over stdio or Streamable HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/basekick-labs/arc-mcp/internal/httpserve"
	"github.com/basekick-labs/arc-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is overridden at build time via:
//
//	-ldflags "-X main.Version=YY.MM.PATCH"
var Version = "dev"

const (
	defaultMaxRows         = 500
	defaultTimeout         = 30 * time.Second
	defaultMaxResponseSize = 50000
	defaultListen          = "127.0.0.1:8080"
	defaultMaxConcurrent   = 8
	// minResponseSize leaves room for a few rows next to the notes and the
	// footer; below it, every answer would be notes only.
	minResponseSize = 4000
)

func main() {
	arcURL := flag.String("arc-url", envOrDefault("ARC_URL", "http://localhost:8000"), "Arc instance URL")
	arcToken := flag.String("arc-token", envOrDefault("ARC_TOKEN", ""), "Arc API token (prefer ARC_TOKEN env var or ARC_TOKEN_FILE to avoid ps aux exposure)")
	maxRows := flag.Int("max-rows", envOrDefaultInt("ARC_MCP_MAX_ROWS", defaultMaxRows), "Maximum rows per query")
	timeout := flag.Duration("timeout", envDuration("ARC_MCP_TIMEOUT", defaultTimeout), "Query timeout")
	maxResponseSize := flag.Int("max-response-size", envOrDefaultInt("ARC_MCP_MAX_RESPONSE_SIZE", defaultMaxResponseSize), "Maximum response size in characters")
	insecure := flag.Bool("insecure", envBool("ARC_MCP_INSECURE"), "Allow token auth over plaintext HTTP to non-loopback hosts (credentials will be sent in cleartext)")
	transport := flag.String("transport", envOrDefault("ARC_MCP_TRANSPORT", "stdio"), "How MCP clients connect: stdio (the default) or http (Streamable HTTP at /mcp)")
	listen := flag.String("listen", envOrDefault("ARC_MCP_LISTEN", defaultListen), "HTTP transport: address to listen on")
	insecureListen := flag.Bool("insecure-listen", envBool("ARC_MCP_INSECURE_LISTEN"), "HTTP transport: allow a non-loopback address with no inbound token (anyone who can reach it can query Arc)")
	tlsCert := flag.String("tls-cert", envOrDefault("ARC_MCP_TLS_CERT_FILE", ""), "HTTP transport: TLS certificate file (with --tls-key) to serve HTTPS")
	tlsKey := flag.String("tls-key", envOrDefault("ARC_MCP_TLS_KEY_FILE", ""), "HTTP transport: TLS key file (with --tls-cert)")
	maxConcurrent := flag.Int("max-concurrent", envOrDefaultInt("ARC_MCP_MAX_CONCURRENT", defaultMaxConcurrent), "HTTP transport: requests served at once (0 for no limit)")
	allowAnyHost := flag.Bool("allow-any-host", envBool("ARC_MCP_ALLOW_ANY_HOST"), "HTTP transport: accept any Host header on a loopback listener (for a TLS proxy on the same host); turns off DNS-rebinding protection")
	toolGroups := flag.String("tools", envOrDefault("ARC_MCP_TOOLS", tools.DefaultGroups), "Tool groups to serve, comma-separated: data (the default), ops (Arc health and metrics)")
	version := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *version {
		fmt.Println(Version)
		os.Exit(0)
	}
	if badEnv != nil {
		fmt.Fprintf(os.Stderr, "Refusing to start: %v\n", badEnv)
		os.Exit(2)
	}
	switch *transport {
	case "stdio", "http":
	default:
		fmt.Fprintf(os.Stderr, "Refusing to start: unknown --transport/ARC_MCP_TRANSPORT %q; use stdio or http\n", *transport)
		os.Exit(2)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fmt.Fprintln(os.Stderr, "Refusing to start: --tls-cert and --tls-key go together")
		os.Exit(2)
	}
	if *maxConcurrent < 0 {
		fmt.Fprintf(os.Stderr, "Refusing to start: --max-concurrent %d is negative; use 0 for no limit\n", *maxConcurrent)
		os.Exit(2)
	}
	if *transport == "stdio" {
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "listen", "insecure-listen", "tls-cert", "tls-key", "max-concurrent", "allow-any-host":
				log.Printf("--%s applies to --transport http only; ignored over stdio", f.Name)
			}
		})
	}
	var httpToken string
	if *transport == "http" {
		t, err := resolveHTTPToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Refusing to start: %v\n", err)
			os.Exit(2)
		}
		httpToken = t
	}

	if *maxResponseSize < minResponseSize {
		log.Printf("max-response-size %d is below %d; using %d", *maxResponseSize, minResponseSize, minResponseSize)
		*maxResponseSize = minResponseSize
	}

	// Before anything reaches the network, so a typo is refused at once.
	groups, blank, err := tools.ParseGroups(*toolGroups)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Refusing to start: %v\n", err)
		os.Exit(2)
	}
	if blank {
		log.Printf("--tools/ARC_MCP_TOOLS names no tool group; using the default, %s", tools.DefaultGroups)
	}
	log.Printf("Tool groups: %s", groups)

	if c := tools.ClampRowLimit(*maxRows); c != *maxRows {
		log.Printf("max-rows %d is outside %d..%d; using %d", *maxRows, tools.MinRowLimit, tools.MaxRowLimit, c)
		*maxRows = c
	}

	// All logging goes to stderr to avoid corrupting the MCP stdio protocol
	log.SetOutput(os.Stderr)

	// Resolve token: ARC_TOKEN_FILE > ARC_TOKEN > --arc-token flag.
	token, err := resolveToken(*arcToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Refusing to start: %v\n", err)
		os.Exit(2)
	}
	*arcToken = token

	// Guard against sending Bearer tokens over cleartext to remote hosts.
	if err := validateArcURL(*arcURL, *arcToken, *insecure); err != nil {
		fmt.Fprintf(os.Stderr, "Refusing to start: %v\n", err)
		os.Exit(2)
	}
	if *insecure {
		log.Printf("WARNING: --insecure is set; Bearer token will be sent over plaintext HTTP")
	}

	// The HTTP listener is opened and checked (address guard, TLS files)
	// before arc-mcp contacts Arc with its token.
	var (
		ln       net.Listener
		httpOpts httpserve.Options
	)
	if *transport == "http" {
		httpOpts = httpserve.Options{
			Listen:         *listen,
			InsecureListen: *insecureListen,
			TLSCertFile:    *tlsCert,
			TLSKeyFile:     *tlsKey,
			AllowAnyHost:   *allowAnyHost,
			MaxConcurrent:  *maxConcurrent,
			QueryTimeout:   *timeout,
		}
		if httpToken != "" {
			httpOpts.Verify = httpserve.StaticToken(httpToken)
		}
		if os.Getenv("MCPGODEBUG") != "" {
			log.Print("WARNING: MCPGODEBUG is set: it can turn off the MCP SDK's DNS-rebinding and cross-origin protection")
		}
		var err error
		ln, err = httpserve.Listen(httpOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Refusing to start: %v\n", err)
			os.Exit(2)
		}
	}

	// Create Arc client
	client := arc.NewClient(*arcURL, *arcToken, *timeout)

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	healthy := true
	if err := client.Health(ctx); err != nil {
		healthy = false
		log.Printf("Warning: Arc health check failed: %v", err)
		log.Printf("The MCP server will start anyway — queries will fail until Arc is reachable at %s", arc.RedactURL(*arcURL))
	} else {
		log.Printf("Connected to Arc at %s", arc.RedactURL(*arcURL))
	}
	// Skipped when Arc is unreachable: a second failure would only repeat
	// the health warning above.
	if *arcToken != "" && healthy {
		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
		permissions, err := client.VerifyToken(verifyCtx)
		verifyCancel()
		if err != nil {
			log.Printf("Warning: Arc token verification failed: %v", err)
		} else {
			for _, line := range tokenScopeLog(permissions, *transport == "http") {
				log.Print(line)
			}
		}
	}

	// Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "arc-mcp",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions(*maxRows, groups),
	})

	// Register tools
	tools.RegisterAll(server, client, groups, *maxRows, *maxResponseSize)

	if ln != nil {
		scheme := "http"
		if *tlsCert != "" {
			scheme = "https"
		}
		auth := "no inbound token"
		if httpToken != "" {
			auth = "bearer token required"
		}
		log.Printf("arc-mcp serving %s://%s%s (%s; max-rows=%d, timeout=%s, max-concurrent=%d)", scheme, ln.Addr(), httpserve.Path, auth, *maxRows, *timeout, *maxConcurrent)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := httpserve.Serve(ctx, ln, httpserve.Handler(server, httpOpts), httpOpts); err != nil {
			fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Run on stdio
	log.Printf("arc-mcp server starting (max-rows=%d, timeout=%s)", *maxRows, *timeout)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

// tokenScopeLog returns the startup log lines describing the permissions Arc
// reports for the configured token: one line listing them, and one warning
// naming every write-class permission (write, delete, admin), since arc-mcp's
// tools only read. Advisory only: Arc enforces permissions, and a token's
// RBAC roles (which /api/v1/auth/verify does not report) can grant more.
func tokenScopeLog(permissions []string, remote bool) []string {
	if len(permissions) == 0 {
		return []string{"Arc token has no direct permissions; its access comes from RBAC roles, which this check cannot see"}
	}
	lines := []string{"Arc token permissions: " + strings.Join(permissions, ", ")}
	var overScoped []string
	for _, p := range permissions {
		switch p {
		case "write", "delete", "admin":
			overScoped = append(overScoped, p)
		}
	}
	if len(overScoped) > 0 {
		lines = append(lines, fmt.Sprintf("WARNING: Arc token carries %s; arc-mcp only reads, so give it a token with only the read permission", strings.Join(overScoped, ", ")))
		if remote {
			lines = append(lines, "WARNING: over HTTP, every client of this endpoint acts with this token's permissions")
		}
	}
	return lines
}

// validateArcURL rejects configurations that would send a Bearer token over
// cleartext HTTP to a non-loopback host. A query string or userinfo in the
// URL follows the same rule as a token: both are sent on every request and
// can carry credentials. Passing --insecure (or setting ARC_MCP_INSECURE)
// bypasses the check but logs a prominent warning. Errors name the URL
// without its query string and userinfo.
func validateArcURL(rawURL, token string, insecure bool) error {
	if rawURL == "" {
		return errors.New("arc URL is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("arc URL is not a valid URL: %w", arc.RedactError(err))
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("arc URL must use http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("arc URL is missing host")
	}
	inURL := u.RawQuery != "" || u.User != nil
	if u.Scheme == "https" || (token == "" && !inURL) || insecure {
		return nil
	}
	host := u.Hostname()
	if isLoopbackHost(host) {
		return nil
	}
	if token == "" {
		return fmt.Errorf("refusing to send the arc URL's query string or userinfo over plaintext HTTP to remote host %q — use https:// or pass --insecure", host)
	}
	return fmt.Errorf("refusing to send Bearer token over plaintext HTTP to remote host %q — use https:// or pass --insecure", host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// badEnv is the first boolean or duration environment variable whose value
// could not be read; main refuses to start on it rather than guess.
var badEnv error

// envBool reads a boolean environment variable with strconv.ParseBool: unset
// or empty is false, and a value that is not a boolean ("yes", a typo) is
// refused at startup. "0" and "false" mean false.
func envBool(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		if badEnv == nil {
			badEnv = fmt.Errorf("%s=%q is not a boolean (use true or false)", key, v)
		}
		return false
	}
	return b
}

// envDuration reads a duration environment variable ("30s", "2m"); unset is
// fallback, a value that does not parse is refused at startup.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		if badEnv == nil {
			badEnv = fmt.Errorf("%s=%q is not a duration (for example 30s or 2m)", key, v)
		}
		return fallback
	}
	return d
}

// resolveHTTPToken returns the inbound token for the HTTP transport:
// ARC_MCP_HTTP_TOKEN_FILE, then ARC_MCP_HTTP_TOKEN, "" when neither is set.
// There is no flag, so it never shows in ps. A file that is set but empty is
// refused: it would otherwise run the endpoint without authentication.
func resolveHTTPToken() (string, error) {
	if path := os.Getenv("ARC_MCP_HTTP_TOKEN_FILE"); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // #nosec G304 -- path comes from ARC_MCP_HTTP_TOKEN_FILE, set by the operator
		if err != nil {
			return "", fmt.Errorf("reading ARC_MCP_HTTP_TOKEN_FILE %q: %w", path, err)
		}
		t := strings.TrimSpace(string(data))
		if t == "" {
			return "", fmt.Errorf("ARC_MCP_HTTP_TOKEN_FILE %q is empty", path)
		}
		return t, nil
	}
	if v, set := os.LookupEnv("ARC_MCP_HTTP_TOKEN"); set && v != "" {
		t := strings.TrimSpace(v)
		if t == "" {
			return "", errors.New("ARC_MCP_HTTP_TOKEN is set but blank")
		}
		return t, nil
	}
	return "", nil
}

// envOrDefaultInt reads an integer environment variable; unset is fallback,
// a value that is not an integer ("abc", "4x") is refused at startup.
func envOrDefaultInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		if badEnv == nil {
			badEnv = fmt.Errorf("%s=%q is not an integer", key, v)
		}
		return fallback
	}
	return n
}

// resolveToken returns the Arc API token using the following precedence:
//
//  1. ARC_TOKEN_FILE env var — reads the file and trims trailing whitespace.
//     Preferred for production: the token never appears in ps aux or shell history.
//  2. ARC_TOKEN env var — convenient for containers and CI.
//  3. flagValue — the --arc-token CLI flag (least preferred; visible in ps aux).
func resolveToken(flagValue string) (string, error) {
	if path := os.Getenv("ARC_TOKEN_FILE"); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // path comes from ARC_TOKEN_FILE env var, not user input
		if err != nil {
			return "", fmt.Errorf("reading ARC_TOKEN_FILE %q: %w", path, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if v := os.Getenv("ARC_TOKEN"); v != "" {
		return v, nil
	}
	if flagValue != "" {
		log.Printf("WARNING: token supplied via --arc-token flag is visible in ps aux; prefer ARC_TOKEN env var or ARC_TOKEN_FILE")
	}
	return flagValue, nil
}

// serverInstructions is what every client receives on connect, for the tool
// groups served: the data text alone by default, byte for byte what it was
// before groups existed, and a paragraph on the operations tools when they
// are on. Per-tool detail lives in the tool descriptions.
func serverInstructions(maxRows int, groups tools.Groups) string {
	var parts []string
	if groups.Data {
		parts = append(parts, dataInstructions(maxRows))
	} else {
		parts = append(parts, opsOnlyLead)
	}
	if groups.Ops {
		parts = append(parts, opsInstructions)
	}
	return strings.Join(parts, "\n\n")
}

// opsOnlyLead replaces the data text when only the operations tools are on:
// there is no query to give habits for.
const opsOnlyLead = "Arc is a time-series lakehouse. This server has only its operations tools: they report on the Arc server itself, and cannot read the data stored in it."

// opsInstructions is the paragraph on the operations tools.
const opsInstructions = "Operations tools: arc_health says whether Arc is alive and ready for traffic; " +
	"arc_metrics returns Arc's own metrics (endpoint activity, the query pool, memory, every counter) and the last minutes of its system, application and API history. " +
	"They describe only the Arc node at the configured URL: in a cluster each node answers for itself, and behind a load balancer each call may reach a different node. " +
	"A not-ready answer (HTTP 503) is a state to report, not a failure."

// dataInstructions is the text for the data tools: what Arc is, and the
// habits that make queries on it cheap and right. The pruning rule is Arc's
// own (internal/pruning/time_bounds.go in the arc repo); keep it in step.
func dataInstructions(maxRows int) string {
	return "Arc is a time-series lakehouse: every reading kept at full resolution, years of history as Parquet, " +
		"queried with one SQL (DuckDB dialect) through these read-only tools.\n\n" +
		"How to query it well:\n" +
		"1. Discover first: list_databases, list_measurements, then describe_measurement for column names, types and the time range.\n" +
		"2. Bound by time. Arc reads only the partitions in range when a single-table SELECT has a top-level condition on time " +
		"against now() plus or minus one interval (seconds to months) or a literal without a UTC offset: " +
		"WHERE time > now() - INTERVAL '1 hour', or time BETWEEN '2026-11-01' AND '2026-11-02'. " +
		"A JOIN, CTE, UNION or subquery anywhere in the query, an OR, or a function of time reads all history.\n" +
		"3. Aggregate before returning rows: time_bucket(INTERVAL '5 minutes', time) or date_trunc('hour', time) with GROUP BY. " +
		"Results are capped at " + strconv.Itoa(maxRows) + " rows, and the answer says when more exist.\n" +
		"4. Start with a recent window and widen it deliberately.\n" +
		"5. Arc blocks some words anywhere in a query, even as column names (load, copy, set, ...): double-quote such a name, as in \"load\".\n" +
		"6. When a query fails, the error says why (a misspelled column and its candidates, a bad cast): fix the SQL rather than retrying it as is."
}
