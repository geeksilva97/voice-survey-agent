// Voice-survey browser client.
//
// Pipeline: Silero VAD (in-browser, WASM) detects when the respondent finishes
// speaking -> we send that utterance (PCM16 16kHz) over a single WebSocket ->
// the Go server transcribes, decides the next line, and STREAMS back TTS audio
// as sentence chunks that we play through a queue (so the first words start fast).
// Half-duplex by default (VAD paused while the agent talks); optional barge-in
// keeps the mic hot so the user can interrupt.

const pollId = location.pathname.split("/").pop();

const el = (id) => document.getElementById(id);
const orb = el("orb"), caption = el("caption"), statusEl = el("status"),
      heard = el("heard"), progress = el("progress"), barfill = el("barfill"),
      transcriptEl = el("transcript");

// Append a line to the running conversation transcript (visible live + at end).
function appendLine(role, text) {
  if (!text) return;
  const row = document.createElement("div");
  row.className = "t-row t-" + role;
  const who = document.createElement("span");
  who.className = "t-who";
  who.textContent = role === "you" ? "You" : "Agent";
  row.appendChild(who);
  row.appendChild(document.createTextNode(text));
  transcriptEl.appendChild(row);
  transcriptEl.scrollTop = transcriptEl.scrollHeight;
}

let ws, vad, audioCtx, vadReady;
let bargeIn = false;
let ended = false;

// Playback queue (streamed sentence chunks for the current agent turn).
let audioQueue = [];
let playing = false;
let ttsEnded = false;          // server sent tts_end for this turn
let playbackDoneSent = false;  // guard: fire onPlaybackDone once per turn
let currentSource = null;      // active AudioBufferSourceNode (for barge-in stop)
let pendingEnd = null;         // end reason received while closing audio still plays

// "User is speaking" keep-alive: while true we ping the server so its silence
// timer keeps resetting during a long, pause-filled answer.
let userSpeaking = false;

el("start").onclick = start;
// Restart re-takes the same poll on the same link (server starts a fresh run
// on every new connection, so a reload is a clean restart).
const restartBtn = el("restart");
if (restartBtn) restartBtn.onclick = () => location.reload();

async function start() {
  bargeIn = el("bargein").checked;
  el("pre").classList.add("hidden");
  el("live").classList.remove("hidden");
  transcriptEl.classList.remove("hidden");
  setState("thinking", "connecting…");

  audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  await audioCtx.resume(); // unlock playback (inside the user gesture)

  // Load VAD in the BACKGROUND so it doesn't delay the agent's first words.
  vadReady = setupVAD().catch((e) => {
    setState("thinking", "microphone unavailable: " + e.message);
    throw e;
  });
  connect(); // server starts streaming the greeting immediately

  // Keep-alive ping so long answers never trip the server silence timer.
  setInterval(() => { if (userSpeaking && !ended) sendJSON({ type: "speaking" }); }, 2500);
}

// ---- VAD ----
async function setupVAD() {
  if (!window.vad || !window.vad.MicVAD) throw new Error("VAD library failed to load (offline?)");
  vad = await window.vad.MicVAD.new({
    // Silero v5 (~32ms/frame). redemptionFrames = trailing silence tolerated
    // before the turn is called over; ~28 ≈ 900ms tolerates mid-answer pauses.
    redemptionFrames: 28,
    minSpeechFrames: 3,
    preSpeechPadFrames: 10,
    positiveSpeechThreshold: 0.6,
    negativeSpeechThreshold: 0.35,
    onSpeechStart: () => {
      console.log("[vad] speech start");
      userSpeaking = true;
      sendJSON({ type: "speaking" }); // reset the server silence timer now
      if (bargeIn && currentSource) { stopPlayback(); sendJSON({ type: "barge_in" }); }
      if (isListening()) setState("listening", "🎤 I can hear you — keep going…");
    },
    onSpeechEnd: (audio) => {
      if (ended) return;
      console.log("[vad] speech end,", audio.length, "samples");
      userSpeaking = false;
      sendPCM(audio); // Float32Array @16kHz -> PCM16
      setState("thinking", "got it — thinking…");
    },
    onVADMisfire: () => { userSpeaking = false; },
  });
  vad.pause(); // stays paused until it's the respondent's turn
}

async function listenTurn() {
  if (ended) return;
  try { await vadReady; } catch { return; } // VAD may still be loading on the first turn
  if (ended) return;
  if (vad) vad.start();
  setState("listening", "your turn — speak whenever you're ready");
}

function isListening() { return orb.classList.contains("listening"); }

// ---- WebSocket ----
function connect() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${proto}://${location.host}/ws?poll=${pollId}`);
  ws.binaryType = "arraybuffer";
  ws.onopen = () => sendJSON({ type: "ready" });
  ws.onclose = () => { if (!ended && !pendingEnd) setState("thinking", "connection closed"); };
  ws.onerror = () => setState("thinking", "connection error");

  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) { audioQueue.push(ev.data); playNext(); return; }
    let m; try { m = JSON.parse(ev.data); } catch { return; }
    switch (m.type) {
      case "agent_say":
        // New agent turn: reset the playback queue/flags.
        audioQueue = []; ttsEnded = false; playbackDoneSent = false; pendingEnd = null;
        if (!bargeIn && vad) vad.pause();
        caption.textContent = m.text;
        if (m.total) { progress.textContent = `${m.index} / ${m.total}`; barfill.style.width = (100 * m.index / m.total) + "%"; }
        heard.textContent = "";
        appendLine("agent", m.text);
        setState("speaking", speakStatus(m.kind));
        break;
      case "tts_end":
        ttsEnded = true;
        if (!playing && audioQueue.length === 0) onPlaybackDone();
        break;
      case "transcript":
        heard.textContent = m.text ? `“${m.text}”` : "(didn't catch that)";
        appendLine("you", m.text);
        break;
      case "cancel":
        stopPlayback(); audioQueue = [];
        break;
      case "done":
        // Don't cut off the closing message: if audio is still playing/queued,
        // remember the reason and end once playback drains (see onPlaybackDone).
        pendingEnd = m.reason;
        if (!playing && audioQueue.length === 0) finish(pendingEnd);
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

// ---- Streamed playback ----
async function playNext() {
  if (playing) return;
  const buf = audioQueue.shift();
  if (!buf) { if (ttsEnded) onPlaybackDone(); return; }
  playing = true;
  let audioBuf;
  try { audioBuf = await audioCtx.decodeAudioData(buf.slice(0)); }
  catch { playing = false; return playNext(); }
  const src = audioCtx.createBufferSource();
  src.buffer = audioBuf;
  src.connect(audioCtx.destination);
  src.onended = () => {
    if (currentSource === src) currentSource = null;
    playing = false;
    playNext();
  };
  currentSource = src;
  src.start();
}

function stopPlayback() {
  if (currentSource) { try { currentSource.onended = null; currentSource.stop(); } catch {} currentSource = null; }
  playing = false;
}

function onPlaybackDone() {
  if (ended || playbackDoneSent) return;
  playbackDoneSent = true;
  if (pendingEnd) { finish(pendingEnd); return; } // closing audio finished → end now
  sendJSON({ type: "playback_done" });
  listenTurn();
}

// ---- UI helpers ----
function setState(cls, text) { orb.className = "orb " + cls; statusEl.textContent = text || ""; }
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
