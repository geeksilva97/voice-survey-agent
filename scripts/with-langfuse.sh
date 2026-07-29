#!/usr/bin/env bash
# Run any command with this machine's local Langfuse credentials exported.
#
#   ./scripts/with-langfuse.sh go run ./cmd/server
#   ./scripts/with-langfuse.sh go run ./cmd/eval -run before-prompt-fix
#
# The keys live in the local Langfuse stack's own .env (written when the stack
# was provisioned headlessly) rather than in this repo, so nothing secret is ever
# committed here. Point LANGFUSE_ENV_FILE elsewhere to use a different instance —
# e.g. Langfuse Cloud keys.
#
# Without this wrapper the app simply runs untraced: obs.Init sees no credentials
# and every export path becomes a no-op.
set -euo pipefail

ENV_FILE="${LANGFUSE_ENV_FILE:-$HOME/projects/langfuse-local/.env}"

if [ ! -r "$ENV_FILE" ]; then
  echo "with-langfuse: no readable env file at $ENV_FILE" >&2
  echo "  start the local stack:  cd ~/projects/langfuse-local && docker compose up -d" >&2
  echo "  or set LANGFUSE_ENV_FILE to a file holding your Langfuse keys." >&2
  exit 1
fi

# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a

# The stack's .env names the keys LANGFUSE_INIT_* (that's what provisions them);
# the app reads the standard LANGFUSE_* names. Bridge them, without overriding
# anything the caller set explicitly.
export LANGFUSE_HOST="${LANGFUSE_HOST:-http://localhost:3001}"
export LANGFUSE_PUBLIC_KEY="${LANGFUSE_PUBLIC_KEY:-${LANGFUSE_INIT_PROJECT_PUBLIC_KEY:-}}"
export LANGFUSE_SECRET_KEY="${LANGFUSE_SECRET_KEY:-${LANGFUSE_INIT_PROJECT_SECRET_KEY:-}}"

if [ -z "$LANGFUSE_PUBLIC_KEY" ] || [ -z "$LANGFUSE_SECRET_KEY" ]; then
  echo "with-langfuse: $ENV_FILE has no Langfuse key pair" >&2
  exit 1
fi

# Fail fast with a clear message rather than letting the app silently buffer
# spans toward an unreachable host.
if ! curl -sf -m 5 "$LANGFUSE_HOST/api/public/health" >/dev/null 2>&1; then
  echo "with-langfuse: $LANGFUSE_HOST is not answering /api/public/health" >&2
  echo "  is the stack up?  cd ~/projects/langfuse-local && docker compose up -d" >&2
  exit 1
fi

echo "with-langfuse: tracing to $LANGFUSE_HOST (key ${LANGFUSE_PUBLIC_KEY:0:11}…)" >&2
exec "$@"
