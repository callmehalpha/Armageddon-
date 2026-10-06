// Browser test of the Phase 5 UI (Playwright + Chromium): "Open IDE" opens
// code-server through the proxy in a new tab, and the Git credentials page
// adds and removes a credential without ever showing the secret again.
//
//   node test/e2e/ide-ui.mjs <setup-url> [screenshot-dir]
// The server must be fresh (the setup URL unused) and configured with a
// code-server (the fake one is fine; set REAL_CODE_SERVER=1 for the real).
import { createRequire } from "node:module";
import { mkdirSync } from "node:fs";
import { randomBytes } from "node:crypto";
const { chromium } = createRequire(import.meta.url)("playwright");

const [setupURL, shots = "ide-ui-shots"] = process.argv.slice(2);
if (!setupURL) { console.error("usage: node ide-ui.mjs <setup-url> [screenshot-dir]"); process.exit(2); }
mkdirSync(shots, { recursive: true });
const fail = (m) => { console.error("FAIL", m); process.exit(1); };
const pass = (m) => console.log("PASS", m);

const browser = await chromium.launch(process.env.CHROMIUM ? { executablePath: process.env.CHROMIUM } : {});
const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
const page = await ctx.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));

await page.goto(setupURL);
await page.fill('input[name="username"]', "abdul");
await page.fill('input[name="password"]', randomBytes(18).toString("base64url"));
await page.click('button[type="submit"]');
await page.waitForSelector("text=Workspaces");
await page.fill('input[placeholder^="name"]', "ide-demo");
await page.click("text=Create workspace");
await page.waitForSelector("#open-ide", { timeout: 30000 });
pass("workspace page shows the Open IDE button");

const [ide] = await Promise.all([ctx.waitForEvent("page"), page.click("#open-ide")]);
await ide.waitForLoadState("domcontentloaded");
if (process.env.REAL_CODE_SERVER) {
  await ide.waitForSelector(".monaco-workbench", { timeout: 60000 }).catch(() => fail("code-server workbench did not load"));
  pass("real code-server workbench loaded through the proxy");
} else {
  const body = await ide.textContent("body");
  if (!/fake code-server folder=.*\/tree/.test(body)) fail("IDE tab shows: " + body);
  pass("IDE tab served by code-server through the proxy, opened on the worktree");
}
await ide.screenshot({ path: `${shots}/1-ide.png` });
await ide.close();

// An anonymous browser cannot reach the same URL.
const anon = await browser.newContext();
const r = await anon.request.get(new URL(await page.getAttribute("#open-ide", "href"), setupURL).href);
if (r.status() !== 401) fail("anonymous IDE access returned " + r.status());
pass("anonymous IDE access refused (401)");

const secret = "ghp_" + randomBytes(16).toString("hex");
await page.click("text=Git credentials");
await page.waitForSelector("text=No stored credentials.");
await page.fill('input[placeholder^="provider host"]', "github.com");
await page.fill('input[placeholder="personal access token"]', secret);
await page.click('button:has-text("Add")');
await page.waitForSelector("td:has-text('github.com')");
const html = await page.content();
if (html.includes(secret)) fail("the token is shown after saving");
pass("PAT added; the page never shows it again");
await page.selectOption("select", "ssh-key");
await page.fill('input[placeholder^="provider host"]', "gitlab.com");
await page.click('button:has-text("Add")');
await page.waitForSelector("text=/ssh-ed25519 AAAA/");
pass("SSH key generated; public key shown");
await page.screenshot({ path: `${shots}/2-credentials.png` });
page.once("dialog", (d) => d.accept());
await page.locator("tr", { hasText: "github.com" }).locator("button").click();
await page.waitForFunction(() => !document.body.innerText.includes("github.com"));
pass("credential removed");

if (errors.length) fail("browser errors: " + errors.join(" | "));
pass("no JavaScript errors");
await browser.close();
console.log("IDE UI CHECKS PASSED");
