// Demo recorder: run one persona through the real voice page and produce a shareable
// MP4 — screen video + the AGENT's voice (half-voices: the persona answers go into
// the fake mic, not the speakers, so only the agent is audible, which is what we want).
//
// How it captures audio without a loopback device: an init script taps every Web
// Audio context so anything connected to `ctx.destination` (the agent's streamed TTS
// in client.js) is ALSO routed to a MediaStreamDestination we record with
// MediaRecorder. Playwright records the (silent) screen video in parallel; ffmpeg
// muxes the two, delaying the audio by the measured start offset so they line up.
//
//   node demo-record.js                # enthusiast, against :8090
//   QA_PERSONA=confused node demo-record.js
//   QA_PERSONA=silent node demo-record.js   # respondent never speaks -> silence-backstop ending
//   QA_BASE=http://localhost:8091 node demo-record.js
//
// Requires: the server running with -qa (default :8090), and ffmpeg on PATH.

const { chromium } = require('@playwright/test');
const path = require('path');
const fs = require('fs');
const { execFileSync } = require('child_process');

const BASE = process.env.QA_BASE || 'http://localhost:8090';
const PERSONA = process.env.QA_PERSONA || 'enthusiast';
const PRODUCT = 'hand-poured scented soy candles for the home';
const PURPOSE = 'see how folks feel about scent and burn time';
const OUT_DIR = path.resolve(__dirname, 'demo-out');
const PERSONA_SCRIPT = path.resolve(__dirname, '..', 'persona-answerer.js');
const SIZE = { width: 1280, height: 800 };

// Init script (runs before page scripts): tap ctx.destination -> a recordable stream.
function audioTap() {
  const Native = window.AudioContext || window.webkitAudioContext;
  if (!Native) return;
  window.__recDests = [];
  // Eagerly give every context a recording destination so it exists before audio flows.
  function Patched(...args) {
    const c = new Native(...args);
    try { c.__recDest = c.createMediaStreamDestination(); window.__recDests.push(c.__recDest); } catch (e) {}
    return c;
  }
  Patched.prototype = Native.prototype;
  window.AudioContext = Patched;
  window.webkitAudioContext = Patched;
  // Mirror any connect(ctx.destination) into that context's recording destination.
  const origConnect = AudioNode.prototype.connect;
  AudioNode.prototype.connect = function (dest, ...rest) {
    const r = origConnect.call(this, dest, ...rest);
    try {
      const ctx = this.context;
      if (ctx && ctx.__recDest && dest === ctx.destination) origConnect.call(this, ctx.__recDest);
    } catch (e) {}
    return r;
  };
  // Mix all tapped destinations into one track and record it. At start time only the
  // agent's context exists (the persona's fake-mic context is created a beat later and
  // never routes to its own destination), so this captures the agent's voice.
  window.__startRec = () => {
    const mix = new Native();
    const out = mix.createMediaStreamDestination();
    window.__recDests.forEach((d) => { try { mix.createMediaStreamSource(d.stream).connect(out); } catch (e) {} });
    window.__chunks = [];
    window.__rec = new MediaRecorder(out.stream, { mimeType: 'audio/webm;codecs=opus' });
    window.__rec.ondataavailable = (e) => { if (e.data && e.data.size) window.__chunks.push(e.data); };
    window.__rec.start();
  };
  window.__stopRec = () => new Promise((res) => {
    const r = window.__rec;
    if (!r) return res('');
    r.onstop = async () => {
      const buf = await new Blob(window.__chunks, { type: 'audio/webm' }).arrayBuffer();
      const b = new Uint8Array(buf);
      let s = '';
      for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
      res(btoa(s));
    };
    r.stop();
  });
}

// Init script for a SILENT respondent: hand the page a fake mic that only ever
// emits silence (nothing connected to the destination), so VAD never fires and the
// server's silence backstop kicks in — Ava nudges, then ends the call. Used instead
// of persona-answerer.js when QA_PERSONA=silent, to demo silence detection + ending.
function silentMic() {
  let ac, dest;
  async function ensure() {
    if (!ac) { ac = new (window.AudioContext || window.webkitAudioContext)(); dest = ac.createMediaStreamDestination(); }
    if (ac.state === 'suspended') { try { await ac.resume(); } catch (e) {} }
  }
  const md = navigator.mediaDevices;
  const orig = md.getUserMedia.bind(md);
  md.getUserMedia = async (c) => { if (c && c.audio) { await ensure(); return dest.stream; } return orig(c); };
  window.__qa = { persona: 'silent', turns: [], done: false };
}

async function main() {
  fs.mkdirSync(OUT_DIR, { recursive: true });

  // 1. Fresh poll via the API.
  const res = await fetch(`${BASE}/api/polls`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ product: PRODUCT, purpose: PURPOSE }),
  });
  if (!res.ok) throw new Error(`create poll: HTTP ${res.status} (is the server running with -qa on ${BASE}?)`);
  const { id } = await res.json();
  console.log(`poll ${id} — persona ${PERSONA}`);

  // 2. Headed-graph browser (headless is fine; audio still renders through Web Audio).
  const browser = await chromium.launch({
    args: [
      '--use-fake-ui-for-media-stream',
      '--use-fake-device-for-media-stream',
      '--autoplay-policy=no-user-gesture-required',
    ],
  });
  const context = await browser.newContext({
    permissions: ['microphone'],
    viewport: SIZE,
    recordVideo: { dir: OUT_DIR, size: SIZE },
  });
  const videoStart = Date.now(); // ~when the silent video timeline begins
  const page = await context.newPage();

  await page.addInitScript(audioTap);
  if (PERSONA === 'silent') {
    await page.addInitScript(silentMic);
  } else {
    await page.addInitScript((p) => { window.__persona = p; }, PERSONA);
    await page.addInitScript({ path: PERSONA_SCRIPT });
  }

  await page.goto(`${BASE}/poll/${id}`);
  await page.click('#start');
  await page.evaluate(() => window.__startRec());
  const recStart = Date.now();

  await page.waitForSelector('#ended:not(.hidden)', { timeout: 240_000 });
  await page.waitForTimeout(500); // let the closing line's tail flush

  const audioB64 = await page.evaluate(() => window.__stopRec());
  const audioPath = path.join(OUT_DIR, `${PERSONA}-${id}.audio.webm`);
  fs.writeFileSync(audioPath, Buffer.from(audioB64, 'base64'));

  const video = page.video();
  await context.close(); // flushes the video file
  await browser.close();
  const videoPath = await video.path();

  // 3. Mux: delay the audio by the (video-start -> rec-start) offset so it lines up.
  const offset = Math.max(0, (recStart - videoStart) / 1000).toFixed(2);
  const mp4 = path.join(OUT_DIR, `demo-${PERSONA}-${id}.mp4`);
  console.log(`muxing (audio offset ${offset}s)…`);
  execFileSync('ffmpeg', [
    '-y',
    '-i', videoPath,
    '-itsoffset', String(offset), '-i', audioPath,
    '-map', '0:v:0', '-map', '1:a:0',
    '-c:v', 'libx264', '-pix_fmt', 'yuv420p', '-preset', 'veryfast',
    '-c:a', 'aac', '-shortest', mp4,
  ], { stdio: ['ignore', 'ignore', 'inherit'] });

  const kb = (fs.statSync(mp4).size / 1024).toFixed(0);
  console.log(`\nDEMO: ${mp4} (${kb} KB)`);
}

main().catch((e) => { console.error(e); process.exit(1); });
