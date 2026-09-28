// Films the approval scene of the demo: drives the agent's terminal (ttyd) and
// the console side by side in stage.html, writes the captions, and records the
// page. Started by record.sh, which brings up the mesh, the console and ttyd.
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
const OUT = process.env.OUT || path.join(here, 'out', 'approval.webm');
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
stage.searchParams.set('console', `${CONSOLE}/mesh/approvals`);
await page.goto(stage.href);

const frame = (host) => page.frames().find((f) => f.url().startsWith(host));
await waitFor(() => frame(TERM) && frame(CONSOLE), 'both frames');
const term = frame(TERM);
await term.waitForSelector('.xterm-helper-textarea');
await frame(CONSOLE).waitForSelector('h1');

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

// 0. The setting
await caption('Un agent CRM, <b>derrière mesh7</b>', 'Il lit librement. Il n\'écrit qu\'avec l\'accord d\'un humain.');
await focusPane('term');
await sleep(5200);

// 1. A read, then a write
await typeInTerm('python3 agent.py action');
await sleep(1200);
await caption('Une lecture : <b>autorisée</b>', 'La politique laisse passer les lectures, sans intervention.');
await sleep(5200);
await enter();
await waitFor(async () => (await pending()).length > 0, 'the approval');
await caption('Une écriture : <b>en attente</b>', 'L\'agent reçoit un identifiant d\'approbation. Rien n\'est exécuté.');
await sleep(5850);

// 2. The human decides, in the console
await focusPane('console');
const con = frame(CONSOLE);
await con.goto(`${CONSOLE}/mesh/approvals`);
await caption('Le valideur voit <b>l\'acte exact</b>', 'Agent, outil, paramètres, et ce que l\'agent a fait juste avant.');
const row = await con.waitForSelector('::-p-text(crm.update_customer)');
await sleep(1500);
await row.click();
const reason = await con.waitForSelector('input[placeholder^="Reasoning"]');
await sleep(4550);
await caption('Il approuve, <b>avec sa justification</b>', 'Qui, quand, pourquoi : la décision entre dans la trace.');
await reason.click();
await page.keyboard.type('Compte stratégique confirmé en comité', { delay: 45 });
await sleep(800);
const approve = await con.waitForSelector('::-p-text(Approve)');
await approve.click();
await waitFor(async () => (await pending()).length === 0, 'the approval to resolve');
await sleep(2600);

// 3. The agent retries
await enter();
await focusPane('term');
await caption('L\'agent relance : <b>exécuté, une fois</b>', 'La même requête, exactement celle qui a été approuvée.');
await sleep(5850);
await enter();
await waitFor(async () => (await pending()).length > 0, 'the second approval');
await caption('Une relance de plus : <b>nouvelle approbation</b>', 'Un accord vaut une exécution, pas un blanc-seing.');
await sleep(6500);

// 4. The proof, in three beats: intact, tampered, caught
await typeInTerm('clear && mesh7 trace verify state/traces.jsonl');
await caption('Chaque décision est <b>chaînée et signée</b>', 'La trace se vérifie de bout en bout : OK.');
await sleep(7000);
await typeInTerm('./run.sh tamper');
await caption('Quelqu\'un <b>réécrit une décision</b>', 'Dans une copie de la trace, l\'approbation humaine devient un simple « allow ».');
await sleep(7000);
await typeInTerm('mesh7 trace verify state/tampered.jsonl');
await caption('La falsification est <b>détectée, à la ligne près</b>', 'BROKEN : l\'empreinte de la ligne 2 ne correspond plus à son contenu.');
await sleep(8000);

// 5. Close
await caption('<b>mesh7</b>', 'Une règle, une trace, et la mémoire de qui a dit oui.');
await sleep(5200);

await rec.stop();
await browser.close();
console.log(OUT);
