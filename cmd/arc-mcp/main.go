// Command arc-mcp is an MCP server that exposes Arc, a columnar analytical
// database, to LLM clients over stdio.
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
	if err := client.Health(ctx); err != nil {
		log.Printf("Warning: Arc health check failed: %v", err)
		log.Printf("The MCP server will start anyway — queries will fail until Arc is reachable at %s", *arcURL)
	} else {
		log.Printf("Connected to Arc at %s", *arcURL)
	}

	// Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "arc-mcp",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: "Arc is a high-performance columnar analytical database. Use list_databases to discover databases, list_measurements to see tables, describe_measurement to understand schema, and query to run SQL (DuckDB dialect). Always describe a measurement before querying it.",
	})

	// Register tools
	tools.RegisterListDatabases(server, client)
	tools.RegisterListMeasurements(server, client)
	tools.RegisterDescribeMeasurement(server, client, *maxResponseSize)
	tools.RegisterQuery(server, client, *maxRows, *maxResponseSize)
	tools.RegisterGetSampleData(server, client, *maxResponseSize)

	// Run on stdio
	log.Printf("arc-mcp server starting (max-rows=%d, timeout=%s)", *maxRows, *timeout)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
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
