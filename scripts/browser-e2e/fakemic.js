// Fake-microphone harness for browser E2E validation.
//
// Inject this as an init script (runs BEFORE page scripts) on the /poll/<id>
// page. It overrides getUserMedia to return a synthesized-speech MediaStream
// instead of a real mic, so the Silero VAD -> STT -> advance loop can be driven
// deterministically with no human speaking and no permission prompt.
//
// Clips are the ones produced by `go run ./cmd/genclips` (served at
// /static/demo/*.wav). After the page loads, call window.__playAnswer('ans0.wav')
// to make the "respondent" speak; VAD fires onSpeechEnd ~640ms after it stops.
//
// Chrome DevTools MCP: pass as navigate_page initScript.
// Plain DevTools console: paste, then reload once so it wraps getUserMedia early.
(() => {
  const log = (...a) => console.log('[fakemic]', ...a);
  const clipNames = ['ans0.wav', 'ans1.wav', 'ans2.wav', 'bail.wav', 'calque.wav', 'yes.wav', 'offtopic.wav', 'unsure.wav', 'goback.wav'];
  const clips = {};
  let ac, dest;
  window.__fakemicReady = false;

  async function ensure() {
    if (!ac) {
      ac = new (window.AudioContext || window.webkitAudioContext)();
      dest = ac.createMediaStreamDestination();
    }
    if (ac.state === 'suspended') { try { await ac.resume(); } catch (e) {} }
  }

  async function preload() {
    await ensure();
    for (const n of clipNames) {
      try {
        const r = await fetch('/static/demo/' + n);
        const b = await r.arrayBuffer();
        clips[n] = await ac.decodeAudioData(b);
      } catch (e) { log('load failed', n, e.message); }
    }
    window.__fakemicReady = true;
    log('clips ready');
  }

  const md = navigator.mediaDevices;
  const orig = md.getUserMedia.bind(md);
  md.getUserMedia = async (c) => {
    if (c && c.audio) {
      await ensure();
      if (!window.__fakemicReady) preload();
      log('fake mic stream');
      return dest.stream;
    }
    return orig(c);
  };

  // Speak one clip into the fake mic. Returns its duration (seconds).
  window.__playAnswer = async (name) => {
    await ensure();
    if (!clips[name]) return -1;
    const s = ac.createBufferSource();
    s.buffer = clips[name];
    s.connect(dest);
    s.start();
    log('playing', name);
    return clips[name].duration;
  };

  preload();
})();
