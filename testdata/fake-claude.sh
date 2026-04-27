#!/usr/bin/env bash
# Fake Claude binary for tests. Echoes a canned stream-json response.
# Reads any stdin to simulate prompt input.
set -e

# Optionally vary output by env var.
case "${FAKE_CLAUDE_MODE:-ok}" in
  ok)
    cat <<'JSON'
{"type":"assistant","message":{"content":[{"type":"text","text":"hello "}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]}}
{"type":"result","subtype":"success"}
JSON
    ;;
  fail)
    echo "claude: fake error" 1>&2
    exit 1
    ;;
  hang)
    sleep 30
    ;;
esac
