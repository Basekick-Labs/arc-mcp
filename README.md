# arc-mcp

MCP (Model Context Protocol) server for [Arc](https://github.com/basekick-labs/arc), the open-source time-series lakehouse: every telemetry reading kept at full resolution, recent data hot and years of history as Parquet in your own bucket, queried with one SQL.

Connect any MCP-compatible assistant (Claude, Cursor, and others) to your Arc instance and ask questions of your whole history in natural language. arc-mcp only reads: it discovers databases and measurements, describes their schema, and runs read-only SQL with what an agent needs to use it well: guidance to bound queries by time (what Arc prunes partitions by), a row cap that says when more rows exist, and Arc's own errors passed back so the model can fix its SQL.

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
| `--max-response-size` | Size limit of an answer, in characters (minimum 4000). Applies to the text and to the structured rows alike, each stopping at a row boundary | `50000` |

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

### Structured output

Every tool declares an MCP `outputSchema` and returns `structuredContent` on success, next to its markdown text: `query` and `get_sample_data` return `columns`, `rows` (numbers exactly as Arc sent them), `row_count`, `truncated` with the reasons (`row_limit`, `arc_row_cap`, `size_budget`, `stream_incomplete`) and `no_data`; `describe_measurement` returns columns with types and typed statistics; the listing tools return their lists. Clients that only read text are unaffected.

### list_databases

List all databases in the Arc instance. Needs a token with read permission.

**Example prompt:** "What databases are available?"

A token Arc restricts to particular databases cannot list them all: Arc refuses
the request rather than returning a partial list, the same way `SHOW DATABASES`
does. The tool reports that as a result rather than an error, saying the token
is scoped and that a database has to be named — so the model asks you which one
instead of retrying. Call `list_measurements` with that database.

### list_measurements

List all measurements (tables) in a database. Needs a token with read
permission for that database.

**Example prompt:** "What tables are in the telemetry database?"

A token that can read some databases but not the one asked for gets the same
scoped result, naming the database it was refused for.

### describe_measurement

Get column names, types, row count, and time range for a measurement. The
statistics are exact; together they get one `--timeout`, and one that runs out
of time or that Arc refuses (a rate limit or quota) is shown as not computed,
with the reason.

**Example prompt:** "Describe the schema of the cpu measurement in the telemetry database"

### query

Execute a read-only SQL query (DuckDB dialect). Supports SELECT, aggregations, JOINs, CTEs, `time_bucket()`, `date_trunc()`, and more. Bound single-table queries by time with a top-level condition (`WHERE time > now() - INTERVAL '1 hour'`): Arc then reads only the partitions in range. A JOIN, CTE or subquery reads all history.

**Example prompts:**
- "What's the average CPU usage per host in the last hour?"
- "Show me the top 10 hosts by memory usage"
- "How many rows are in the sensors measurement?"

### get_sample_data

Get recent sample rows from a measurement, ordered by time descending.

**Example prompt:** "Show me some recent data from the temperature measurement"

## Safety

- **Read-only:** Only one read statement runs per call: SELECT, FROM, VALUES, WITH, PIVOT, UNPIVOT, SHOW, DESCRIBE, SUMMARIZE or EXPLAIN. arc-mcp applies a subset of Arc's own query checks so that a refusal comes back with a reason the model can act on: Arc refuses words such as `load`, `copy` and `set` anywhere in a query, so a column with one of those names has to be double-quoted (`AS "load"`). When Arc refuses something arc-mcp did not check, Arc's validation message is passed through. Arc's checks and a read-only token remain the real boundary
- **Permission answers are kept apart:** Arc's 401 (token missing, invalid or expired), its 403 for a token without read permission, its 403 for a token scoped to other databases, and its 404 for a database that does not exist produce four different messages. arc-mcp never retries a refusal and never widens a token's reach to work around one
- **Row limits:** Queries return at most 500 rows by default (`--max-rows` / `ARC_MCP_MAX_ROWS`, 1 to 100,000). arc-mcp asks Arc for one row more than it shows: it appends `LIMIT 501` when the query has no LIMIT of its own and lowers a larger one, leaving the WHERE clause untouched so Arc still prunes partitions by time. When more rows exist, the answer says so at the top instead of looking complete
- **Size limit:** an answer stops at the last row that fits `--max-response-size`, in the text and in the structured output alike, and says how many rows it shows
- **Timeout:** 30-second default query timeout; a query that runs out of time tells the model to aggregate or narrow the time range
- **Read-only, declared:** every tool carries the MCP read-only annotation, so clients can run it without asking each time
- **Errors the model can act on:** when DuckDB rejects a query (a misspelled column and its candidates, a bad cast, a missing GROUP BY), its message is passed back with Arc's rewritten SQL and every storage path removed; storage and server faults stay generic

## Architecture

```
arc-mcp (MCP Server)  ──HTTP──>  Arc (time-series lakehouse)
     │                              │
  stdio/JSON-RPC               REST API
     │                              │
  LLM Client                   Parquet Storage
(Claude, Cursor, etc.)
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
