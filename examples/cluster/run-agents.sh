#!/usr/bin/env bash
# run-agents.sh — spawn a small batch of demo runs across both
# replicas so the Web UI has visible activity to look at.
#
# Companion to verify.sh: where verify.sh proves the cluster
# invariants programmatically, this script populates the UI with
# representative load so an operator can see runs on different
# replicas (different chip colors), some completed, some still
# running.
#
# Usage (from repo root, after `docker compose ... up -d`):
#   source .env.local                      # exports LOOMCYCLE_AUTH_TOKEN
#   ./examples/cluster/run-agents.sh
#
# Layout: 2 users × 3 prompt tiers = 6 parallel runs.
#   - alice + bob → simulate two distinct users in the Users view
#   - quick / medium / long → mix of fast-completing + still-running
#     rows when the UI is opened within ~30s
#   - alternating PORT → each user's history shows BOTH replica chips,
#     proving cluster distribution at a glance
#
# Re-run to top up activity; agent_ids carry a nanosecond suffix so
# nothing collides.

set -u

: "${LOOMCYCLE_AUTH_TOKEN:?LOOMCYCLE_AUTH_TOKEN must be set (source .env.local first)}"

for user in alice bob; do
  for tier in quick medium long; do
    case $tier in
      quick)  PORT=18787; PROMPT="One sentence about distributed systems." ;;
      medium) PORT=18788; PROMPT="Write 200 words on three distributed-systems milestones." ;;
      long)   PORT=18787; PROMPT="Write a thorough 1500-word essay tracing distributed systems history decade by decade since the 1960s. Be comprehensive." ;;
    esac
    AID="demo-${user}-${tier}-$(date +%s%N)"
    echo "→ $user/$tier on :$PORT  ($AID)"
    curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
      -X POST "http://localhost:$PORT/v1/runs" \
      -H "Content-Type: application/json" \
      -d "{\"agent\":\"default\",\"agent_id\":\"$AID\",\"user_id\":\"$user\",\"segments\":[{\"role\":\"user\",\"content\":[{\"type\":\"trusted-text\",\"text\":\"$PROMPT\"}]}]}" \
      --no-buffer >/dev/null 2>&1 &
  done
done

echo
echo "6 runs spawned. Open the UI within ~30s to catch the long ones running:"
echo '  open "http://localhost:18080/ui?token=$LOOMCYCLE_AUTH_TOKEN"'
