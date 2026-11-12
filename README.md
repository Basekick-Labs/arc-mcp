# arc-mcp

MCP (Model Context Protocol) server for [Arc](https://github.com/basekick-labs/arc) — a high-performance columnar analytical database.

Connect any MCP-compatible LLM (Claude, GPT, etc.) to your Arc instance to discover schemas and query data using natural language.

## Installation

```bash
go install github.com/basekick-labs/arc-mcp/cmd/arc-mcp@latest
```

Or build from source:

```bash
git clone https://github.com/basekick-labs/arc-mcp.git
cd arc-mcp
go build -o arc-mcp ./cmd/arc-mcp
```

## Usage

```bash
arc-mcp \
  --arc-url http://localhost:8000 \
  --arc-token "your-api-token" \
  --max-rows 500 \
  --timeout 30s
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `ARC_URL` | Arc instance URL | `http://localhost:8000` |
| `ARC_TOKEN` | Arc API token | (none) |
| `ARC_TOKEN_FILE` | File containing the Arc API token | (none) |
| `ARC_MCP_MAX_ROWS` | Maximum rows per query | `500` |

### CLI Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--arc-url` | Arc instance URL | `$ARC_URL` or `http://localhost:8000` |
| `--arc-token` | Arc API token | `$ARC_TOKEN` |
| `--max-rows` | Maximum rows per query | `500` |
| `--timeout` | Query timeout | `30s` |
| `--max-response-size` | Max response characters | `50000` |

### Token hygiene

Use a dedicated, short-lived Arc token with only the `read` permission for
arc-mcp. Do not share an administrator or write-capable token with an MCP
client. Prefer `ARC_TOKEN_FILE` (or a secret-mounted `ARC_TOKEN`) so the token
does not appear in shell history or process arguments.

Create one with an admin token (`expires_in` is optional):

```bash
curl -s -X POST "$ARC_URL/api/v1/auth/tokens" \
  -H "Authorization: Bearer $ARC_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"arc-mcp","permissions":["read"],"expires_in":"30d"}'
```

The response holds the new token once; store it in the file `ARC_TOKEN_FILE`
points to. Leaving `permissions` out gives Arc's default, `read,write`.

At startup, arc-mcp asks Arc's `/api/v1/auth/verify` endpoint for the token's
permissions and logs them, with one warning naming any of `write`, `delete` or
`admin`. The check is skipped when Arc is unreachable. It sees only the token's
own permissions, not RBAC roles, and it does not stop arc-mcp from starting:
Arc remains responsible for enforcing permissions.

## Docker Compose (quick-start)

The fastest way to evaluate arc-mcp is with Docker Compose — it brings up Arc and arc-mcp together, exposing MCP over HTTP/SSE for remote LLM clients:

```bash
cp .env.example .env
# edit .env — at minimum set ARC_TOKEN
docker compose --profile with-arc up
```

- Arc API:             `http://localhost:8000`
- MCP over SSE:        `http://localhost:8080/sse`

If you already run Arc elsewhere, omit the `--profile with-arc` flag, set `ARC_URL` and `ARC_TOKEN` in `.env`, and run:

```bash
docker compose up arc-mcp
```

See [`docker-compose.yml`](docker-compose.yml) and [`.env.example`](.env.example) for the full surface.

## Connecting to LLM Clients

arc-mcp uses the **stdio** transport (JSON-RPC over stdin/stdout), which is supported by all major MCP-compatible clients.

### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "arc": {
      "command": "arc-mcp",
      "args": ["--arc-url", "http://localhost:8000", "--arc-token", "your-token"]
    }
  }
}
```

### Claude Code (CLI)

Add to your project's `.claude/settings.json` or global `~/.claude/settings.json`:

```json
{
  "mcpServers": {
    "arc": {
      "command": "arc-mcp",
      "args": ["--arc-url", "http://localhost:8000", "--arc-token", "your-token"]
    }
  }
}
```

### Cursor

Add to `.cursor/mcp.json` in your project root:

```json
{
  "mcpServers": {
    "arc": {
      "command": "arc-mcp",
      "args": ["--arc-url", "http://localhost:8000", "--arc-token", "your-token"]
    }
  }
}
```

### Windsurf

Add to `~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "arc": {
      "command": "arc-mcp",
      "args": ["--arc-url", "http://localhost:8000", "--arc-token", "your-token"]
    }
  }
}
```

### ChatGPT Desktop

Add to ChatGPT's MCP settings (Settings > MCP Servers > Add Server):

```json
{
  "mcpServers": {
    "arc": {
      "command": "arc-mcp",
      "args": ["--arc-url", "http://localhost:8000", "--arc-token", "your-token"]
    }
  }
}
```

### Any MCP-compatible client

arc-mcp speaks standard MCP over stdio. To connect from any client that supports the protocol, run:

```bash
arc-mcp --arc-url http://your-arc-host:8000 --arc-token your-token
```

The server reads JSON-RPC from stdin and writes to stdout. All logs go to stderr.

## Available Tools

### list_databases

List all databases in the Arc instance.

**Example prompt:** "What databases are available?"

### list_measurements

List all measurements (tables) in a database.

**Example prompt:** "What tables are in the telemetry database?"

### describe_measurement

Get column names, types, row count, and time range for a measurement.

**Example prompt:** "Describe the schema of the cpu measurement in the telemetry database"

### query

Execute a read-only SQL query (DuckDB dialect). Supports SELECT, aggregations, JOINs, CTEs, `time_bucket()`, `date_trunc()`, and more.

**Example prompts:**
- "What's the average CPU usage per host in the last hour?"
- "Show me the top 10 hosts by memory usage"
- "How many rows are in the sensors measurement?"

### get_sample_data

Get recent sample rows from a measurement, ordered by time descending.

**Example prompt:** "Show me some recent data from the temperature measurement"

## Safety

- **Read-only:** All write operations (INSERT, UPDATE, DELETE, DROP, ALTER, CREATE, ATTACH, etc.) are blocked before reaching Arc
- **Row limits:** Queries are capped at 500 rows by default (configurable)
- **Response truncation:** Large responses are truncated to prevent overwhelming LLM context windows
- **Timeout:** 30-second default query timeout

## Architecture

```
arc-mcp (MCP Server)  ──HTTP──>  Arc (Analytical DB)
     │                              │
  stdio/JSON-RPC               REST API
     │                              │
  LLM Client                   Parquet Storage
(Claude, GPT, etc.)
```

The MCP server is a thin client that communicates with Arc over its existing REST API. It does not import Arc internals or share code with Arc.

## Development

```bash
# Run tests
go test ./...

# Build
go build ./cmd/arc-mcp

# Run locally
./arc-mcp --arc-url http://localhost:8000 --arc-token your-token
```

## License

MIT
