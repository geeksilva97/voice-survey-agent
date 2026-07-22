// Persona QA — headless, one command, assertion-backed.
//
// Each test drives the real voice page end-to-end with a simulated respondent
// (LLM-in-character answers, synthesized in a distinct voice, played into a fake
// mic). We assert on OUTCOMES, not exact words — because the answers are generated
// fresh each run, the transcript strings vary but the outcome shouldn't:
//   - end_reason (from the API — the authoritative, deterministic result),
//   - slot statuses (answered / skipped),
//   - the real classifier intents that fired (mirrored over the -qa channel).
//
// The expectations below mirror `Expect` in internal/qa/personas.go — that Go
// file is the source of truth for what each persona is supposed to do; keep the
// two in sync.

const { test, expect } = require('@playwright/test');
const H = require('./lib/harness');

const STATUS_TERMINAL = new Set(['answered', 'skipped']);

test.describe('voice-survey persona QA', () => {
  test('enthusiast completes the whole survey', async ({ page, request }) => {
    const id = await H.createPoll(request);
    const run = await H.runPersona(page, id, 'enthusiast');
    const poll = await H.groundTruth(request, id);
    await attach(test.info(), poll, run);

    expect(poll.end_reason, 'enthusiast should complete').toBe('completed');
    const s = H.slots(poll);
    expect(s.length, 'survey should have questions').toBeGreaterThan(0);
    expect(s.every((q) => q.status === 'answered'),
      `every slot answered — got ${statuses(s)}`).toBeTruthy();
  });

  test('neutral completes with vague-but-valid answers (no re-ask misfire)', async ({ page, request }) => {
    const id = await H.createPoll(request);
    const run = await H.runPersona(page, id, 'neutral');
    const poll = await H.groundTruth(request, id);
    await attach(test.info(), poll, run);

    expect(poll.end_reason, 'neutral should complete').toBe('completed');
    const s = H.slots(poll);
    expect(s.every((q) => q.status === 'answered'),
      `vague answers should be accepted, every slot answered — got ${statuses(s)}`).toBeTruthy();
    // The whole point of "neutral": lukewarm/vague answers are accepted as-is.
    // needs_help must NOT fire (that would be the classifier over-reacting to
    // low-information but genuine answers and looping).
    const help = H.surveyIntents(run.intents).filter((i) => i.intent === 'needs_help');
    expect(help.length, `needs_help should not misfire on vague answers — fired ${help.length}x`).toBe(0);
  });

  test('rusher bails mid-survey after answering once', async ({ page, request }) => {
    const id = await H.createPoll(request);
    const run = await H.runPersona(page, id, 'rusher');
    const poll = await H.groundTruth(request, id);
    await attach(test.info(), poll, run);

    expect(poll.end_reason, 'rusher should bail').toBe('bailed');
    // The bail intent must have actually been classified (not a silence timeout).
    expect(H.intentFired(run.intents, 'wants_stop'),
      'wants_stop should fire for the rusher').toBeTruthy();
    const s = H.slots(poll);
    expect(s.some((q) => q.status === 'answered'),
      'rusher should have answered at least one question before bailing').toBeTruthy();
    expect(s.some((q) => q.status !== 'answered'),
      'rusher should leave at least one question unanswered (bailed early)').toBeTruthy();
  });

  test('confused triggers needs_help, then completes', async ({ page, request }) => {
    const id = await H.createPoll(request);
    const run = await H.runPersona(page, id, 'confused');
    const poll = await H.groundTruth(request, id);
    await attach(test.info(), poll, run);

    expect(poll.end_reason, 'confused should still complete').toBe('completed');
    // The defining behavior: at least one needs_help during the survey (agent
    // reassures + re-poses the same question rather than advancing blindly).
    const help = H.surveyIntents(run.intents).filter((i) => i.intent === 'needs_help');
    expect(help.length, 'needs_help should fire at least once for the confused persona').toBeGreaterThan(0);
    // And it must still terminate cleanly — every slot reaches a terminal state
    // (answered, or honestly skipped after the re-ask budget), never left dangling.
    const s = H.slots(poll);
    expect(s.every((q) => STATUS_TERMINAL.has(q.status)),
      `every slot terminal (answered/skipped) — got ${statuses(s)}`).toBeTruthy();
  });
});

function statuses(slots) {
  return slots.map((q) => q.status).join(', ') || '(none)';
}

// Attach the run detail to the HTML report so a failure is diagnosable without
// re-running: the ground-truth poll, the classifier intents, and the transcript.
async function attach(info, poll, run) {
  await info.attach('outcome.json', {
    body: JSON.stringify({
      end_reason: poll.end_reason,
      slots: H.slots(poll).map((q) => ({ status: q.status, answer: q.answer })),
      intents: run.intents,
      spoken: run.spoken,
      transcript: run.transcript,
    }, null, 2),
    contentType: 'application/json',
  });
}
