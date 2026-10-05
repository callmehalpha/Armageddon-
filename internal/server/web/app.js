// Armageddon web UI (MVP). Plain JS; all user data is inserted with
// textContent, never innerHTML.
"use strict";

let me = null, csrf = "";

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v);
  }
  for (const k of kids.flat()) if (k != null) el.append(k instanceof Node ? k : String(k));
  return el;
}

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) { opt.headers["Content-Type"] = "application/json"; opt.body = JSON.stringify(body); }
  if (method !== "GET") opt.headers["X-CSRF-Token"] = csrf;
  const r = await fetch(path, opt);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

function go(path) { history.pushState({}, "", path); route(); }
window.addEventListener("popstate", route);
document.addEventListener("click", (e) => {
  const a = e.target.closest("a[data-nav]");
  if (a) { e.preventDefault(); go(a.getAttribute("href")); }
});

function render(...nodes) { const app = document.getElementById("app"); app.replaceChildren(...nodes); }
function when(ts) { return new Date(ts).toLocaleString(); }
function ago(ts) {
  const s = Math.round((Date.now() - ts) / 1000);
  if (s < 60) return s + "s ago"; if (s < 3600) return Math.round(s / 60) + "m ago";
  if (s < 86400) return Math.round(s / 3600) + "h ago"; return Math.round(s / 86400) + "d ago";
}

function nav() {
  const n = document.getElementById("nav");
  if (!me) { n.replaceChildren(); return; }
  n.replaceChildren(
    h("a", { href: "/", "data-nav": "" }, "Workspaces"),
    h("a", { href: "/devices", "data-nav": "" }, "Devices"),
    me.role === "admin" ? h("a", { href: "/admin", "data-nav": "" }, "Users") : null,
    h("span", { class: "muted", style: "margin-left:14px" }, me.username),
    h("button", { class: "ghost", onclick: async () => { await api("POST", "/api/logout"); me = null; go("/"); } }, "Log out"));
}

let cleanup = null;
async function route() {
  if (cleanup) { cleanup(); cleanup = null; }
  const path = location.pathname, q = new URLSearchParams(location.search);
  try {
    const r = await fetch("/api/me");
    if (r.ok) { const d = await r.json(); me = d.user; csrf = d.csrf || ""; } else me = null;
  } catch { me = null; }
  nav();
  if (path === "/setup") return setupPage(q.get("token") || "");
  if (path === "/invite") return invitePage(q.get("token") || "");
  if (!me) {
    const s = await api("GET", "/api/setup").catch(() => ({}));
    return s.needs_setup ? render(h("div", { class: "card" }, h("h1", {}, "Welcome"),
      h("p", {}, "This server has no users yet. Open the setup link printed in the server log to create the first admin."))) : loginPage();
  }
  if (path === "/pair") return pairPage(q.get("code") || "");
  if (path === "/devices") return devicesPage();
  if (path === "/admin") return adminPage();
  const m = path.match(/^\/w\/([A-Z0-9]+)$/);
  if (m) return workspacePage(m[1]);
  return homePage();
}

function formPage(title, intro, fields, submitLabel, onSubmit) {
  const err = h("div", { class: "err" });
  const inputs = fields.map(([name, type, ph]) => h("input", { name, type, placeholder: ph, autocomplete: type === "password" ? "new-password" : "username", required: "" }));
  const form = h("form", { class: "stack", onsubmit: async (e) => {
    e.preventDefault(); err.textContent = "";
    const v = Object.fromEntries(inputs.map(i => [i.name, i.value]));
    try { await onSubmit(v); } catch (x) { err.textContent = x.message; }
  } }, ...inputs, h("button", { type: "submit" }, submitLabel), err);
  render(h("div", { class: "card" }, h("h1", {}, title), intro ? h("p", { class: "muted" }, intro) : null, form));
}

function loginPage() {
  formPage("Log in", null, [["username", "text", "username"], ["password", "password", "password"]], "Log in", async (v) => {
    const d = await api("POST", "/api/login", v); me = d.user; csrf = d.csrf; go(location.pathname + location.search);
  });
}

function setupPage(token) {
  formPage("Create the first admin", "This one-time link expires an hour after the server printed it.",
    [["username", "text", "username"], ["password", "password", "password (8+ characters)"]], "Create admin", async (v) => {
      const d = await api("POST", "/api/setup", { token, ...v }); me = d.user; csrf = d.csrf; go("/");
    });
}

function invitePage(token) {
  formPage("Join this Armageddon server", "Choose your username and password.",
    [["username", "text", "username"], ["password", "password", "password (8+ characters)"]], "Create account", async (v) => {
      const d = await api("POST", "/api/invites/accept", { token, ...v }); me = d.user; csrf = d.csrf; go("/");
    });
}

async function homePage() {
  const list = h("div");
  const err = h("div", { class: "err" });
  const name = h("input", { placeholder: "name, e.g. dokta", required: "" });
  const url = h("input", { placeholder: "optional: clone from https://github.com/…", style: "flex:1; min-width:260px" });
  const create = h("form", { class: "row", onsubmit: async (e) => {
    e.preventDefault(); err.textContent = "";
    try { const w = await api("POST", "/api/workspaces", { name: name.value, source_url: url.value }); go("/w/" + w.id); }
    catch (x) { err.textContent = x.message; }
  } }, name, url, h("button", { type: "submit" }, "Create workspace"));
  render(h("h1", {}, "Workspaces"), h("div", { class: "card" }, create, err), list);
  const ws = await api("GET", "/api/workspaces");
  if (!ws.length) list.append(h("p", { class: "muted" }, "No workspaces yet."));
  for (const w of ws) {
    list.append(h("div", { class: "card row", style: "justify-content:space-between" },
      h("div", {}, h("a", { href: "/w/" + w.id, "data-nav": "" }, h("strong", {}, w.name)), " ",
        h("span", { class: "pill " + w.state }, w.state), " ",
        h("span", { class: "muted" }, w.source_url || "empty")),
      h("span", { class: "muted" }, w.checkpoint_seq ? "checkpoint #" + w.checkpoint_seq : "")));
  }
}

async function workspacePage(id) {
  let w;
  try { w = await api("GET", "/api/workspaces/" + id); } catch (x) { return render(h("p", { class: "err" }, x.message)); }
  if (w.state !== "ready") {
    render(h("h1", {}, w.name), h("div", { class: "card" }, h("span", { class: "pill " + w.state }, w.state), " ",
      w.state_reason ? h("span", { class: "err" }, w.state_reason) : h("span", { class: "muted" }, "Preparing the workspace…")));
    if (w.state === "creating" || w.state === "importing") { const t = setTimeout(route, 1500); cleanup = () => clearTimeout(t); }
    return;
  }
  const holder = w.lease.holder_kind === "server" ? "the server seat (this browser)" : "device " + w.lease.holder_device;
  const termBox = h("div", { id: "term" });
  const cps = h("tbody"), evs = h("tbody");
  const cloneCmd = "armageddon clone " + w.id;
  render(
    h("h1", {}, w.name, " ", h("span", { class: "pill ready" }, "ready")),
    h("div", { class: "banner" }, "Workspace is owned by ", h("strong", {}, holder), ` · epoch ${w.lease.epoch} · `,
      h("span", { class: "muted" }, w.source_url || "empty workspace")),
    termBox,
    h("div", { class: "grid", style: "margin-top:14px" },
      h("div", { class: "card" }, h("h2", {}, "Checkpoints"),
        h("p", { class: "muted" }, "Uncommitted work is captured automatically a few seconds after it changes."),
        h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "#"), h("th", {}, "HEAD"), h("th", {}, "when"))), cps)),
      h("div", { class: "card" }, h("h2", {}, "Replicate to a laptop"),
        h("p", {}, "Install the CLI, then:"),
        h("pre", { class: "cmd" }, `armageddon login ${location.origin}\n${cloneCmd}\ncd ${w.slug} && armageddon follow`),
        h("p", { class: "muted" }, "The replica follows this workspace and survives on its own if this server is lost."),
        h("h2", {}, "Git remote"), h("pre", { class: "cmd" }, w.git_url))),
    h("div", { class: "card" }, h("h2", {}, "Activity"), h("table", {}, evs)));

  const term = new Terminal({ cursorBlink: true, fontFamily: "ui-monospace, Menlo, monospace", fontSize: 13, convertEol: false });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit); term.open(termBox); fit.fit();
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/api/workspaces/${id}/terminal`);
  ws.binaryType = "arraybuffer";
  const send = (m) => ws.readyState === 1 && ws.send(JSON.stringify(m));
  ws.onopen = () => { send({ t: "r", c: term.cols, r: term.rows }); term.focus(); };
  ws.onmessage = (e) => term.write(new Uint8Array(e.data));
  ws.onclose = (e) => term.write(`\r\n\x1b[33m[terminal closed${e.reason ? ": " + e.reason : ""}]\x1b[0m\r\n`);
  term.onData((d) => send({ t: "i", d }));
  const onResize = () => { fit.fit(); send({ t: "r", c: term.cols, r: term.rows }); };
  window.addEventListener("resize", onResize);

  async function refresh() {
    const [c, e] = await Promise.all([api("GET", `/api/workspaces/${id}/checkpoints`), api("GET", `/api/workspaces/${id}/events`)]).catch(() => [[], []]);
    cps.replaceChildren(...c.slice(0, 12).map(x => h("tr", {}, h("td", {}, x.seq),
      h("td", { class: "mono" }, (x.head_ref || "").replace("refs/heads/", "") + " " + (x.head_oid || "").slice(0, 8)),
      h("td", { class: "muted", title: when(x.created_at) }, ago(x.created_at)))));
    evs.replaceChildren(...(e || []).slice(0, 15).map(x => h("tr", {}, h("td", { class: "muted", title: when(x.TS) }, ago(x.TS)),
      h("td", {}, x.Type), h("td", { class: "mono muted" }, JSON.stringify(x.Payload)))));
  }
  refresh();
  const timer = setInterval(refresh, 4000);
  cleanup = () => { clearInterval(timer); window.removeEventListener("resize", onResize); ws.close(); term.dispose(); };
}

async function pairPage(code) {
  const err = h("div", { class: "err" });
  let info;
  try { info = await api("GET", "/api/pair/lookup?code=" + encodeURIComponent(code)); }
  catch (x) { return render(h("div", { class: "card" }, h("h1", {}, "Pair a device"), h("p", { class: "err" }, x.message))); }
  render(h("div", { class: "card" }, h("h1", {}, "Pair a device"),
    h("p", {}, "A device is asking to connect to your account:"),
    h("table", {}, h("tr", {}, h("th", {}, "Name"), h("td", {}, info.name)), h("tr", {}, h("th", {}, "Platform"), h("td", {}, info.platform)),
      h("tr", {}, h("th", {}, "Code"), h("td", { class: "mono" }, code)), h("tr", {}, h("th", {}, "Key fingerprint"), h("td", { class: "mono" }, info.fingerprint))),
    h("p", { class: "muted" }, "Only approve if this matches what your terminal shows."),
    h("button", { onclick: async (e) => {
      e.target.disabled = true;
      try { await api("POST", "/api/pair/approve", { code }); render(h("div", { class: "card" }, h("h1", { class: "ok" }, "Device approved"), h("p", {}, "You can close this tab; the CLI will finish logging in."))); }
      catch (x) { err.textContent = x.message; e.target.disabled = false; }
    } }, "Approve " + info.name), err));
}

async function devicesPage() {
  const body = h("tbody");
  render(h("h1", {}, "Devices"), h("div", { class: "card" }, h("table", {},
    h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Platform"), h("th", {}, "Fingerprint"), h("th", {}, "Last seen"), h("th", {}))), body)));
  const ds = await api("GET", "/api/devices");
  if (!ds.length) body.append(h("tr", {}, h("td", { colspan: "5", class: "muted" }, "No paired devices. Run `armageddon login` on a laptop.")));
  for (const d of ds) body.append(h("tr", {}, h("td", {}, d.name), h("td", {}, d.platform), h("td", { class: "mono" }, d.fingerprint),
    h("td", { class: "muted" }, ago(d.last_seen_at)),
    h("td", {}, d.revoked ? h("span", { class: "pill failed" }, "revoked") : h("button", { class: "ghost", onclick: async () => {
      if (confirm("Revoke " + d.name + "? It loses access immediately.")) { await api("DELETE", "/api/devices/" + d.id); devicesPage(); }
    } }, "Revoke"))));
}

async function adminPage() {
  const out = h("div");
  render(h("h1", {}, "Users"), h("div", { class: "card" },
    h("button", { onclick: async () => {
      const d = await api("POST", "/api/invites");
      out.replaceChildren(h("p", {}, "Send this one-time link (valid " + d.expires_in + "):"), h("pre", { class: "cmd" }, d.url));
    } }, "Create invite link"), out), h("div", { class: "card" }, h("table", { id: "users" })));
  const us = await api("GET", "/api/users");
  document.getElementById("users").append(...us.map(u => h("tr", {}, h("td", {}, u.username), h("td", { class: "muted" }, u.role))));
}

route();
