// The approval scene: a read goes through, a write waits for a human who
// approves it in the console, the retry runs once, and the trace proves it.
export const start = '/mesh/approvals';

export default async function ({ caption, focusPane, typeInTerm, enter, sleep, waitFor, pending, frame, openConsole, CONSOLE, page }) {
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
  const con = await openConsole('/mesh/approvals');
  await caption('Le valideur voit <b>l\'acte exact</b>', 'Agent, outil, paramètres, et ce que l\'agent a fait juste avant.');
  const row = await con.waitForSelector('::-p-text(crm.update_customer)');
  await sleep(1500);
  await row.evaluate((e) => e.click()); // the console frame is scaled: no mouse coordinates
  const reason = await con.waitForSelector('input[placeholder^="Reasoning"]');
  await sleep(4550);
  await caption('Il approuve, <b>avec sa justification</b>', 'Qui, quand, pourquoi : la décision entre dans la trace.');
  await reason.evaluate((e) => e.focus());
  await page.keyboard.type('Compte stratégique confirmé en comité', { delay: 45 });
  await sleep(800);
  const approve = await con.waitForSelector('::-p-text(Approve)');
  await approve.evaluate((e) => e.click());
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
}
