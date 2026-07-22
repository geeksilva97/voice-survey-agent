// Playwright config for the headless persona QA suite.
//
// This suite drives the REAL /poll/<id> page in headless Chromium with a fake
// microphone and LLM-driven personas — the same technique as the Chrome DevTools
// MCP flow (see docs/BROWSER-QA.md), but as a one-command, assertion-backed
// regression suite. Nothing in the pipeline is mocked: VAD, Whisper STT, the turn
// classifier, Kokoro TTS, pacing, and the ending logic all run for real.
//
// The suite manages its OWN server on a dedicated port (default 8091) so it never
// clobbers a dev server you have running on :8090. Override with env:
//   QA_PORT           listen port for the test server            (default 8091)
//   QA_CLASSIFY_MODEL turn-classifier model                      (default claude-sonnet-5)
//   QA_REUSE_SERVER   "1" to reuse an already-running -qa server on QA_PORT
//
// The classifier defaults to claude-sonnet-5 because the rusher/confused personas
// depend on accurate intent detection (bail / needs_help); the key is loaded
// SERVER-SIDE from pepita's .env and never appears here or on the command line.
// Set QA_CLASSIFY_MODEL=qwen2.5:3b to run fully offline (lower fidelity).

const path = require('path');
const { defineConfig, devices } = require('@playwright/test');

const PORT = process.env.QA_PORT || '8091';
const MODEL = process.env.QA_CLASSIFY_MODEL || 'claude-sonnet-5';
const BASE_URL = `http://localhost:${PORT}`;
const POC_ROOT = path.resolve(__dirname, '..', '..', '..'); // scripts/browser-e2e/playwright -> poc/

module.exports = defineConfig({
  testDir: __dirname,
  // A full persona run (greeting + consent + N questions, each an LLM generation
  // + TTS + playback + real VAD endpointing) takes tens of seconds; enthusiast is
  // slowest because its answers are long to synthesize and play.
  timeout: 240_000,
  expect: { timeout: 10_000 },
  // One respondent at a time: the PoC is single-session and the speech engine is
  // mutex-serialized, so parallel runs would contend. Keep it serial.
  fullyParallel: false,
  workers: 1,
  // Personas are LLM-driven, so an occasional off-character generation is possible;
  // one retry absorbs that without masking a real, repeatable regression.
  retries: 1,
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]],

  use: {
    baseURL: BASE_URL,
    headless: true,
    // Grant mic permission up front (no dialog). The fake-mic override in
    // persona-answerer.js supplies the actual audio; these flags just make sure
    // getUserMedia is permitted and Web Audio isn't autoplay-blocked in headless.
    permissions: ['microphone'],
    launchOptions: {
      args: [
        '--use-fake-ui-for-media-stream',
        '--use-fake-device-for-media-stream',
        '--autoplay-policy=no-user-gesture-required',
      ],
    },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },

  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],

  // Build and launch the server with the DEV-ONLY persona endpoint (-qa) on the
  // dedicated test port. `exec` so SIGTERM on teardown reaches the server, not the
  // shell. Reuse an existing server only when explicitly asked (it must be -qa).
  webServer: {
    command: `go build -o bin/server ./cmd/server && exec ./bin/server -qa -addr :${PORT} -classify-model ${MODEL}`,
    cwd: POC_ROOT,
    url: BASE_URL + '/',
    timeout: 120_000,
    reuseExistingServer: process.env.QA_REUSE_SERVER === '1',
    stdout: 'pipe',
    stderr: 'pipe',
  },
});
