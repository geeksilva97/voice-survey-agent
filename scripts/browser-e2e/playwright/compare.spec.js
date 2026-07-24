// A/B comparison runner: the same personas through the classifier path and the
// EXPERIMENTAL agent-loop path, on the real voice page with a fake mic.
//
// This is an experiment harness, not a regression gate — it ASSERTS almost
// nothing and instead records what happened so the two paths can be read side by
// side. The pass/fail suite is personas.spec.js.
//
// Run against a server you started yourself (so you choose the driver):
//   QA_REUSE_SERVER=1 QA_PORT=8091 QA_LABEL=classifier \
//   QA_OUT=/tmp/ab-classifier.json npx playwright test compare.spec.js --retries=0
//
// Env:
//   QA_LABEL     label written into the output ("classifier" | "agent")
//   QA_OUT       absolute path for the JSON result file
//   QA_PERSONAS  comma-separated persona ids (default: all four)

const fs = require('fs');
const { test } = require('@playwright/test');
const H = require('./lib/harness');

const LABEL = process.env.QA_LABEL || 'unlabelled';
const OUT = process.env.QA_OUT || `/tmp/ab-${LABEL}.json`;
const PERSONAS = (process.env.QA_PERSONAS || 'enthusiast,neutral,rusher,confused').split(',');

const results = { label: LABEL, startedAt: new Date().toISOString(), runs: [] };

test.describe.configure({ mode: 'serial' });

for (const persona of PERSONAS) {
  test(`[${LABEL}] ${persona}`, async ({ page, request }) => {
    const started = Date.now();
    const id = await H.createPoll(request);
    let error = null;
    let run = { transcript: [], spoken: [], intents: [] };
    try {
      run = await H.runPersona(page, id, persona);
    } catch (e) {
      error = String(e && e.message ? e.message : e);
    }
    const wallMs = Date.now() - started;

    let poll = {};
    try {
      poll = await H.groundTruth(request, id);
    } catch (e) {
      error = error || String(e);
    }

    const slots = H.slots(poll).map((q) => ({ status: q.status, answer: q.answer || '' }));
    results.runs.push({
      persona,
      pollId: id,
      wallMs,
      error,
      endReason: poll.end_reason || null,
      slots,
      answered: slots.filter((s) => s.status === 'answered').length,
      skipped: slots.filter((s) => s.status === 'skipped').length,
      open: slots.filter((s) => s.status !== 'answered' && s.status !== 'skipped').length,
      // Signal names: classifier intents on one path, tool names on the other.
      signals: run.intents.map((i) => i.intent).filter(Boolean),
      spoken: run.spoken,
      transcript: run.transcript,
    });
    fs.writeFileSync(OUT, JSON.stringify(results, null, 2));
    console.log(`[${LABEL}] ${persona}: end=${poll.end_reason} answered=${slots.filter((s) => s.status === 'answered').length} wall=${(wallMs / 1000).toFixed(1)}s${error ? ' ERROR=' + error : ''}`);
  });
}
