// Browser test of the web UI (Playwright + Chromium):
//   setup URL → admin → create workspace → type in the browser terminal →
//   file appears on the server seat and a checkpoint is listed.
//
//   node test/e2e/ui.mjs <setup-url> <screenshot-dir>
// The server must be running; the setup URL is the one it printed.
import { createRequire } from "node:module";
import { mkdirSync } from "node:fs";
// CommonJS require honours NODE_PATH, so a globally installed playwright works.
const { chromium } = createRequire(import.meta.url)("playwright");

const [setupURL, shots = "ui-shots"] = process.argv.slice(2);
if (!setupURL) { console.error("usage: node ui.mjs <setup-url> [screenshot-dir]"); process.exit(2); }
mkdirSync(shots, { recursive: true });
const base = new URL(setupURL).origin;
const fail = (m) => { console.error("FAIL", m); process.exit(1); };
const pass = (m) => console.log("PASS", m);

const browser = await chromium.launch(process.env.CHROMIUM ? { executablePath: process.env.CHROMIUM } : {});
const page = await browser.newPage({ viewport: { width: 1280, height: 860 } });
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });

// 1. setup
await page.goto(setupURL);
await page.fill('input[name="username"]', "abdul");
await page.fill('input[name="password"]', "correct-horse-9");
await page.screenshot({ path: `${shots}/1-setup.png` });
await page.click('button[type="submit"]');
await page.waitForSelector("text=Workspaces");
pass("admin created through the setup page");

// 2. create workspace
await page.fill('input[placeholder^="name"]', "web-demo");
await page.click("text=Create workspace");
await page.waitForSelector("#term", { timeout: 30000 });
pass("workspace created and opened");

// 3. use the terminal
await page.waitForTimeout(800);
await page.click("#term");
await page.keyboard.type("echo hello-from-the-browser > browser.txt && git status --short && whoami\n");
await page.waitForFunction(() => document.querySelector("#term")?.innerText.includes("browser.txt"), null, { timeout: 15000 })
  .catch(() => fail("terminal did not show git status output"));
pass("terminal runs commands (git status shows the new file)");
const text = await page.locator("#term").innerText();
if (!/ws-[a-z0-9]+/.test(text)) fail("terminal is not running as the workspace user:\n" + text);
pass("terminal runs as the isolated workspace user (" + text.match(/ws-[a-z0-9]+/)[0] + ")");

// 4. checkpoint appears in the UI
await page.waitForFunction(() => document.querySelectorAll("tbody tr").length >= 2, null, { timeout: 20000 })
  .catch(() => fail("no new checkpoint listed"));
pass("new checkpoint listed in the UI");
await page.screenshot({ path: `${shots}/2-workspace.png` });

// 5. devices + users pages render
await page.click("text=Devices");
await page.waitForSelector("text=No paired devices");
await page.click("text=Users");
await page.click("text=Create invite link");
await page.waitForSelector("text=/invite\\?token=/");
await page.screenshot({ path: `${shots}/3-invite.png` });
pass("devices and users pages work (invite link created)");

// 6. logged-out access is refused
const anon = await browser.newPage();
const r = await anon.request.get(`${base}/api/workspaces`);
if (r.status() !== 401) fail("anonymous API access returned " + r.status());
pass("anonymous API access refused");

if (errors.length) fail("browser errors: " + errors.join(" | "));
pass("no JavaScript errors");
await browser.close();
console.log("UI CHECKS PASSED");
