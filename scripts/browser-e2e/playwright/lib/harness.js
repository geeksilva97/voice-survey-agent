// Shared helpers for the persona QA suite.
//
// The heavy lifting — the fake microphone and the on-demand persona answers —
// lives in scripts/browser-e2e/persona-answerer.js, which this suite injects
// VERBATIM (the exact same file the Chrome DevTools MCP flow uses). Here we only:
//   - create a fresh poll via the API,
//   - inject the persona harness and drive the page to its end state,
//   - scrape what happened (transcript, spoken turns, and the real classifier
//     intents mirrored back over the -qa "qa_intent" channel),
//   - read authoritative ground truth (end_reason + slot statuses) from the API.

const path = require('path');
const { expect } = require('@playwright/test');

// QA always uses the same product/purpose so runs are comparable (see the
// project constraints). The persona — not the product — is what varies.
const PRODUCT = 'hand-poured scented soy candles for the home';
const PURPOSE = 'see how folks feel about scent and burn time';

// The fake-mic + persona-answerer harness, shared with the Chrome MCP flow.
const PERSONA_SCRIPT = path.resolve(__dirname, '..', '..', 'persona-answerer.js');

// createPoll asks the server to generate a fresh survey and returns its id.
async function createPoll(request) {
  const res = await request.post('/api/polls', { data: { product: PRODUCT, purpose: PURPOSE } });
  expect(res.ok(), `create poll failed: HTTP ${res.status()}`).toBeTruthy();
  const body = await res.json();
  expect(body.id, 'poll id missing from create response').toBeTruthy();
  return body.id;
}

// runPersona injects the persona harness, starts the session, and waits for the
// page to reach its end screen. Returns everything a test needs to assert on.
async function runPersona(page, id, personaId) {
  // Order matters: set the persona BEFORE the harness reads window.__persona.
  // Both run before the page's own scripts (addInitScript runs in insertion order).
  await page.addInitScript((p) => { window.__persona = p; }, personaId);
  await page.addInitScript({ path: PERSONA_SCRIPT });

  await page.goto(`/poll/${id}`);
  // The Start click is the user gesture that unlocks the AudioContext; the server
  // then streams the greeting immediately and the harness takes over from there.
  await page.click('#start');

  // The client removes .hidden from #ended once the session finishes (any reason).
  await page.waitForSelector('#ended:not(.hidden)', { timeout: 220_000 });

  const transcript = await page.$$eval('#transcript .t-row', (rows) =>
    rows.map((row) => {
      const who = row.querySelector('.t-who');
      const label = who ? who.textContent : '';
      const text = row.textContent.slice(label.length).trim();
      return { who: label, text };
    }));

  const spoken = await page.evaluate(() => (window.__qa && window.__qa.turns) || []);
  const intents = await page.evaluate(() => window.__qaIntents || []);

  return { transcript, spoken, intents };
}

// groundTruth reads the poll back from the API and waits until the run has ended
// (end_reason is set). The server saves the poll before sending `done`, so by the
// time #ended is visible this is already terminal; the small retry loop just
// absorbs any last-write timing jitter.
async function groundTruth(request, id, { tries = 20, delayMs = 400 } = {}) {
  for (let i = 0; i < tries; i++) {
    const res = await request.get(`/api/polls/${id}`);
    if (res.ok()) {
      const poll = await res.json();
      if (poll.end_reason) return poll; // "" (NotEnded) is omitted by the server
    }
    await new Promise((r) => setTimeout(r, delayMs));
  }
  throw new Error(`poll ${id} never reached a terminal end_reason`);
}

// slots returns the survey question slots (text/status/answer), or [].
function slots(poll) {
  return (poll.survey && poll.survey.questions) || [];
}

// surveyIntents returns just the intents classified during the survey phase
// (excludes greeting/consent), which is what most behavioral assertions care about.
function surveyIntents(intents) {
  return intents.filter((i) => i.phase === 'survey');
}

// intentFired reports whether a given intent was classified in any phase.
function intentFired(intents, name) {
  return intents.some((i) => i.intent === name);
}

module.exports = {
  PRODUCT, PURPOSE, PERSONA_SCRIPT,
  createPoll, runPersona, groundTruth, slots, surveyIntents, intentFired,
};
