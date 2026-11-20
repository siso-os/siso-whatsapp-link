#!/usr/bin/env node
// siso-wa: SISO agents post into the ONE agent WhatsApp group (and read it) through the gateway on the Mac mini.
// It holds the agent token only, which the gateway limits to /health and the chats in WA_AGENT_CHATS; every send is
// labelled "SISO agent: " by the gateway and capped at 20 a day. Shaan, 16 Nov 2026: agents post only in the agent
// group, labelled, so no one mistakes them for him typing.
//
//   siso-wa health                 is the link connected; which chat is bound; agent sends today
//   siso-wa post "<text>"          post to the bound agent chat (text "-" reads stdin); prints the message id
//   siso-wa post --image FILE [--caption TEXT]
//                                  post a PNG or JPEG (max 5 MB) with a caption (labelled too; "-" reads stdin)
//   siso-wa read [--limit N]       that chat's recent messages, oldest first (for reading replies)
//   --json                         raw gateway JSON (health, read)
//
// Config: ~/.config/siso-secrets/whatsapp-agent.env (0600, written by deploy/agent-env.sh): WA_URL, WA_AGENT_TOKEN.
// SISO_WA_ENV points at another file. Exit codes: 0 ok, 1 error or refused, 2 no agent chat bound, 64 usage.
// No dependencies (Node 18+). Source: sisodias/siso apps/whatsapp-link/cli/siso-wa.mjs.

import { readFileSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const ENV_PATH = process.env.SISO_WA_ENV || join(homedir(), ".config/siso-secrets/whatsapp-agent.env");
const UNBOUND = "no agent chat bound yet: run deploy/bind-agent-chat.sh";
const IMAGE_MAX = 5 * 1024 * 1024; // the gateway's limit (media.go imageMaxBytes); it checks again

// readImage checks a picture before it leaves the laptop: PNG or JPEG by its own bytes, at most 5 MB.
function readImage(file) {
  let size, bytes;
  try {
    size = statSync(file).size;
    if (size > IMAGE_MAX) die(`${file}: image too large (max 5 MB)`, 64);
    bytes = readFileSync(file);
  } catch (e) {
    die(`cannot read ${file} (${e.code || e.message})`, 64);
  }
  const png = bytes.subarray(0, 8).equals(Buffer.from("89504e470d0a1a0a", "hex"));
  const jpg = bytes.subarray(0, 3).equals(Buffer.from("ffd8ff", "hex"));
  if (!png && !jpg) die(`${file}: only PNG or JPEG images can be sent`, 64);
  return bytes;
}

function die(msg, code = 1) {
  process.stderr.write(`siso-wa: ${msg}\n`);
  process.exit(code);
}

function loadEnv() {
  let text;
  try {
    text = readFileSync(ENV_PATH, "utf8");
  } catch {
    die(`no config at ${ENV_PATH}: run apps/whatsapp-link/deploy/agent-env.sh`);
  }
  const env = {};
  for (const line of text.split("\n")) {
    const m = line.match(/^\s*([A-Z_][A-Z0-9_]*)\s*=\s*(.*?)\s*$/);
    if (m) env[m[1]] = m[2].replace(/^["']|["']$/g, "");
  }
  if (!env.WA_URL || !env.WA_AGENT_TOKEN) die(`${ENV_PATH} needs WA_URL and WA_AGENT_TOKEN`);
  return { url: env.WA_URL.replace(/\/+$/, ""), token: env.WA_AGENT_TOKEN };
}

async function call(cfg, method, path, body, timeoutMs = 30_000) {
  let res;
  try {
    res = await fetch(cfg.url + path, {
      method,
      headers: { Authorization: `Bearer ${cfg.token}`, ...(body ? { "Content-Type": "application/json" } : {}) },
      body: body ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(timeoutMs), // an image send uploads first: the gateway allows it 60 s
    });
  } catch (e) {
    die(`gateway unreachable at ${cfg.url} (${e.cause?.code || e.name})`);
  }
  let data = {};
  try {
    data = await res.json();
  } catch {}
  return { status: res.status, data };
}

async function health(cfg) {
  const r = await call(cfg, "GET", "/health");
  if (r.status !== 200) die(`health: ${r.status} ${r.data.error || ""}`.trim());
  return r.data;
}

function boundChat(h, want) {
  const chats = h.agentChats || [];
  if (chats.length === 0) die(UNBOUND, 2);
  if (want) {
    if (!chats.includes(want)) die(`${want} is not an agent chat (bound: ${chats.join(", ")})`);
    return want;
  }
  if (chats.length > 1) die(`more than one agent chat is bound; pick one with --chat (${chats.join(", ")})`);
  return chats[0];
}

function parseArgs(argv) {
  const out = { _: [], json: false, limit: 30, chat: "", image: "", caption: undefined };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--json") out.json = true;
    else if (a === "--limit") out.limit = Number(argv[++i]);
    else if (a.startsWith("--limit=")) out.limit = Number(a.slice(8));
    else if (a === "--chat") out.chat = argv[++i] || "";
    else if (a.startsWith("--chat=")) out.chat = a.slice(7);
    else if (a === "--image") out.image = argv[++i] || "";
    else if (a.startsWith("--image=")) out.image = a.slice(8);
    else if (a === "--caption") out.caption = argv[++i] ?? "";
    else if (a.startsWith("--caption=")) out.caption = a.slice(10);
    else if (a === "-h" || a === "--help") out.help = true;
    else out._.push(a);
  }
  if (!Number.isInteger(out.limit) || out.limit < 1 || out.limit > 500) die("--limit is a whole number from 1 to 500", 64);
  return out;
}

function when(ts) {
  return new Date(ts * 1000).toISOString().slice(0, 16).replace("T", " ") + "Z";
}

async function readStdin() {
  const chunks = [];
  for await (const c of process.stdin) chunks.push(c);
  return Buffer.concat(chunks).toString("utf8");
}

const USAGE = `usage: siso-wa health [--json]
       siso-wa post "<text>"   (text "-" reads stdin; posts to the bound agent chat, labelled "SISO agent: ")
       siso-wa post --image FILE [--caption TEXT]   (PNG or JPEG, max 5 MB; the caption is labelled; "-" reads stdin)
       siso-wa read [--limit N] [--json]`;

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const [cmd, ...rest] = args._;
  if (args.help || !cmd) {
    process.stdout.write(USAGE + "\n");
    process.exit(cmd || args.help ? 0 : 64);
  }
  const cfg = loadEnv();

  if (cmd === "health") {
    const h = await health(cfg);
    if (args.json) return void process.stdout.write(JSON.stringify(h, null, 2) + "\n");
    const l = h.link || {};
    const since = l.since ? ` since ${when(l.since)}` : "";
    const chats = (h.agentChats || []).length ? h.agentChats.join(", ") : "none bound";
    process.stdout.write(
      `link ${l.state || "unknown"} (${l.loggedIn ? "logged in" : "not logged in"})${since} · gateway ${h.version} · ` +
        `send ${h.sendEnabled ? "on" : "off"} · agent chat: ${chats} · agent sends today ${h.agentSendsDay ?? "?"}/${h.agentSendPerDay ?? 20}\n`,
    );
    return;
  }

  if (cmd === "post") {
    let body;
    if (args.image) {
      if (rest.length) die(`with --image the words go in --caption "<text>"\n${USAGE}`, 64);
      const image = readImage(args.image).toString("base64");
      let caption = args.caption ?? "";
      if (caption === "-") caption = await readStdin();
      body = { image, caption };
    } else {
      if (args.caption !== undefined) die(`--caption goes with --image FILE\n${USAGE}`, 64);
      let text = rest.join(" ");
      if (text === "-") text = await readStdin();
      if (!text.trim()) die(USAGE, 64);
      body = { text };
    }
    const jid = boundChat(await health(cfg), args.chat);
    const r = await call(cfg, "POST", `/chats/${encodeURIComponent(jid)}/send`, body, body.image ? 90_000 : 30_000);
    if (r.status !== 200 || !r.data.id) {
      const extra = r.data.retryAfterMs ? ` (retry in ${Math.ceil(r.data.retryAfterMs / 1000)} s)` : "";
      die(`refused ${r.status}: ${r.data.error || "no answer"}${extra}`);
    }
    process.stdout.write(r.data.id + "\n");
    return;
  }

  if (cmd === "read") {
    const jid = boundChat(await health(cfg), args.chat);
    const r = await call(cfg, "GET", `/chats/${encodeURIComponent(jid)}/messages?limit=${args.limit}`);
    if (r.status !== 200) die(`read: ${r.status} ${r.data.error || ""}`.trim());
    if (args.json) return void process.stdout.write(JSON.stringify(r.data, null, 2) + "\n");
    process.stdout.write(`# ${r.data.chat?.name || jid} (${jid})\n`);
    for (const m of r.data.messages || []) {
      const text = m.text || "";
      const who = m.fromMe ? (text.startsWith("SISO agent: ") ? "agent" : "Shaan") : m.senderName || m.sender || "?";
      const kind = m.deleted ? "[deleted] " : m.kind && m.kind !== "text" ? `[${m.kind}] ` : "";
      process.stdout.write(`${when(m.ts)}  ${who}: ${kind}${text.replace(/\n/g, "\n    ")}\n`);
    }
    return;
  }

  die(`unknown command "${cmd}"\n${USAGE}`, 64);
}

main();
