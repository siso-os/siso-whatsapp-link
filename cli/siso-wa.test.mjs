// node --test apps/whatsapp-link/cli/siso-wa.test.mjs  (a fake gateway on loopback; fixture data only, nothing reaches WhatsApp)
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { execFile } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const CLI = join(dirname(fileURLToPath(import.meta.url)), "siso-wa.mjs");
const TOKEN = "f".repeat(64);
const GROUP = "120000000000000002@g.us";

function gateway(agentChats) {
  const seen = [];
  const srv = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      seen.push({ method: req.method, url: req.url, auth: req.headers.authorization, body });
      const send = (code, v) => {
        res.writeHead(code, { "Content-Type": "application/json" });
        res.end(JSON.stringify(v));
      };
      if (req.headers.authorization !== `Bearer ${TOKEN}`) return send(401, { error: "unauthorized" });
      if (req.url === "/health")
        return send(200, {
          link: { state: "connected", loggedIn: true, connected: true, since: 1763290000 },
          version: "0.3.0", sendEnabled: true, agentChats, agentSendsDay: 1, agentSendPerDay: 20,
        });
      if (req.method === "POST" && req.url === `/chats/${encodeURIComponent(GROUP)}/send`)
        return send(200, { id: JSON.parse(body).image ? "IMGID1" : "MSGID1" });
      if (req.url.startsWith(`/chats/${encodeURIComponent(GROUP)}/messages`))
        return send(200, {
          chat: { jid: GROUP, name: "Fixture Agent Group" },
          messages: [
            { ts: 1763290100, fromMe: true, kind: "text", text: "SISO agent: build is green" },
            { ts: 1763290200, fromMe: false, sender: "100000000002@s.whatsapp.net", senderName: "Fixture Bob", kind: "text", text: "thanks" },
          ],
        });
      send(403, { error: "forbidden" });
    });
  });
  return new Promise((ok) => srv.listen(0, "127.0.0.1", () => ok({ srv, seen, url: `http://127.0.0.1:${srv.address().port}` })));
}

function run(url, ...args) {
  const dir = mkdtempSync(join(tmpdir(), "siso-wa-"));
  const env = join(dir, "agent.env");
  writeFileSync(env, `# test\nWA_URL=${url}\nWA_AGENT_TOKEN=${TOKEN}\n`, { mode: 0o600 });
  return new Promise((ok) =>
    execFile("node", [CLI, ...args], { env: { ...process.env, SISO_WA_ENV: env } }, (err, stdout, stderr) =>
      ok({ code: err ? err.code : 0, stdout, stderr }),
    ),
  );
}

test("no agent chat bound: post and read exit 2 and send nothing", async () => {
  const g = await gateway([]);
  try {
    const p = await run(g.url, "post", "x");
    assert.equal(p.code, 2);
    assert.match(p.stderr, /no agent chat bound yet: run deploy\/bind-agent-chat\.sh/);
    assert.equal((await run(g.url, "read")).code, 2);
    assert.ok(!g.seen.some((r) => r.method === "POST"), "nothing was posted");
    const h = await run(g.url, "health");
    assert.equal(h.code, 0);
    assert.match(h.stdout, /link connected \(logged in\).*agent chat: none bound/);
  } finally {
    g.srv.close();
  }
});

test("bound: post sends the text with the agent token and prints the id; read prints the chat", async () => {
  const g = await gateway([GROUP]);
  try {
    const p = await run(g.url, "post", "build", "is", "green");
    assert.equal(p.code, 0, p.stderr);
    assert.equal(p.stdout.trim(), "MSGID1");
    const sent = g.seen.find((r) => r.method === "POST");
    assert.equal(sent.auth, `Bearer ${TOKEN}`);
    assert.deepEqual(JSON.parse(sent.body), { text: "build is green" });
    const r = await run(g.url, "read", "--limit", "5");
    assert.equal(r.code, 0, r.stderr);
    assert.match(r.stdout, /agent: SISO agent: build is green/);
    assert.match(r.stdout, /Fixture Bob: thanks/);
    assert.ok(g.seen.some((q) => q.url.endsWith("/messages?limit=5")));
    assert.equal((await run(g.url, "post", "x", "--chat", "999@g.us")).code, 1, "an unbound chat is refused locally");
  } finally {
    g.srv.close();
  }
});

// A real 1x1 PNG and JPEG (the smallest valid files), so the CLI's own type check sees true signatures.
const PNG_1PX = Buffer.from(
  "89504e470d0a1a0a0000000d4948445200000001000000010806000000" + "1f15c4890000000d49444154789c636060606000000005000165a2c6a50000000049454e44ae426082",
  "hex",
);

function fixtureFile(name, bytes) {
  const dir = mkdtempSync(join(tmpdir(), "siso-wa-img-"));
  const p = join(dir, name);
  writeFileSync(p, bytes);
  return p;
}

test("post --image sends the picture base64 with its caption and prints the id", async () => {
  const g = await gateway([GROUP]);
  try {
    const file = fixtureFile("shot.png", PNG_1PX);
    const p = await run(g.url, "post", "--image", file, "--caption", "*6. Watch*\nthree shops");
    assert.equal(p.code, 0, p.stderr);
    assert.equal(p.stdout.trim(), "IMGID1");
    const sent = g.seen.find((r) => r.method === "POST");
    assert.equal(sent.auth, `Bearer ${TOKEN}`);
    assert.deepEqual(JSON.parse(sent.body), { image: PNG_1PX.toString("base64"), caption: "*6. Watch*\nthree shops" });
    const q = await run(g.url, "post", `--image=${file}`);
    assert.equal(q.code, 0, q.stderr);
    assert.deepEqual(JSON.parse(g.seen.filter((r) => r.method === "POST")[1].body), { image: PNG_1PX.toString("base64"), caption: "" });
  } finally {
    g.srv.close();
  }
});

test("post --image refuses a wrong type, a missing file, an oversize file or text beside it, sending nothing", async () => {
  const g = await gateway([GROUP]);
  try {
    const gif = fixtureFile("a.png", Buffer.from("GIF89a" + "x".repeat(20)));
    const r1 = await run(g.url, "post", "--image", gif, "--caption", "x");
    assert.equal(r1.code, 64);
    assert.match(r1.stderr, /only PNG or JPEG/);
    const r2 = await run(g.url, "post", "--image", "/nonexistent/shot.png");
    assert.equal(r2.code, 64);
    assert.match(r2.stderr, /cannot read/);
    const big = fixtureFile("big.png", Buffer.concat([PNG_1PX, Buffer.alloc(5 * 1024 * 1024)]));
    const r3 = await run(g.url, "post", "--image", big);
    assert.equal(r3.code, 64);
    assert.match(r3.stderr, /max 5 MB/);
    const ok = fixtureFile("ok.png", PNG_1PX);
    const r4 = await run(g.url, "post", "--image", ok, "loose", "text");
    assert.equal(r4.code, 64);
    assert.match(r4.stderr, /--caption/);
    const r5 = await run(g.url, "post", "--caption", "no image");
    assert.equal(r5.code, 64);
    assert.ok(!g.seen.some((r) => r.method === "POST"), "nothing was posted");
  } finally {
    g.srv.close();
  }
});
