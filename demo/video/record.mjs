// Films one scene of the demo (scenes/<name>.mjs): drives the agent's terminal
// (ttyd) and the console side by side in stage.html, writes the captions, and
// records the page. Started by record.sh, which brings up the mesh, the
// console and ttyd.
//
// A terminal drawn by xterm.js is a canvas: its text cannot be read back, so
// the script waits on the mesh's own state (pending approvals) where it
// matters and on fixed beats elsewhere.
import puppeteer from 'puppeteer-core';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const MESH = process.env.MESH || 'http://localhost:9191';
const TERM = process.env.TERM_URL || 'http://127.0.0.1:7691';
const CONSOLE = process.env.CONSOLE_URL || 'http://localhost:3118';
const SCENE = process.argv[2] || 'approval';
const scene = await import(`./scenes/${SCENE}.mjs`);
const OUT = process.env.OUT || path.join(here, 'out', `${SCENE}.webm`);
const CHROME = process.env.CHROME;
const W = 1600, H = 900;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function pending() {
  const r = await fetch(`${MESH}/approvals?status=pending`);
  const body = await r.json();
  return Array.isArray(body) ? body : (body.approvals ?? []);
}

async function waitFor(check, what, timeout = 15000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (await check()) return;
    await sleep(250);
  }
  throw new Error(`timed out waiting for ${what}`);
}

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: ['--no-sandbox', `--window-size=${W},${H}`],
  defaultViewport: { width: W, height: H, deviceScaleFactor: 1 },
});
const page = await browser.newPage();
const stage = new URL('file://' + path.join(here, 'stage.html'));
stage.searchParams.set('term', TERM);
stage.searchParams.set('console', CONSOLE + scene.start);
await page.goto(stage.href);

const frame = (host) => page.frames().find((f) => f.url().startsWith(host));
await waitFor(() => frame(TERM) && frame(CONSOLE), 'both frames');
const term = frame(TERM);
await term.waitForSelector('.xterm-helper-textarea');
// The console, with its sidebar folded: the pane is narrower than the screen
// the console is laid out for, and every row should fit.
async function openConsole(pathname) {
  const con = frame(CONSOLE);
  if (pathname) await con.goto(CONSOLE + pathname);
  await con.waitForSelector('h1');
  await con.$eval('aside button', (b) => b.click());
  return con;
}
await openConsole();

const caption = (t, s = '') => page.evaluate((t, s) => window.caption(t, s), t, s);
const focusPane = (id) => page.evaluate((id) => window.focusPane(id), id);
async function typeInTerm(cmd) {
  await focusPane('term');
  await term.focus('.xterm-helper-textarea');
  await page.keyboard.type(cmd, { delay: 55 });
  await sleep(300);
  await page.keyboard.press('Enter');
}
async function enter() {
  await term.focus('.xterm-helper-textarea');
  await page.keyboard.press('Enter');
}

await sleep(1500);
const rec = await page.screencast({ path: OUT });

await scene.default({ caption, focusPane, typeInTerm, enter, sleep, waitFor, pending, frame, openConsole, CONSOLE, page });

await rec.stop();
await browser.close();
console.log(OUT);
