// Command arc-mcp is an MCP server that gives LLM clients read-only access to
// Arc, the open-source time-series lakehouse, over stdio.
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
	"strconv"
	"strings"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
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
)

func main() {
	arcURL := flag.String("arc-url", envOrDefault("ARC_URL", "http://localhost:8000"), "Arc instance URL")
	arcToken := flag.String("arc-token", envOrDefault("ARC_TOKEN", ""), "Arc API token (prefer ARC_TOKEN env var or ARC_TOKEN_FILE to avoid ps aux exposure)")
	maxRows := flag.Int("max-rows", envOrDefaultInt("ARC_MCP_MAX_ROWS", defaultMaxRows), "Maximum rows per query")
	timeout := flag.Duration("timeout", defaultTimeout, "Query timeout")
	maxResponseSize := flag.Int("max-response-size", defaultMaxResponseSize, "Maximum response size in characters")
	insecure := flag.Bool("insecure", envOrDefault("ARC_MCP_INSECURE", "") != "", "Allow token auth over plaintext HTTP to non-loopback hosts (credentials will be sent in cleartext)")
	version := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *version {
		fmt.Println(Version)
		os.Exit(0)
	}

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

	// Create Arc client
	client := arc.NewClient(*arcURL, *arcToken, *timeout)

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	healthy := true
	if err := client.Health(ctx); err != nil {
		healthy = false
		log.Printf("Warning: Arc health check failed: %v", err)
		log.Printf("The MCP server will start anyway — queries will fail until Arc is reachable at %s", *arcURL)
	} else {
		log.Printf("Connected to Arc at %s", *arcURL)
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
			for _, line := range tokenScopeLog(permissions) {
				log.Print(line)
			}
		}
	}

	// Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "arc-mcp",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions(*maxRows),
	})

	// Register tools
	tools.RegisterAll(server, client, *maxRows, *maxResponseSize)

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
func tokenScopeLog(permissions []string) []string {
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
	}
	return lines
}

// validateArcURL rejects configurations that would send a Bearer token over
// cleartext HTTP to a non-loopback host. Passing --insecure (or setting
// ARC_MCP_INSECURE) bypasses the check but logs a prominent warning.
func validateArcURL(rawURL, token string, insecure bool) error {
	if rawURL == "" {
		return errors.New("arc URL is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("arc URL is not a valid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("arc URL must use http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("arc URL is missing host")
	}
	if u.Scheme == "https" || token == "" || insecure {
		return nil
	}
	host := u.Hostname()
	if isLoopbackHost(host) {
		return nil
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

func envOrDefaultInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
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

// serverInstructions is what every client receives on connect: what Arc is,
// and the habits that make queries on it cheap and right. Per-tool detail
// lives in the tool descriptions. The pruning rule is Arc's own
// (internal/pruning/time_bounds.go in the arc repo); keep it in step.
func serverInstructions(maxRows int) string {
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
