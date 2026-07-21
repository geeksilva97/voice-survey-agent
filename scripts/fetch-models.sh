#!/usr/bin/env bash
# Downloads the local speech models for the PoC into poc/models/.
# - Whisper base.en  (STT)  ~150MB
# - Kokoro en v0.19  (TTS)  ~330MB
# Models are prebuilt ONNX bundles published by the sherpa-onnx project.
set -euo pipefail

cd "$(dirname "$0")/.."
MODELS_DIR="models"
mkdir -p "$MODELS_DIR"
cd "$MODELS_DIR"

REL="https://github.com/k2-fsa/sherpa-onnx/releases/download"

fetch() {
  local url="$1" dir="$2" tarball
  tarball="$(basename "$url")"
  if [ -d "$dir" ]; then
    echo "✓ $dir already present, skipping"
    return
  fi
  echo "↓ downloading $tarball ..."
  curl -L --fail -o "$tarball" "$url"
  echo "⁃ extracting ..."
  tar xf "$tarball"
  rm -f "$tarball"
  echo "✓ $dir ready"
}

# STT: Whisper base.en
fetch "$REL/asr-models/sherpa-onnx-whisper-base.en.tar.bz2" "sherpa-onnx-whisper-base.en"

# TTS: Kokoro English v0.19
fetch "$REL/tts-models/kokoro-en-v0_19.tar.bz2" "kokoro-en-v0_19"

echo
echo "All models ready under $(pwd)"
ls -1
