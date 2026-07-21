// Voice-survey browser client.
//
// Pipeline: Silero VAD (in-browser, WASM) detects when the respondent finishes
// speaking -> we send that utterance (PCM16 16kHz) over a single WebSocket ->
// the Go server transcribes, decides the next line, and streams back WAV audio
// we play. Half-duplex by default (VAD paused while the agent talks); optional
// barge-in keeps the mic hot so the user can interrupt.

const pollId = location.pathname.split("/").pop();

const el = (id) => document.getElementById(id);
const orb = el("orb"), caption = el("caption"), statusEl = el("status"),
      heard = el("heard"), progress = el("progress"), barfill = el("barfill");

let ws, vad, audioCtx;
let currentSource = null;   // active AudioBufferSourceNode (for barge-in stop)
let bargeIn = false;
let ended = false;

el("start").onclick = start;

async function start() {
  bargeIn = el("bargein").checked;
  el("pre").classList.add("hidden");
  el("live").classList.remove("hidden");
  setState("thinking", "connecting…");

  audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  await audioCtx.resume(); // unlock playback (we're inside a user gesture)

  try {
    await setupVAD();
  } catch (e) {
    setState("thinking", "microphone unavailable: " + e.message);
    return;
  }
  connect();
}

// ---- VAD ----
async function setupVAD() {
  if (!window.vad || !window.vad.MicVAD) {
    throw new Error("VAD library failed to load (offline?)");
  }
  vad = await window.vad.MicVAD.new({
    // vad-web 0.0.22 defaults to the Silero v5 model (~32ms/frame). The primary
    // end-of-turn knob is redemptionFrames = trailing silence tolerated before
    // we call the turn over. ~20 frames ≈ 640ms, which feels natural for survey
    // answers (raise it if the agent cuts people off; lower for snappier turns).
    redemptionFrames: 20,
    minSpeechFrames: 3,
    preSpeechPadFrames: 10,
    positiveSpeechThreshold: 0.6,
    negativeSpeechThreshold: 0.35,
    onSpeechStart: () => {
      // Barge-in: user talks while the agent is playing -> interrupt.
      if (bargeIn && currentSource) {
        stopPlayback();
        sendJSON({ type: "barge_in" });
      }
      if (isListening()) setState("listening", "listening…");
    },
    onSpeechEnd: (audio) => {
      if (ended) return;
      // audio: Float32Array @16kHz. Ship it as PCM16 and wait for the reply.
      sendPCM(audio);
      setState("thinking", "…");
    },
    onVADMisfire: () => {},
  });
  vad.pause(); // stays paused until it's the respondent's turn
}

function listenTurn() {
  if (ended) return;
  vad && vad.start();
  setState("listening", "your turn — speak whenever you're ready");
}

function isListening() {
  return orb.classList.contains("listening");
}

// ---- WebSocket ----
function connect() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${proto}://${location.host}/ws?poll=${pollId}`);
  ws.binaryType = "arraybuffer";

  ws.onopen = () => sendJSON({ type: "ready" });
  ws.onclose = () => { if (!ended) setState("thinking", "connection closed"); };
  ws.onerror = () => setState("thinking", "connection error");

  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) { playWav(ev.data); return; }
    let m; try { m = JSON.parse(ev.data); } catch { return; }
    switch (m.type) {
      case "agent_say":
        caption.textContent = m.text;
        if (m.total) { progress.textContent = `${m.index} / ${m.total}`; barfill.style.width = (100 * m.index / m.total) + "%"; }
        heard.textContent = "";
        setState("speaking", speakStatus(m.kind));
        break;
      case "transcript":
        heard.textContent = m.text ? `“${m.text}”` : "(didn't catch that)";
        break;
      case "cancel":
        stopPlayback();
        break;
      case "done":
        finish(m.reason);
        break;
    }
  };
}

function sendJSON(o) { if (ws && ws.readyState === 1) ws.send(JSON.stringify(o)); }

function sendPCM(float32) {
  const pcm = new Int16Array(float32.length);
  for (let i = 0; i < float32.length; i++) {
    let s = Math.max(-1, Math.min(1, float32[i]));
    pcm[i] = s < 0 ? s * 32768 : s * 32767;
  }
  if (ws && ws.readyState === 1) ws.send(pcm.buffer);
}

// ---- Playback ----
async function playWav(buf) {
  try {
    // Keep the mic hot only in barge-in mode; otherwise half-duplex.
    if (!bargeIn && vad) vad.pause();
    const audioBuf = await audioCtx.decodeAudioData(buf.slice(0));
    stopPlayback();
    const src = audioCtx.createBufferSource();
    src.buffer = audioBuf;
    src.connect(audioCtx.destination);
    src.onended = () => {
      if (currentSource === src) currentSource = null;
      onPlaybackDone();
    };
    currentSource = src;
    src.start();
  } catch (e) {
    onPlaybackDone(); // don't stall the turn loop on a decode error
  }
}

function stopPlayback() {
  if (currentSource) {
    try { currentSource.onended = null; currentSource.stop(); } catch {}
    currentSource = null;
  }
}

function onPlaybackDone() {
  if (ended) return;
  sendJSON({ type: "playback_done" });
  listenTurn();
}

// ---- UI helpers ----
function setState(cls, text) {
  orb.className = "orb " + cls;
  statusEl.textContent = text || "";
}
function speakStatus(kind) {
  return kind === "question" ? "asking…" : kind === "closing" ? "wrapping up…" : "speaking…";
}
function finish(reason) {
  ended = true;
  stopPlayback();
  if (vad) vad.pause();
  el("live").classList.add("hidden");
  el("ended").classList.remove("hidden");
  const msgs = {
    completed: "✅ All done — thank you for your answers!",
    bailed: "👋 Thanks for your time!",
    silence: "⏳ Session ended. Thanks for stopping by!",
  };
  el("endmsg").textContent = msgs[reason] || "Thanks!";
  barfill.style.width = "100%";
}
