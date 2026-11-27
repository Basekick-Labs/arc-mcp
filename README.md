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
ARC_URL=http://localhost:8000 ARC_TOKEN_FILE=/path/to/token \
  arc-mcp --max-rows 500 --timeout 30s
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `ARC_URL` | Arc instance URL | `http://localhost:8000` |
| `ARC_TOKEN` | Arc API token | (none) |
| `ARC_TOKEN_FILE` | File containing the Arc API token (an empty file is refused) | (none) |
| `ARC_MCP_MAX_ROWS` | Maximum rows per query | `500` |
| `ARC_MCP_TOOLS` | Tool groups to serve: `data`, `ops` or `data,ops` (see [Operations tools](#operations-tools-opt-in)) | `data` |
| `ARC_MCP_TIMEOUT` | Query timeout (`30s`, `2m`) | `30s` |
| `ARC_MCP_MAX_RESPONSE_SIZE` | Size limit of an answer, in characters | `50000` |
| `ARC_MCP_INSECURE` | `true` to allow a token over plaintext HTTP to a non-loopback Arc | `false` |
| `ARC_MCP_TRANSPORT` | `stdio` or `http` (see [Remote access over HTTP](#remote-access-over-http)) | `stdio` |
| `ARC_MCP_LISTEN` | HTTP: address to listen on | `127.0.0.1:8080` |
| `ARC_MCP_HTTP_TOKEN_FILE` | HTTP: file containing the bearer token clients must send | (none) |
| `ARC_MCP_HTTP_TOKEN` | HTTP: the bearer token clients must send | (none) |
| `ARC_MCP_INSECURE_LISTEN` | HTTP: `true` to serve a non-loopback address with no token | `false` |
| `ARC_MCP_MAX_CONCURRENT` | HTTP: requests served at once (`0` for no limit) | `8` |
| `ARC_MCP_TLS_CERT_FILE`, `ARC_MCP_TLS_KEY_FILE` | HTTP: serve HTTPS with this certificate and key | (none) |
| `ARC_MCP_ALLOW_ANY_HOST` | HTTP: accept any `Host` header on a loopback listener (see below) | `false` |
| `ARC_MCP_AUTH_MODE` | HTTP: `static` or `passthrough` (see [Authentication modes](#authentication-modes)) | `static` |
| `ARC_MCP_TLS_UPSTREAM` | HTTP: `true` when TLS terminates in front of arc-mcp (lets passthrough listen without `--tls-cert`) | `false` |

Boolean variables take `true`/`false` (or `1`/`0`); any other value is refused at startup.

### CLI Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--arc-url` | Arc instance URL | `$ARC_URL` or `http://localhost:8000` |
| `--arc-token` | Arc API token. Visible in the process list: prefer `ARC_TOKEN_FILE`. Ignored when `ARC_TOKEN_FILE` or `ARC_TOKEN` is set | (none) |
| `--max-rows` | Maximum rows per query | `500` |
| `--timeout` | Query timeout | `30s` |
| `--max-response-size` | Size limit of an answer, in characters (minimum 4000). Applies to the text and to the structured rows alike, each stopping at a row boundary | `50000` |
| `--tools` | Tool groups to serve, comma-separated: `data`, `ops` | `$ARC_MCP_TOOLS` or `data` |
| `--transport` | `stdio` or `http` | `stdio` |
| `--listen` | HTTP: address to listen on | `127.0.0.1:8080` |
| `--insecure-listen` | HTTP: serve a non-loopback address with no inbound token | `false` |
| `--max-concurrent` | HTTP: requests served at once | `8` |
| `--tls-cert`, `--tls-key` | HTTP: serve HTTPS | (none) |
| `--allow-any-host` | HTTP: turn off DNS-rebinding protection on a loopback listener | `false` |
| `--auth-mode` | HTTP: `static` or `passthrough` | `static` |
| `--tls-upstream` | HTTP: TLS terminates in front of arc-mcp | `false` |

Every flag also has the environment variable in the table above. The inbound HTTP token has no flag, so it never shows in the process list.

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

## Remote access over HTTP

By default arc-mcp speaks stdio: the MCP client starts it as a subprocess. To run it as a service that clients reach over the network, use the Streamable HTTP transport:

```bash
export ARC_TOKEN_FILE=/etc/arc-mcp/arc-token
export ARC_MCP_HTTP_TOKEN_FILE=/etc/arc-mcp/http-token   # e.g. openssl rand -hex 32 > http-token
arc-mcp --transport http --listen 0.0.0.0:8080
```

- MCP endpoint: `http://host:8080/mcp`. Liveness: `GET /healthz` (does not call Arc, needs no token).
- **Inbound token.** With `ARC_MCP_HTTP_TOKEN_FILE` or `ARC_MCP_HTTP_TOKEN` set, every request to `/mcp` needs `Authorization: Bearer <token>`. arc-mcp refuses to listen on a non-loopback address without one, unless `--insecure-listen` is set. On `127.0.0.1` the token is optional.
- **One token for everyone** (the default, static mode). The inbound token is a shared secret for the endpoint, not per-user authentication: every client acts with arc-mcp's Arc token, and Arc sees all their queries as that token. Give arc-mcp a read-only Arc token, and use the access log (one line per request: peer, JSON-RPC method, tool) to see who did what. For a token per user, see [Authentication modes](#authentication-modes).
- **TLS.** The bearer token travels in every request. Serve HTTPS with `--tls-cert`/`--tls-key`, or terminate TLS at a proxy in front of arc-mcp. A proxy on the same host that forwards to `127.0.0.1` with the public `Host` header is refused by the DNS-rebinding protection; pass `--allow-any-host` for that setup, and only for it.
- **Stateless.** Each request is answered on its own (JSON responses, no long-lived stream), so any number of replicas can sit behind a load balancer.
- At most `--max-concurrent` requests (default 8) are served at once; the rest wait. Request bodies are limited to 2 MiB. On SIGTERM, requests in flight get up to 4 × `--timeout` + 5s to finish.
- Requests must carry `Content-Type: application/json` exactly (a `; charset=utf-8` parameter is refused with 415) and accept `application/json, text/event-stream`, as the MCP specification says. Browser clients are not supported (no CORS).

Clients connect with the URL and the header. For Claude Code:

```bash
claude mcp add --transport http arc http://host:8080/mcp --header 'Authorization: Bearer ${ARC_MCP_HTTP_TOKEN}'
```

The single quotes keep the token out of Claude Code's config: it stores `${ARC_MCP_HTTP_TOKEN}` and reads the variable from its environment when it connects.

Clients configured with JSON that support remote servers take the URL and the header. Cursor's form is below; it reads `${env:NAME}` from its environment. Field names vary (Windsurf calls the URL `serverUrl`), so check your client's documentation:

```json
{
  "mcpServers": {
    "arc": {
      "url": "https://arc-mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer ${env:ARC_MCP_HTTP_TOKEN}" }
    }
  }
}
```

### Authentication modes

| | `static` (default) | `passthrough` |
|---|---|---|
| Arc token used | arc-mcp's own (`ARC_TOKEN_FILE`) | each caller's own |
| Clients send | the shared `ARC_MCP_HTTP_TOKEN` | their Arc token |
| Arc's permissions and limits | the same for every client | per caller |
| Use it when | one team, one read token, a private network | several users or tenants who each have Arc tokens |

With `--auth-mode passthrough` (`ARC_MCP_AUTH_MODE=passthrough`), each client sends its own Arc token as `Authorization: Bearer <token>`. arc-mcp checks it with Arc's `/api/v1/auth/verify` and sends it on every request it makes to Arc for that client; it never queries Arc with a token of its own. Arc then applies each caller's permissions, database scopes and row caps.

```bash
ARC_URL=https://arc.example.com arc-mcp --transport http --listen 0.0.0.0:8080 --auth-mode passthrough --tls-cert cert.pem --tls-key key.pem
claude mcp add --transport http arc https://arc-mcp.example.com/mcp --header 'Authorization: Bearer ${ARC_TOKEN}'
```

- **Arc must check tokens.** arc-mcp refuses to start when Arc's authentication is off (or when it cannot tell what is answering), since callers' tokens would then decide nothing. When Arc is unreachable at startup it starts anyway, and answers 503 until Arc is back.
- **Refused combinations:** stdio (no caller token to pass); an Arc token of arc-mcp's own (`ARC_TOKEN`, `ARC_TOKEN_FILE`, `--arc-token`); the shared `ARC_MCP_HTTP_TOKEN` (both are the `Authorization` header); `--insecure-listen`; an Arc URL with a query string or userinfo (Arc could read a token from them instead of the caller's).
- A missing or invalid token gets 401; when Arc cannot answer the check, 503 with `Retry-After`. A valid token is remembered for up to 60 seconds (never past its expiry), so a token revoked in Arc still passes arc-mcp's check for up to a minute, but every query it sends is refused by Arc at once, and arc-mcp then forgets it.
- arc-mcp's read-only checks, row cap and size limit apply whatever the caller's token allows: an admin token reads no more than a read token. A token carrying write, delete or admin is logged once with a warning; callers should use read tokens.
- **Who did what.** The access log names each caller by Arc's token id (`caller=arc-token:42`), and Arc's own audit log, when enabled (an Arc Enterprise feature; reads are recorded only with `audit_log.include_reads`), records the token's name for each query. Arc sees every request coming from arc-mcp's address, so the access log's `peer=` is the only record of the caller's address.
- **TLS is required** on a non-loopback address: the bearer token is a caller's Arc credential. Use `--tls-cert`/`--tls-key`, or `--tls-upstream` (`ARC_MCP_TLS_UPSTREAM`) when a proxy or ingress terminates TLS in front of arc-mcp. The Arc URL follows the same rule as a static token: `https://`, a loopback address, or `--insecure`.
- **Helm:** `--set auth.mode=passthrough`, with no `arc.token`, `arc.tokenSecretRef` or `http.tokenSecretRef`; with `http.tlsSecretName` or `http.tlsUpstream=true`; and an `https://` `arc.url` or `arc.insecure=true` (the default `arc.url` is plain HTTP inside the cluster).

**Upgrading from the `arc-mcp-gateway` image.** Earlier releases exposed HTTP through a separate image that wrapped arc-mcp with supergateway, serving the older HTTP+SSE transport at `/sse`. That image is no longer published. Run the `arc-mcp` image with `--transport http` instead, and point clients at `/mcp`. With Docker Compose, set `ARC_MCP_VERSION` in your `.env` to a release that has the HTTP transport (an `.env` copied from an older example pins one that does not) and add `ARC_MCP_HTTP_TOKEN`.

With the Helm chart, the `gateway.*` values are gone and the chart refuses to render while they are set (with `helm upgrade --reuse-values`, use `--reset-then-reuse-values`). The new values:

| Value | Meaning | Default |
|-------|---------|---------|
| `http.tokenSecretRef.name`, `.key` | Secret holding the inbound bearer token, mounted as a file | (none), `token` |
| `http.insecureListen` | Serve without an inbound token (restrict who can reach the Service yourself) | `false` |
| `http.tlsSecretName` | `kubernetes.io/tls` Secret to serve HTTPS from | (none) |
| `http.maxConcurrent` | Requests served at once | `8` |
| `http.port` | Container port | `8080` |
| `terminationGracePeriodSeconds` | Shutdown grace; keep it above 4 × `arc.timeout` + 5s | `130` |
| `extraArgs`, `extraEnv` | More flags and environment variables for arc-mcp | `[]` |

One of `http.tokenSecretRef.name` and `http.insecureListen` is required. Probes use `/healthz`.

## Docker Compose (quick-start)

The fastest way to evaluate arc-mcp is with Docker Compose — it brings up Arc and arc-mcp together, serving MCP over Streamable HTTP for remote LLM clients:

```bash
cp .env.example .env
# edit .env — at minimum set ARC_TOKEN and ARC_MCP_HTTP_TOKEN
docker compose --profile with-arc up
```

- Arc API:      `http://localhost:8000`
- MCP endpoint: `http://localhost:8080/mcp` (send `Authorization: Bearer $ARC_MCP_HTTP_TOKEN`)

The port is published on the host's loopback address only. The inbound token is required even so: without it, a web page open in a browser on the same machine could reach the endpoint through DNS rebinding.

If you already run Arc elsewhere, omit the `--profile with-arc` flag, set `ARC_URL` and `ARC_TOKEN` in `.env`, and run:

```bash
docker compose up arc-mcp
```

See [`docker-compose.yml`](docker-compose.yml) and [`.env.example`](.env.example) for the full surface.

## Connecting to LLM Clients

The examples below run arc-mcp locally over **stdio**: the client starts it as a subprocess. For a shared, remote arc-mcp, see [Remote access over HTTP](#remote-access-over-http).

Every example passes the Arc URL and the token **in the environment, not as arguments**: arguments show in the process list, and `ARC_TOKEN_FILE` keeps the token out of the client's config file too. Put a read-only token (see [Token hygiene](#token-hygiene)) in a file only you can read, without it passing through your shell history:

```bash
mkdir -p ~/.config/arc-mcp
(umask 077; cat > ~/.config/arc-mcp/token)
# paste the token, press Enter, then Ctrl-D
```

Use absolute paths in the configs below (`/Users/you/...`, not `~`): clients start arc-mcp without a shell, so `~` is not expanded, and desktop apps may not see your shell's `PATH` either. `which arc-mcp` (`where arc-mcp` on Windows) prints the path to put in `command`; `go install` puts it in `$(go env GOPATH)/bin`, usually `~/go/bin`.

### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS), `%APPDATA%\Claude\claude_desktop_config.json` (Windows) or `~/.config/Claude/claude_desktop_config.json` (Linux), then restart Claude Desktop. On Windows, `command` ends in `arc-mcp.exe`, and each backslash in a path is written `\\` in JSON:

```json
{
  "mcpServers": {
    "arc": {
      "command": "/Users/you/go/bin/arc-mcp",
      "env": {
        "ARC_URL": "http://localhost:8000",
        "ARC_TOKEN_FILE": "/Users/you/.config/arc-mcp/token"
      }
    }
  }
}
```

### Claude Code (CLI)

Register it with `claude mcp add`:

```bash
claude mcp add arc \
  -e ARC_URL=http://localhost:8000 \
  -e 'ARC_TOKEN_FILE=${HOME}/.config/arc-mcp/token' \
  -- "$(which arc-mcp)"
```

The single quotes keep `${HOME}` unexpanded: Claude Code expands it when it starts arc-mcp, so the config holds no home directory of yours. The default scope is local (this project, only you). `--scope user` makes it available in every project; `--scope project` writes it to the project's `.mcp.json`, to share with your team: commit that file with `ARC_TOKEN_FILE`, never a token (and each teammate needs arc-mcp at the same path, or use plain `arc-mcp` if it is on everyone's `PATH`). `claude mcp list` shows what is registered.

### Cursor

Add to `.cursor/mcp.json` in your project root (or `~/.cursor/mcp.json` for every project). `${userHome}` keeps your home directory out of a file you may commit:

```json
{
  "mcpServers": {
    "arc": {
      "type": "stdio",
      "command": "${userHome}/go/bin/arc-mcp",
      "env": {
        "ARC_URL": "http://localhost:8000",
        "ARC_TOKEN_FILE": "${userHome}/.config/arc-mcp/token"
      }
    }
  }
}
```

### Windsurf

Open the MCP config file from Cascade's MCP settings ("Open MCP config file"; `~/.codeium/windsurf/mcp_config.json` in many versions) and add:

```json
{
  "mcpServers": {
    "arc": {
      "command": "/Users/you/go/bin/arc-mcp",
      "env": {
        "ARC_URL": "http://localhost:8000",
        "ARC_TOKEN_FILE": "/Users/you/.config/arc-mcp/token"
      }
    }
  }
}
```

### Remote clients

A client on another machine connects to an arc-mcp running with `--transport http`, with the endpoint URL and an `Authorization` header: see [Remote access over HTTP](#remote-access-over-http) for the server and for Claude Code's and Cursor's remote configs.

ChatGPT authenticates custom MCP servers (added under Plugins) with OAuth or not at all, never with a static bearer token, so it cannot use arc-mcp's HTTP endpoint; OAuth is tracked in [#2](https://github.com/basekick-labs/arc-mcp/issues/2). Do not expose arc-mcp without authentication to make it work: anyone who finds the URL could query Arc. OpenAI's Secure MCP Tunnel can instead run a stdio server inside your network; we have not tested it with arc-mcp.

### Any MCP-compatible client

arc-mcp speaks standard MCP over stdio. Start it with the Arc URL and token in its environment:

```bash
ARC_URL=http://your-arc-host:8000 ARC_TOKEN_FILE=/path/to/token arc-mcp
```

The server reads JSON-RPC from stdin and writes to stdout. All logs go to stderr.

## Available Tools

### Structured output

Every tool declares an MCP `outputSchema` and returns `structuredContent` on success, next to its markdown text: `query` and `get_sample_data` return `columns`, `column_types` (Arc's type name per column, such as `int64`, `utf8` or `timestamp[us]`; empty with an Arc before 27.01.1), `rows` (numbers exactly as Arc sent them), `row_count`, `truncated` with the reasons (`row_limit`, `arc_row_cap`, `size_budget`, `stream_incomplete`) and `no_data`; `describe_measurement` returns columns with types and typed statistics; the listing tools return their lists. Clients that only read text are unaffected.

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

## Operations tools (opt-in)

Two more tools report on the Arc server itself rather than on the data in it.
They are off by default, so a data-only setup never sees them: turn them on
with `--tools=data,ops` (or `ARC_MCP_TOOLS=data,ops`), or serve them alone with
`--tools=ops`. A name that is not a group stops arc-mcp at startup with the list
of valid ones; an empty value means `data`. In the Helm chart, set `arc.tools`
as a list: `--set 'arc.tools={data,ops}'`.

### arc_health

Probes `/health` (is Arc alive) and `/ready` (should traffic go to it) and
returns each probe's HTTP status and Arc's answer. A not-ready answer (HTTP
503, with Arc's reason) is a result, not an error. Arc answers these probes
without checking the token, so a healthy answer does not mean the token works.

**Example prompt:** "Is Arc healthy?"

### arc_metrics

Reads one family of Arc's own metrics:

| `family` | Arc route | What it holds |
|---|---|---|
| `endpoints` | `/api/v1/metrics/endpoints` | HTTP, ingest, query, buffer, storage, compaction and auth activity |
| `query_pool` | `/api/v1/metrics/query-pool` | The DuckDB connection pool |
| `memory` | `/api/v1/metrics/memory` | Go runtime and garbage collector |
| `counters` | `/api/v1/metrics` | Every counter Arc keeps (the largest answer) |
| `system_history` | `/api/v1/metrics/timeseries/system` | CPU, memory and goroutines over time |
| `application_history` | `/api/v1/metrics/timeseries/application` | Ingest and query counters over time |
| `api_history` | `/api/v1/metrics/timeseries/api` | HTTP requests and latency over time |

`minutes` (history families only, 1 to 1440, default 10) is the look-back. Arc
records a point every 5 seconds and keeps 30 minutes of them by default
(`metrics.timeseries_interval_seconds`, `metrics.timeseries_retention_minutes`
in Arc's configuration). A history answer carries at most 20 points: when Arc
has more, arc-mcp keeps every k-th one counting back from the newest, which is
always kept, and says so in `sampled_every`. `from` and `to` are the timestamps
of the oldest and the newest point returned, the window actually covered.
Numbers come through exactly as Arc sends them.

Answers are never cut short to fit `--max-response-size`: history is sampled
further (fewer points), `arc_health` drops Arc's bodies and keeps the statuses,
and a snapshot that does not fit is refused, naming the setting, as is history
when not even the newest point fits. The limit applies to each answer's text; the
same answer is also attached as structured content (JSON a client can read
without parsing the text), so what crosses the wire can be about twice that
size.

```json
{"family":"api_history","history":{"from":"2026-11-27T01:35:36.157-03:00","to":"2026-11-27T01:45:06.156-03:00","total_points":120,"sampled_every":6,"points":[{"timestamp":"2026-11-27T01:35:36.157-03:00","values":{"http_requests_total":51200,"http_latency_avg_us":840,"...":"..."}},"..."]}}
```

**Example prompts:**
- "Is Arc healthy?" → `arc_health`
- "Why are queries slow?" → `arc_metrics` with `query_pool`, then `api_history`
- "Is ingest keeping up?" → `arc_metrics` with `endpoints`, then `application_history`

Both tools describe only the Arc node at the configured URL. In a cluster each
node answers for itself, so point arc-mcp at the node you want to inspect;
behind a load balancer or a Kubernetes Service, successive calls may reach
different nodes. Arc's application logs are not included: reading them needs an
admin token, and arc-mcp asks for a read-only one.

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
  stdio or Streamable HTTP     REST API
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
ARC_URL=http://localhost:8000 ARC_TOKEN_FILE=/path/to/token ./arc-mcp
```

## License

MIT
