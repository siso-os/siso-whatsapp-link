#!/usr/bin/env node
// The soak report: reads the gateway's flight recorder (GET /soak, numbers and states only) and the chaos results,
// checks them against the acceptance limits below, draws the memory and state graph, and composes a siso-shell U19
// page.  node soak/report.mjs [--hours 24 | --since UNIX | --from-link] [--post] [--out soak/results/report.html]
// --post sends it to the console as card WHATSAPP-soak (updated in place on every run).
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync, mkdirSync, existsSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const arg = (k, d) => { const i = process.argv.indexOf(k); return i > 0 ? process.argv[i + 1] : d; };
const flag = (k) => process.argv.includes(k);
const LIMITS = { hours: 24, coverage: 0.99, connected: 0.99, rssMaxMB: 300, rssGrowthMB: 50, backSec: 120, gapSec: 150 };

const cfgUrl = JSON.parse(readFileSync(path.join(homedir(), ".config/siso/whatsapp-link.json"), "utf8")).url.replace(/\/$/, "");
const token = execFileSync("security", ["find-generic-password", "-s", "siso-whatsapp-link-token", "-a", "siso", "-w"], { encoding: "utf8" }).trim();
const get = async (p) => { const r = await fetch(cfgUrl + p, { headers: { authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(15000) }); if (!r.ok) throw new Error(`${p} ${r.status}`); return r.json(); };

const now = Math.floor(Date.now() / 1000);
const hours = Number(arg("--hours", LIMITS.hours));
let since = Number(arg("--since", now - hours * 3600));
const health = await get("/health");
let rows = (await get(`/soak?since=${flag("--from-link") ? 0 : since}`)).rows.sort((a, b) => a.ts - b.ts);
if (flag("--from-link")) { const first = rows.find((r) => r.loggedIn); since = first ? first.ts : now; rows = rows.filter((r) => r.ts >= since); }
const chaosFile = path.join(here, "results/chaos.jsonl");
const chaos = existsSync(chaosFile) ? readFileSync(chaosFile, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l)).filter((c) => c.linked && c.at >= since) : [];
// A chaos window: from just before the break until a minute after it was back (or its timeout).
const windows = chaos.map((c) => [c.at - 5, c.at + (c.backSec ?? (c.secs ?? 0) + 300) + 65]);
const inChaos = (t) => windows.some(([a, b]) => t >= a && t <= b);

const start = rows[0]?.ts ?? now, end = Math.max(now, rows.at(-1)?.ts ?? now);
const spanH = (end - start) / 3600;
const samples = rows.filter((r) => r.kind === "sample");
const minutes = Math.max(1, Math.round((end - start) / 60));
const gaps = [];
for (let i = 1; i < rows.length; i++) if (rows[i].ts - rows[i - 1].ts > LIMITS.gapSec) gaps.push([rows[i - 1].ts, rows[i].ts]);
if (rows.length && now - rows.at(-1).ts > LIMITS.gapSec) gaps.push([rows.at(-1).ts, now]);
const gapSec = gaps.filter(([a]) => !inChaos(a)).reduce((s, [a, b]) => s + (b - a), 0);
const calm = samples.filter((r) => !inChaos(r.ts));
const calmMinutes = Math.max(1, minutes - Math.round(windows.reduce((s, [a, b]) => s + (Math.min(b, end) - Math.max(a, start)), 0) / 60));
const connectedShare = calm.filter((r) => r.connected).length / calmMinutes;
const coverage = 1 - gapSec / Math.max(60, end - start);
const starts = rows.filter((r) => r.kind === "start");
const unplanned = starts.filter((r) => !inChaos(r.ts) && r !== rows[0]);
const byPid = new Map(); for (const r of rows) byPid.set(r.pid, Math.max(byPid.get(r.pid) ?? 0, r.reconnects));
const reconnects = [...byPid.values()].reduce((a, b) => a + b, 0);
const rss = samples.map((r) => r.rssMB).sort((a, b) => a - b);
const median = (xs) => (xs.length ? [...xs].sort((a, b) => a - b)[Math.floor(xs.length / 2)] : 0);
const hourMedian = (from, to) => median(samples.filter((r) => r.ts >= from && r.ts < to).map((r) => r.rssMB));
const growth = spanH >= 2 ? hourMedian(end - 3600, end + 1) - hourMedian(start, start + 3600) : 0;
const sum = (k, xs = samples) => xs.reduce((s, r) => s + (r[k] || 0), 0);
const live = sum("live"), self = sum("self"), lateCalm = sum("late", calm);
const lagMax = Math.max(0, ...calm.map((r) => r.lagMaxSec));
const lagAvg = live ? samples.reduce((s, r) => s + r.lagAvgSec * r.live, 0) / live : 0;
const bad = rows.filter((r) => ["logged_out", "banned", "outdated"].includes(r.state));
const linkedRows = rows.filter((r) => r.loggedIn);
const linkedH = linkedRows.length ? (linkedRows.at(-1).ts - linkedRows[0].ts) / 3600 : 0;

const fmtT = (t) => new Date(t * 1000).toLocaleString("en-GB", { timeZone: "Europe/London", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
const pct = (x) => `${(x * 100).toFixed(2)}%`;
const chk = (name, pass, observed) => ({ name: `${name} · <span class="dim">${observed}</span>`, pass: pass === null ? false : pass, ...(pass === null ? { note: "not observed yet" } : {}) });

const soakChecks = [
  chk(`linked for ${LIMITS.hours} h`, linkedH >= LIMITS.hours ? true : null, `${linkedH.toFixed(1)} h linked of ${spanH.toFixed(1)} h recorded`),
  chk(`recorder covered ≥ ${pct(LIMITS.coverage)} of minutes`, coverage >= LIMITS.coverage, `${pct(coverage)} · ${gaps.length} gap${gaps.length === 1 ? "" : "s"} > ${LIMITS.gapSec} s`),
  chk(`connected ≥ ${pct(LIMITS.connected)} of calm minutes`, linkedRows.length ? connectedShare >= LIMITS.connected : null, `${pct(connectedShare)} of ${calmMinutes} min`),
  chk("no unplanned restart", unplanned.length === 0, `${unplanned.length} unplanned · ${starts.length} starts in all`),
  chk(`memory under ${LIMITS.rssMaxMB} MB`, (rss.at(-1) ?? 0) <= LIMITS.rssMaxMB, `${rss[0] ?? 0}–${rss.at(-1) ?? 0} MB, median ${median(rss)} MB`),
  chk(`no leak: last hour within ${LIMITS.rssGrowthMB} MB of the first`, spanH >= 2 ? growth <= LIMITS.rssGrowthMB : null, spanH >= 2 ? `${growth >= 0 ? "+" : ""}${growth} MB` : "needs 2 h"),
  chk("never logged out, banned or outdated", bad.length === 0, bad.length ? bad.map((r) => r.state).join(", ") : "none"),
  chk("live messages flow in", live > 0 ? true : null, `${live} live messages, ${lateCalm} late outside the tests, worst ${lagMax} s`),
  chk("round trip on his own chat (he messages himself from his phone)", self > 0 ? true : null, `${self} seen`),
];
const want = [["kill9", 0], ["restart", 0], ["stall", 60], ["netdrop", 90], ["netdrop", 240]];
const arms = want.map(([test, secs]) => {
  const c = chaos.filter((x) => x.test === test && (secs === 0 || x.secs === secs)).at(-1);
  return { test, secs, c };
});
const back = (c) => (c ? (c.backAfterUndoSec ?? c.backSec) : null);
const chaosChecks = arms.map(({ test, secs, c }) => chk(`${test}${secs ? ` ${secs} s` : ""}: back within ${LIMITS.backSec} s${test === "netdrop" ? " of the network returning" : ""}`,
  c ? c.ok && back(c) <= LIMITS.backSec && c.blockVerified !== false : null,
  c ? `${back(c)} s${c.blockVerified ? " · block verified" : ""}${c.reconnects ? ` · ${c.reconnects} reconnect${c.reconnects > 1 ? "s" : ""}` : ""}` : "not run while linked"));
const all = [...soakChecks, ...chaosChecks];
const passed = all.filter((c) => c.pass).length;
const pending = all.filter((c) => c.note).length;
const failed = all.length - passed - pending;
const verdict = failed ? "FAIL" : pending ? "RUNNING" : "PASS";

// The graph: memory (RSS, heap) over time, live messages per hour as bars, the link state as a band, test markers.
function graph() {
  const W = 960, H = 300, L = 44, R = 12, top = 14, plotH = 170, barTop = 196, barH = 44, bandY = 252, bandH = 14;
  if (!rows.length) return `<svg viewBox="0 0 ${W} 60" width="100%"><text x="${L}" y="34" font-size="13" fill="#888">No recorder rows yet.</text></svg>`;
  const x = (t) => L + ((t - start) / Math.max(1, end - start)) * (W - L - R);
  const yMax = Math.max(100, Math.ceil(((rss.at(-1) ?? 0) * 1.25) / 50) * 50);
  const y = (v) => top + plotH - (v / yMax) * plotH;
  const line = (k) => samples.map((r, i) => `${i ? "L" : "M"}${x(r.ts).toFixed(1)},${y(r[k]).toFixed(1)}`).join("");
  const colors = { connected: "#22a06b", needs_qr: "#8590a2", pairing: "#8590a2", starting: "#8590a2", connecting: "#e2b203", disconnected: "#e56910", logged_out: "#c9372c", banned: "#c9372c", outdated: "#c9372c" };
  let band = "";
  for (let i = 0; i < rows.length; i++) {
    const a = rows[i].ts, b = i + 1 < rows.length ? Math.min(rows[i + 1].ts, a + LIMITS.gapSec) : Math.min(now, a + LIMITS.gapSec);
    band += `<rect x="${x(a).toFixed(1)}" y="${bandY}" width="${Math.max(0.6, x(b) - x(a)).toFixed(1)}" height="${bandH}" fill="${colors[rows[i].state] ?? "#8590a2"}"/>`;
  }
  for (const [a, b] of gaps) band += `<rect x="${x(a).toFixed(1)}" y="${bandY}" width="${Math.max(1, x(b) - x(a)).toFixed(1)}" height="${bandH}" fill="#c9372c"/>`;
  const perHour = new Map(); for (const r of samples) { const h = Math.floor(r.ts / 3600) * 3600; perHour.set(h, (perHour.get(h) ?? 0) + r.live); }
  const barMax = Math.max(1, ...perHour.values());
  const bars = [...perHour].map(([h, n]) => { const bh = (n / barMax) * barH; return `<rect x="${x(h).toFixed(1)}" y="${(barTop + barH - bh).toFixed(1)}" width="${Math.max(1, x(h + 3600) - x(h) - 1).toFixed(1)}" height="${bh.toFixed(1)}" fill="#579dff" opacity=".55"><title>${fmtT(h)}: ${n} messages</title></rect>`; }).join("");
  const marks = chaos.map((c) => `<line x1="${x(c.at).toFixed(1)}" x2="${x(c.at).toFixed(1)}" y1="${top}" y2="${bandY + bandH}" stroke="#8f7ee7" stroke-dasharray="3 3"/><text x="${(x(c.at) + 3).toFixed(1)}" y="${top + 10}" font-size="10" fill="#8f7ee7">${c.test}${c.secs ? " " + c.secs + "s" : ""}</text>`).join("");
  const restarts = starts.map((r) => `<path d="M${x(r.ts).toFixed(1)},${(bandY - 2).toFixed(1)}l-4,-7h8z" fill="${inChaos(r.ts) ? "#8f7ee7" : "#c9372c"}"><title>restart ${fmtT(r.ts)}</title></path>`).join("");
  const grid = [0, yMax / 2, yMax].map((v) => `<line x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}" stroke="#8590a2" stroke-opacity=".25"/><text x="${L - 6}" y="${y(v) + 4}" font-size="10" text-anchor="end" fill="#8590a2">${v} MB</text>`).join("");
  const ticks = []; for (let t = Math.ceil(start / 21600) * 21600; t <= end; t += 21600) ticks.push(`<text x="${x(t).toFixed(1)}" y="${H - 12}" font-size="10" text-anchor="middle" fill="#8590a2">${fmtT(t)}</text>`);
  return `<svg viewBox="0 0 ${W} ${H}" width="100%" role="img" aria-label="Gateway memory, messages and link state over the soak" style="display:block;margin:8px 0 4px">${grid}${bars}<path d="${line("heapMB")}" fill="none" stroke="#8590a2" stroke-width="1.2"/><path d="${line("rssMB")}" fill="none" stroke="#1d7afc" stroke-width="2"/>${marks}${restarts}${band}<text x="${L}" y="${barTop - 4}" font-size="10" fill="#8590a2">messages / hour (max ${barMax})</text><text x="${L}" y="${bandY - 12}" font-size="10" fill="#8590a2">link: <tspan fill="#22a06b">connected</tspan> · <tspan fill="#e2b203">connecting</tspan> · <tspan fill="#e56910">disconnected</tspan> · <tspan fill="#c9372c">down / logged out</tspan> · <tspan fill="#8590a2">unlinked</tspan></text>${ticks.join("")}<text x="${W - R}" y="${top + 10}" font-size="10" text-anchor="end"><tspan fill="#1d7afc">RSS</tspan> <tspan fill="#8590a2">heap</tspan></text></svg>`;
}

const sha = (() => { try { return execFileSync("git", ["-C", here, "rev-parse", "--short", "HEAD"], { encoding: "utf8" }).trim(); } catch { return "?"; } })();
const tone = verdict === "PASS" ? "ok" : verdict === "FAIL" ? "danger" : "warn";
const data = {
  meta: { title: "WhatsApp link · soak", description: "The WhatsApp gateway on the Mac mini, measured over a 24 h soak with crash and network-drop tests. Numbers only.", stream: "agents",
    crumb: [{ label: "Agent Base", href: "#" }, { label: "WhatsApp" }, { label: "soak" }],
    actions: [{ label: "siso-whatsapp-link", href: "https://github.com/sisodias/siso-whatsapp-link", icon: "git-branch", external: true }],
    source: "GET /soak on the gateway (soak.csv, one row a minute) · soak/results/chaos.jsonl" },
  heading: { breadcrumb: [{ label: "WhatsApp" }, { label: "soak" }],
    kicker: `soak <span class="sep">·</span> WA-3 <span class="sep">·</span> <span class="dim">Mac mini · gateway ${health.version} · ${fmtT(start)} → ${fmtT(end)} (UK time)</span>`,
    title: `Does the WhatsApp link stay up? ${verdict}`,
    subtitle: `${passed} of ${all.length} checks pass${pending ? `, ${pending} still to observe` : ""}${failed ? `, <b>${failed} failing</b>` : ""}. Now: <code>${health.link.state}</code>, ${health.rssMB} MB, ${health.counts.chats} chats, ${health.counts.messages} messages.`,
    badges: [{ label: verdict, tone, dot: true }, { label: "sending off", tone: "ok" }, { label: "read receipts off", tone: "ok" }] },
  sections: { prereg: { n: "01", title: "What we expected", sub: "limits set before the run, in soak/report.mjs" }, arms: { n: "02", title: "Break tests", sub: "soak/chaos.sh on the mini; each waits until the gateway is back where it started" },
    numbers: { n: "03", title: "Memory, messages and link state", sub: `${samples.length} one-minute samples, ${rows.length - samples.length} start/stop/state rows` },
    verdict: { n: "04", title: "Acceptance" }, audit: { n: "05", title: "How it was measured" }, changes: { n: "06", title: "What it changes" }, agent: { n: "07", title: "Agent entry" } },
  prereg: { facts: [
    { k: "setup", v: "whatsmeow companion device on Shaan's number, Mac mini (home line, always on), launchd KeepAlive, watchdog at 600 MB or 15 min down" },
    { k: "soak", v: `${LIMITS.hours} h linked; recorder covers ≥ ${pct(LIMITS.coverage)} of minutes; connected ≥ ${pct(LIMITS.connected)} outside the tests; no unplanned restart` },
    { k: "memory", v: `RSS under ${LIMITS.rssMaxMB} MB throughout; last hour within ${LIMITS.rssGrowthMB} MB of the first (no leak)` },
    { k: "breaks", v: `kill -9, clean restart, 60 s freeze, 90 s and 240 s network drop: each back within ${LIMITS.backSec} s` },
    { k: "round trip", v: "he sends a note to his own chat from his phone; it reaches the gateway (counted, never read). Nothing is sent from here." },
  ], hashes: [{ k: "siso-whatsapp-link", v: sha }, { k: "gateway", v: health.version }, { k: "limits", v: `<code>${JSON.stringify(LIMITS)}</code>` }] },
  arms: arms.map(({ test, secs, c }) => ({ id: `${test}${secs ? " " + secs + "s" : ""}`, tone: c ? (c.ok ? "ok" : "danger") : "warn",
    requested: { kill9: "kill -9 the process", restart: "launchctl kickstart -k", stall: `SIGSTOP ${secs} s, then SIGCONT`, netdrop: `pf drops WhatsApp's servers for ${secs} s` }[test],
    applied: c ? `back in ${back(c)} s` : "not run while linked", applied_note: c ? `detected after ${c.detectedSec ?? "—"} s${c.blockVerified ? "; block verified (web.whatsapp.com unreachable)" : ""}` : "",
    started: c ? fmtT(c.at) : "—", finished: c ? `${c.after}${c.pidChanged ? " · new process" : ""}` : "—", exit: c ? `${c.ok ? "ok" : "not back"} · reconnects ${c.reconnects}` : "" })),
  rows: [
    { label: "detected after (s)", values: arms.map(({ c }) => c?.detectedSec ?? "—"), ratio: "—" },
    { label: "back after the break (s)", values: arms.map(({ c }) => c?.backSec ?? "—"), ratio: `≤ ${LIMITS.backSec + 240}` },
    { label: "back after it was undone (s)", values: arms.map(({ c }) => c?.backAfterUndoSec ?? "—"), ratio: `≤ ${LIMITS.backSec}` },
    { label: "reconnects", values: arms.map(({ c }) => c?.reconnects ?? "—"), ratio: "—" },
  ],
  numbers_note: `${graph()}Recorded ${spanH.toFixed(1)} h (${linkedH.toFixed(1)} h linked) · memory ${rss[0] ?? 0}–${rss.at(-1) ?? 0} MB (median ${median(rss)}) · connected ${pct(connectedShare)} of calm minutes · ${reconnects} reconnects · ${starts.length} starts (${unplanned.length} unplanned) · ${live} live messages (${self} in his own chat), mean delay ${lagAvg.toFixed(1)} s, worst ${lagMax} s outside the tests. The last column is the limit, not a ratio.`,
  verdict: { kicker: "soak/report.mjs", value: verdict, tone, text: all.map((c) => `${c.pass ? "✅" : c.note ? "⏳" : "❌"} ${c.name}`).join("<br>"), claim: "“long-running tests, long-running integration” (Shaan, 3 Oct)" },
  acceptance: [{ id: "24 h soak", checks: soakChecks }, { id: "break tests", checks: chaosChecks }],
  audit: { facts: [
    { claim: "The gateway records itself: one row a minute, plus one per link state change, start and stop, to soak.csv (0600) on the mini. States and numbers only; a test checks no chat id, name or text reaches it.", receipt: "soak.go · soak_test.go" },
    { claim: "Uptime is read from gaps in that record: a gap over 150 s means the process or the mini was down.", receipt: "soak/report.mjs" },
    { claim: "Break tests time the gateway's own /health from the mini every 0.5 s; the network drop is a pf anchor (com.apple/siso-wa-soak) with a failsafe that lifts it whatever happens.", receipt: "soak/chaos.sh · results/chaos.jsonl" },
  ], limits: ["Message delay is measured against WhatsApp's own send time (1 s resolution, phone clock).", "Laptop-side reachability is not measured: the laptop sleeps and travels; the mini is the always-on side."],
    version: { label: `siso-whatsapp-link ${sha} · gateway ${health.version}`, href: "https://github.com/sisodias/siso-whatsapp-link" } },
  changes: [{ title: "WhatsApp space in Agent Base", href: "https://github.com/sisodias/siso-internal-labs-agent-base/pull/1", type: "PR", tone: "agents", why: "the soak is the gate before calling it live" },
    { title: "Decision memo WA-1", href: "http://127.0.0.1:8891/card/WHATSAPP-mura7ces/html", type: "U16 decision", tone: "research", why: "whatsmeow on the mini was the pick; this measures it" }],
  quickstart: { entry: { path: "SISO_Agency/apps/siso-contact/siso-whatsapp-link/soak/" }, owner: { name: "WHATSAPP (worker of AGENT-BASE)", last: fmtT(now) },
    verification: "Run the report again and compare the verdict list; rerun a break with soak/chaos.sh.",
    commands: ["node soak/report.mjs --from-link --post", "soak/chaos.sh all", "curl -H \"Authorization: Bearer $TOKEN\" http://100.66.34.21:5480/soak"] },
};

const out = path.resolve(arg("--out", path.join(here, "results/report.html")));
mkdirSync(path.dirname(out), { recursive: true });
const tmp = path.join(tmpdir(), `wa-soak-${process.pid}.json`);
writeFileSync(tmp, JSON.stringify(data));
execFileSync(path.join(homedir(), "SISO_Workspace/Great_Library_of_SISO/banks/siso-shell/bin/compose"), ["U19", tmp, "--base", "https://siso-shell.pages.dev", "--out", out], { stdio: "inherit" });
writeFileSync(out.replace(/\.html$/, ".json"), JSON.stringify({ verdict, passed, pending, failed, checks: all.map(({ name, pass, note }) => ({ name: name.replace(/<[^>]+>/g, ""), pass, note })), at: now }, null, 1));
if (flag("--post")) execFileSync(path.join(homedir(), ".claude/console/bin/console-post"), ["-a", "WHATSAPP", "-k", "html", "-i", "WHATSAPP-soak", "-t", `WhatsApp soak: ${verdict} (${passed}/${all.length})`, "-"], { input: readFileSync(out), stdio: ["pipe", "inherit", "inherit"] });
console.log(`${verdict}: ${passed}/${all.length} pass, ${pending} pending, ${failed} failing → ${out}`);
