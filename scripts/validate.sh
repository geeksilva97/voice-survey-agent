#!/usr/bin/env bash
# One-command validation gate for the voice-survey PoC.
# Run this after EVERY change (see VALIDATION.md). It covers everything that can
# be checked without a browser: build, vet, unit tests, the LLM turn classifier,
# and the full server conversation loop (happy-path completion + silence ending)
# driven headlessly over WebSocket by cmd/probe.
#
# The browser voice pipeline (mic/VAD/playback/barge-in) is validated separately
# via scripts/browser-e2e/ — see VALIDATION.md.
#
# Usage:  ./scripts/validate.sh
# Exit 0 = all passed; non-zero = something failed (details above the summary).
set -uo pipefail
cd "$(dirname "$0")/.."

PORT="${VS_PORT:-8099}"          # test port (app default is 8090)
MODEL="${VS_MODEL:-qwen2.5:3b}"
PASS=0; FAIL=0; SKIP=0
SRV_PID=""

green(){ printf '\033[32m%s\033[0m\n' "$*"; }
red(){   printf '\033[31m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[33m%s\033[0m\n' "$*"; }

pass(){ green   "  PASS: $*"; PASS=$((PASS+1)); }
fail(){ red     "  FAIL: $*"; FAIL=$((FAIL+1)); }
skip(){ yellow  "  SKIP: $*"; SKIP=$((SKIP+1)); }

cleanup(){ [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null; }
trap cleanup EXIT

echo "== 1. build =="
if go build ./... 2>&1; then pass "go build ./..."; else fail "go build"; fi

echo "== 2. vet =="
if go vet ./... 2>&1; then pass "go vet ./..."; else fail "go vet"; fi

echo "== 3. unit tests (state machine + repair helpers) =="
if go test ./internal/survey/ ./internal/ws/ 2>&1; then pass "survey state machine + ws repair helpers"; else fail "unit tests"; fi

# Ollama-dependent steps
OLLAMA_UP=0
if curl -s -o /dev/null http://localhost:11434/api/tags; then OLLAMA_UP=1; fi

echo "== 4. LLM turn classifier (bail-out detection) =="
if [ "$OLLAMA_UP" = 1 ]; then
  if go test ./internal/llm/ -run 'TestClassifyTurn|TestClassifyQuirkyAnswer' 2>&1; then pass "LLM classify (answer vs wants_stop, quirky answers)"; else fail "LLM classify"; fi
else
  skip "ollama not on :11434 — start it and re-run"
fi

echo "== 5. intent-classification eval (labeled corpus vs live LLM) =="
# Gate runs the LOCAL model only (fast/offline). For the full cross-model
# comparison matrix run: go run ./cmd/eval   (defaults to all models).
if [ "$OLLAMA_UP" = 1 ]; then
  # -judge "" keeps the gate offline (the default ack judge is an Anthropic model).
  EVAL_OUT=$(go run ./cmd/eval -models "$MODEL" -judge "" 2>&1)
  echo "$EVAL_OUT" | grep -E 'EVAL (PASSED|FAILED)'
  if echo "$EVAL_OUT" | grep -q 'EVAL PASSED'; then pass "intent eval (acc>=90%, answer>=95%)"; else fail "intent eval"; echo "$EVAL_OUT" | tail -25; fi
else
  skip "ollama not on :11434 — eval needs it"
fi

# Models present?
MODELS_OK=1
[ -d models/kokoro-en-v0_19 ] && [ -d models/sherpa-onnx-whisper-base.en ] || MODELS_OK=0

echo "== 6. headless conversation (happy + silence) =="
if [ "$OLLAMA_UP" = 1 ] && [ "$MODELS_OK" = 1 ]; then
  go build -o bin/server ./cmd/server || fail "server build"
  ./bin/server -addr ":$PORT" >/tmp/vs-validate.log 2>&1 &
  SRV_PID=$!
  # wait for listen
  for _ in $(seq 1 30); do curl -s -o /dev/null "http://localhost:$PORT/" && break; sleep 0.5; done

  HAPPY=$(go run ./cmd/probe -addr "localhost:$PORT" -mode happy -max 30 2>&1 | tail -1)
  if echo "$HAPPY" | grep -q 'reason=completed'; then pass "happy path -> completed"; else fail "happy path ($HAPPY)"; fi

  SILENT=$(go run ./cmd/probe -addr "localhost:$PORT" -mode silent -max 10 2>&1 | tail -1)
  if echo "$SILENT" | grep -q 'reason=silence'; then pass "silence backstop -> silence"; else fail "silence backstop ($SILENT)"; fi
else
  [ "$MODELS_OK" = 1 ] || skip "models missing — run ./scripts/fetch-models.sh"
  [ "$OLLAMA_UP" = 1 ] || skip "ollama down — probes need it"
fi

echo
echo "===================="
echo "PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
echo "===================="
[ "$FAIL" -eq 0 ] || { red "VALIDATION FAILED"; exit 1; }
green "VALIDATION PASSED"
