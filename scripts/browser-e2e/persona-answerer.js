// Persona-driven auto-answerer for browser E2E QA.
//
// Inject as an init script (runs BEFORE page scripts) on the /poll/<id> page,
// with the server started with -qa (mounts POST /api/qa/reply). It:
//   1. overrides getUserMedia to return a synthesized MediaStream (fake mic);
//   2. on each LISTENING turn, reads the agent's current line from #caption,
//      asks /api/qa/reply for THIS persona's next spoken answer (LLM in
//      character -> TTS in the persona's voice), and plays it into the fake mic.
// The answer then flows back through the real Silero VAD -> Whisper STT ->
// classifier path, exactly like a human talking.
//
// Pick the persona with window.__persona (set it in the same init script, or
// substitute it before injecting). Progress ("k / n" in #progress) gives the
// "answered so far" count personas like the rusher use to decide when to bail.
//
// Inspect window.__qa during/after a run: { persona, turns:[...], done }.
(() => {
  const PERSONA = (typeof window.__persona === 'string' && window.__persona) || 'enthusiast';
  let ac, dest, armed = false, busy = false;
  window.__qa = { persona: PERSONA, turns: [], done: false };

  async function ensure() {
    if (!ac) { ac = new (window.AudioContext || window.webkitAudioContext)(); dest = ac.createMediaStreamDestination(); }
    if (ac.state === 'suspended') { try { await ac.resume(); } catch (e) {} }
  }

  const md = navigator.mediaDevices, orig = md.getUserMedia.bind(md);
  md.getUserMedia = async (c) => { if (c && c.audio) { await ensure(); return dest.stream; } return orig(c); };

  async function speak(question, answered) {
    await ensure();
    let r;
    try {
      r = await fetch('/api/qa/reply', {
        method: 'POST', headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ persona: PERSONA, question, answered }),
      });
    } catch (e) { window.__qa.turns.push('FETCH-ERR ' + e.message); return; }
    if (!r.ok) { window.__qa.turns.push('HTTP ' + r.status); return; }
    const text = r.headers.get('X-QA-Text') || '';
    const audio = await ac.decodeAudioData(await r.arrayBuffer());
    const s = ac.createBufferSource(); s.buffer = audio; s.connect(dest); s.start();
    window.__qa.turns.push('SAY: ' + text);
  }

  // One answer per listening turn: arm on entering LISTENING, disarm on leaving,
  // so re-posed questions (repair / needs-help) each get a fresh reply.
  setInterval(async () => {
    const orb = document.querySelector('#orb'); if (!orb) return;
    const ended = document.querySelector('#ended');
    if (ended && !ended.classList.contains('hidden')) { window.__qa.done = true; return; }
    if (!orb.classList.contains('listening')) { armed = false; return; }
    if (armed || busy) return;
    armed = true; busy = true;
    const question = (document.querySelector('#caption')?.textContent || '').trim();
    const pm = (document.querySelector('#progress')?.textContent || '').match(/(\d+)\s*\/\s*(\d+)/);
    const answered = pm ? Math.max(0, parseInt(pm[1], 10) - 1) : 0;
    try { await speak(question, answered); } finally { busy = false; }
  }, 300);
})();
