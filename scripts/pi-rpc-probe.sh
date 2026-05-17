#!/usr/bin/env bash
# pi-rpc-probe.sh — round-trip every RPC event kind the daemon depends on.
#
# Usage:
#   PROMPT='say hi and stop' ./scripts/pi-rpc-probe.sh > probe.jsonl
#
# Captured stdout is JSON Lines. Filter with jq:
#   jq -r .type probe.jsonl | sort -u
# Expected unique types include:
#   agent_start, agent_end, turn_start, turn_end,
#   message_start, message_update, message_end,
#   tool_execution_*, response, extension_ui_request
#
# The script is intentionally simple — it does NOT respond to
# extension_ui_requests, so any `select`/`confirm`/`input` will time out.
# Use --no-voice / --no-peon and a chatty model to keep it healthy.

set -u

PROMPT="${PROMPT:-Reply with the single word OK and stop.}"
MODEL="${PI_MODEL:-}"
THINKING="${PI_THINKING:-off}"

ARGS=( --mode rpc --no-session --thinking "$THINKING" --no-voice --no-peon )
[ -n "$MODEL" ] && ARGS+=( --model "$MODEL" )

{
  # 1. Probe initial state so we capture get_state -> response shape.
  printf '{"id":"q1","type":"get_state"}\n'
  # 2. Real prompt that exercises agent_start … agent_end.
  printf '{"id":"p1","type":"prompt","message":%s}\n' "$(jq -Rn --arg p "$PROMPT" '$p')"
  # 3. Hold stdin open just long enough for agent_end to arrive,
  #    then closing stdin asks pi to shut down cleanly.
  sleep "${PROBE_SLEEP:-25}"
} | pi "${ARGS[@]}"
