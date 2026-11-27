// Starts an unpacked arc-mcp extension the way Claude Desktop does: the
// manifest's mcp_config resolved by mcpb's own resolver for this platform,
// then MCP over stdio (initialize, tools/list, and list_databases when an
// Arc is expected). CI runs it on macOS and Windows against an unreachable
// Arc; locally it runs against a real one.
//
//   MCPB_PKG=<installed @anthropic-ai/mcpb dir> node mcpb/smoke.mjs <unpacked-dir>
//
// SMOKE_ARC_URL       the arc_url to configure (a non-default one, so a lost
//                     ARC_URL cannot hide behind the localhost default)
// SMOKE_ARC_TOKEN_FILE  the arc_token comes from this file (a dummy otherwise)
// SMOKE_EXPECT_ARC=1  Arc is reachable: require "Connected to Arc at <url>",
//                     "Arc token permissions: <SMOKE_EXPECT_PERMISSIONS>" and a
//                     list_databases result that names SMOKE_EXPECT_DB if set
//
// The token never goes on a command line, and nothing here prints it.
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { join, resolve as resolvePath, sep } from "node:path";
import { pathToFileURL } from "node:url";

const fail = (msg) => {
  console.error(`smoke: FAIL: ${msg}`);
  process.exit(1);
};
if (!process.argv[2] || !process.env.MCPB_PKG) fail("usage: MCPB_PKG=<dir> node smoke.mjs <unpacked-dir>");
const dir = resolvePath(process.argv[2]);
const { getMcpConfigForManifest, hasRequiredConfigMissing } = await import(
  pathToFileURL(join(process.env.MCPB_PKG, "dist", "index.js")).href
);
const manifest = JSON.parse(readFileSync(join(dir, "manifest.json"), "utf8"));
const url = process.env.SMOKE_ARC_URL || "http://127.0.0.1:9";
const token = process.env.SMOKE_ARC_TOKEN_FILE
  ? readFileSync(process.env.SMOKE_ARC_TOKEN_FILE, "utf8").trim()
  : "smoke-dummy-token";
const expectArc = process.env.SMOKE_EXPECT_ARC === "1";

const resolve = (userConfig) =>
  getMcpConfigForManifest({ manifest, extensionPath: dir, systemDirs: {}, userConfig, pathSeparator: sep });

// The form: an empty token keeps the server from starting; an empty URL
// falls back to the manifest default.
if (!hasRequiredConfigMissing({ manifest, userConfig: {} })) fail("a missing arc_token is not reported as missing");
const byDefault = await resolve({ arc_token: token });
if (byDefault?.env?.ARC_URL !== "http://localhost:8000") fail(`arc_url left empty resolves to ${byDefault?.env?.ARC_URL}`);

const cfg = await resolve({ arc_url: url, arc_token: token });
if (!cfg) fail("the manifest resolves to no server config");
const line = [cfg.command, ...(cfg.args || [])].join(" ");
if (line.includes(token)) fail("the token is on the command line");
if (cfg.env.ARC_TOKEN !== token || cfg.env.ARC_URL !== url || cfg.env.ARC_TOKEN_FILE !== "")
  fail("ARC_URL, ARC_TOKEN or ARC_TOKEN_FILE did not resolve as configured");
console.log(`smoke: ${process.platform}: ${line}`);

// A hostile inherited environment: every one of these would change what
// arc-mcp serves or how; the manifest's args and env must win.
const env = {
  ...process.env,
  ARC_MCP_TRANSPORT: "http",
  ARC_MCP_AUTH_MODE: "passthrough",
  ARC_MCP_TOOLS: "data,ops",
  ARC_MCP_INSECURE: "true",
  ARC_TOKEN_FILE: join(dir, "no-such-token-file"),
  ...cfg.env,
};
const child = spawn(cfg.command, cfg.args || [], { env, stdio: ["pipe", "pipe", "pipe"] });
let stderr = "";
child.stderr.on("data", (d) => (stderr += d));
const timer = setTimeout(() => {
  child.kill();
  fail(`no answer within 30s; stderr:\n${stderr}`);
}, 30000);

const pending = new Map();
let buf = "";
child.stdout.on("data", (d) => {
  buf += d;
  let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const msg = JSON.parse(buf.slice(0, i));
    buf = buf.slice(i + 1);
    pending.get(msg.id)?.(msg);
  }
});
child.on("exit", (code) => {
  if (pending.size) {
    clearTimeout(timer);
    fail(`arc-mcp exited (${code}) before answering; stderr:\n${stderr}`);
  }
});
let next = 1;
const call = (method, params) =>
  new Promise((ok) => {
    const id = next++;
    pending.set(id, (msg) => {
      pending.delete(id);
      ok(msg);
    });
    child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
  });

const init = await call("initialize", {
  protocolVersion: "2025-06-18",
  capabilities: {},
  clientInfo: { name: "arc-mcp-smoke", version: "1" },
});
if (init.error) fail(`initialize: ${JSON.stringify(init.error)}`);
child.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) + "\n");
const list = await call("tools/list", {});
const got = (list.result?.tools || []).map((t) => t.name).sort();
const want = manifest.tools.map((t) => t.name).sort();
if (JSON.stringify(got) !== JSON.stringify(want)) fail(`tools/list ${got} is not the manifest's ${want}`);

let dbs = "";
if (expectArc) {
  const res = await call("tools/call", { name: "list_databases", arguments: {} });
  dbs = JSON.stringify(res.result?.content || []);
  if (res.error || res.result?.isError) fail(`list_databases: ${JSON.stringify(res.error || res.result)}`);
  if (process.env.SMOKE_EXPECT_DB && !dbs.includes(process.env.SMOKE_EXPECT_DB))
    fail(`list_databases does not name ${process.env.SMOKE_EXPECT_DB}`);
}
child.stdin.end();
await new Promise((ok) => child.on("close", ok));
clearTimeout(timer);

console.log(stderr.trimEnd().replace(/^/gm, "smoke: arc-mcp: "));
const need = ["Tool groups: data\n"];
if (expectArc) {
  need.push(`Connected to Arc at ${url}\n`);
  need.push(`Arc token permissions: ${process.env.SMOKE_EXPECT_PERMISSIONS || "read"}\n`);
} else {
  need.push(`reachable at ${url}\n`);
}
for (const s of need) if (!stderr.includes(s)) fail(`stderr lacks ${JSON.stringify(s.trim())}`);
console.log(`smoke: ok: ${got.length} tools${expectArc ? ", list_databases answered" : ""}, URL and token from env`);
