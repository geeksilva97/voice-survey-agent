// Auto-answerer for browser E2E validation. Inject AFTER clicking "Start" on the
// /poll/<id> page (requires fakemic.js already installed as an init script).
//
// It watches the agent orb; whenever the conversation enters the "listening"
// state it plays the next answer clip through the fake mic. This makes a full
// multi-turn conversation run autonomously, immune to tool-call latency.
//
// Ground truth for pass/fail is the server's data/<id>.json "end_reason"
// (expect "completed" for this happy-path driver). window.__log records the flow.
//
// To validate the BAIL ending instead, set window.__answers = ['bail.wav'] before
// the first listening turn (or edit the array below).
(() => {
  window.__answers = window.__answers || ['ans0.wav', 'ans1.wav', 'ans2.wav'];
  window.__ai = 0;
  window.__answering = false;
  window.__log = [];
  if (window.__driver) clearInterval(window.__driver);

  window.__driver = setInterval(async () => {
    const orb = document.getElementById('orb');
    const endedEl = document.getElementById('ended');
    if (endedEl && getComputedStyle(endedEl).display !== 'none') {
      clearInterval(window.__driver);
      window.__log.push('ENDED: ' + (document.getElementById('endmsg') || {}).textContent);
      return;
    }
    if (orb && orb.classList.contains('listening') && !window.__answering) {
      window.__answering = true;
      const clip = window.__answers[Math.min(window.__ai, window.__answers.length - 1)];
      window.__ai++;
      const cap = (document.getElementById('caption') || {}).textContent || '';
      window.__log.push('answering with ' + clip + ' | agent: ' + cap.slice(0, 60));
      const dur = await window.__playAnswer(clip);
      // Hold the lock until the clip + VAD redemption + processing is well past.
      setTimeout(() => { window.__answering = false; }, (dur + 2.5) * 1000);
    }
  }, 400);

  return 'auto-answerer installed';
})();
