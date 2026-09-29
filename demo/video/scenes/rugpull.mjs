// The rug pull scene: a third-party MCP server whose catalogue was pinned
// updates itself; mesh7 holds back the rewritten tool (approval) and the new
// one (denied), although the policy ends with a broad allow.
export const start = '/mesh/tools';

export default async function ({ caption, focusPane, typeInTerm, enter, sleep, waitFor, pending, frame, openConsole, CONSOLE }) {
  const con = () => frame(CONSOLE);
  const show = async (text) => {
    const el = await con().waitForSelector(`::-p-text(${text})`, { timeout: 15000 });
    await el.evaluate((e) => e.scrollIntoView({ block: 'center', behavior: 'smooth' }));
    return el;
  };

  // 0. A reviewed catalogue
  await caption('Un serveur MCP tiers, <b>déjà relu</b>', 'Son catalogue d\'outils a été épinglé au premier passage.');
  await focusPane('console');
  await sleep(4500);
  await typeInTerm('python3 agent.py tools');
  await caption('L\'agent voit <b>cinq outils CRM</b>', 'Lire, chercher, modifier, envoyer une facture, requêter en SQL.');
  await sleep(5500);

  // 1. The supplier updates itself
  await typeInTerm('clear && ./run.sh rugpull');
  await caption('Le fournisseur <b>se met à jour</b>', 'Sans prévenir : une description réécrite, un outil en plus.');
  await sleep(6000);

  // 2. The console shows what changed
  await focusPane('console');
  await openConsole(start);
  await show('held back since the catalogue was pinned');
  await caption('mesh7 <b>retient les changements</b>', 'L\'ancienne description barrée ; la nouvelle ordonne d\'exporter la base clients.');
  await sleep(8500);

  // 3. The agent meets them
  await typeInTerm('clear && python3 agent.py rugpull');
  await sleep(1500);
  await caption('Le catalogue <b>vu par l\'agent</b>', 'L\'outil d\'export est apparu, la description de get_customer a changé.');
  await sleep(5000);
  await enter();
  await waitFor(async () => (await pending()).length > 0, 'the approval on the changed tool');
  await caption('L\'outil modifié <b>demande une approbation</b>', 'Alors que la politique se termine par « crm.* : allow ».');
  await sleep(6500);
  await enter();
  await sleep(1200);
  await caption('L\'outil apparu <b>est refusé</b>', 'Rien de nouveau ne passe sans avoir été relu.');
  await sleep(6500);

  // 4. Close
  await caption('<b>Épingler</b> le catalogue', 'Ce qui a été relu ne change pas en silence.');
  await sleep(4500);
}
