#!/usr/bin/env bash
# Otto — single-user Telegram bot wrapping Claude Code with MCP tools.
# Idempotent: skips steps that are already done. Re-run anytime.
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

OTTO_CONFIG_DIR="$HOME/.config/otto"
OTTO_STATE_DIR="$HOME/.local/state/otto"
OTTO_BIN_DIR="$HOME/.local/bin"
OTTO_BIN="$OTTO_BIN_DIR/otto"
mkdir -p "$OTTO_CONFIG_DIR" "$OTTO_STATE_DIR" "$OTTO_BIN_DIR"

# ── OS detect ───────────────────────────────────────────────────────────────
OS="$(uname -s)"
case "$OS" in
  Linux)
    if ! command -v pacman &>/dev/null; then
      echo "  [!] Otto's setup.sh targets Arch (pacman). Other distros: install"
      echo "      go nodejs npm jq curl manually, then re-run."
      exit 1
    fi
    PKG_MGR=pacman
    ;;
  Darwin)
    PKG_MGR=brew
    echo "  [note] macOS detected. Will build the binary and write config, but"
    echo "         skip systemd unit install (use launchd or run ./otto manually)."
    ;;
  *) echo "  [!] Unsupported OS: $OS"; exit 1 ;;
esac

if [ "$EUID" -eq 0 ]; then
  echo "  [!] Don't run as root. Run as your normal user; sudo is invoked when needed."
  exit 1
fi

# ── Detect existing state ───────────────────────────────────────────────────
HAS_TELEGRAM=false
HAS_NOTION=false
HAS_GCAL_OAUTH=false
HAS_GCAL_AUTHED=false
HAS_GMAIL_OAUTH=false
HAS_GMAIL_AUTHED=false
HAS_GDRIVE_AUTHED=false
HAS_CLAUDE_AUTHED=false

CONFIG_FILE="$OTTO_CONFIG_DIR/config.toml"
CLIENT_SECRET_FILE="$OTTO_CONFIG_DIR/client_secret.json"
MCP_FILE="$OTTO_CONFIG_DIR/mcp.json"
SYSTEM_PROMPT_FILE="$OTTO_CONFIG_DIR/system_prompt.md"

# Reuse AbdurRazzaq's credential paths if they're already authed — saves
# re-doing the OAuth dance you already did once.
GMAIL_OAUTH_PATH="$HOME/.gmail-mcp/gcp-oauth.keys.json"
GCAL_TOKENS_PATH="$HOME/.config/google-calendar-mcp/tokens.json"
GDRIVE_CREDS_PATH="$HOME/.mcp-gdrive/credentials.json"

# Detect any Gmail accounts already authorized (one credentials-<label>.json
# per account, written by `npx … server-gmail-autoauth-mcp auth`).
EXISTING_GMAIL_LABELS=()
shopt -s nullglob
for f in "$HOME/.gmail-mcp/credentials-"*.json; do
  base="${f##*/credentials-}"
  EXISTING_GMAIL_LABELS+=("${base%.json}")
done
shopt -u nullglob

# If Otto's local copy is missing but AbdurRazzaq has already populated the
# Gmail MCP's copy with a Desktop OAuth client, reuse it transparently —
# saves the user re-downloading the same JSON from GCP Console.
if [ ! -f "$CLIENT_SECRET_FILE" ] && [ -f "$GMAIL_OAUTH_PATH" ]; then
  if python3 -c "import json,sys; d=json.load(open(sys.argv[1])); sys.exit(0 if 'installed' in d else 1)" "$GMAIL_OAUTH_PATH" 2>/dev/null; then
    cp "$GMAIL_OAUTH_PATH" "$CLIENT_SECRET_FILE"
    chmod 600 "$CLIENT_SECRET_FILE"
  fi
fi
[ -f "$CLIENT_SECRET_FILE" ] && HAS_GCAL_OAUTH=true
[ -f "$GCAL_TOKENS_PATH" ] && HAS_GCAL_AUTHED=true
[ -f "$GMAIL_OAUTH_PATH" ] && HAS_GMAIL_OAUTH=true
[ ${#EXISTING_GMAIL_LABELS[@]} -gt 0 ] && HAS_GMAIL_AUTHED=true
[ -f "$GDRIVE_CREDS_PATH" ] && HAS_GDRIVE_AUTHED=true

if [ -f "$CONFIG_FILE" ]; then
  grep -qE '^telegram_bot_token *= *"[^"]+' "$CONFIG_FILE" 2>/dev/null && HAS_TELEGRAM=true
  grep -qE '^notion_api_key *= *"[^"]+' "$CONFIG_FILE" 2>/dev/null && HAS_NOTION=true
fi

# Claude Code stores credentials under ~/.claude/ (interactive `claude /login`
# or `claude setup-token`). If the user has set ANTHROPIC_API_KEY in their
# shell env we accept that too — Otto inherits whatever auth claude already
# has, it doesn't manage Anthropic credentials itself.
if [ -d "$HOME/.claude" ] || [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  HAS_CLAUDE_AUTHED=true
fi

# ── Welcome ─────────────────────────────────────────────────────────────────
clear
cat <<'BANNER'
  ╔══════════════════════════════════════════╗
  ║                                          ║
  ║                Otto Setup                ║
  ║                                          ║
  ║   Telegram bot wrapping Claude Code.     ║
  ║   Just follow the prompts.               ║
  ║                                          ║
  ╚══════════════════════════════════════════╝
BANNER
echo ""
echo "  Checking what you already have..."
echo ""
$HAS_GCAL_OAUTH    && echo "    [ok] Google OAuth client installed" || echo "    • Google OAuth client — needed"
$HAS_GCAL_AUTHED   && echo "    [ok] Google Calendar signed in"     || echo "    • Google Calendar — needed"
$HAS_GMAIL_OAUTH   && echo "    [ok] Gmail OAuth keys"              || echo "    • Gmail OAuth keys — needed"
if $HAS_GMAIL_AUTHED; then
  echo "    [ok] Gmail accounts: ${EXISTING_GMAIL_LABELS[*]}"
else
  echo "    • Gmail accounts — needed"
fi
$HAS_GDRIVE_AUTHED && echo "    [ok] Google Drive signed in"        || echo "    • Google Drive — needed"
$HAS_NOTION        && echo "    [ok] Notion token saved"            || echo "    • Notion token — needed"
$HAS_TELEGRAM      && echo "    [ok] Telegram bot configured"       || echo "    • Telegram bot — needed"
$HAS_CLAUDE_AUTHED && echo "    [ok] Claude Code authenticated"     || echo "    • Claude Code — run 'claude /login' (or set ANTHROPIC_API_KEY)"
echo ""
echo "  Will only ask about ones still needed."
echo ""
read -p "  Press Enter to continue..."

# ── System deps ─────────────────────────────────────────────────────────────
clear
echo ""
echo "  ┌──────────────────────────────────────────┐"
echo "  │  System dependencies                     │"
echo "  └──────────────────────────────────────────┘"
echo ""

case "$PKG_MGR" in
  pacman)
    NEED_PKGS=()
    for pkg in go nodejs npm jq curl base-devel python lsof; do
      if ! pacman -Qi "$pkg" &>/dev/null; then NEED_PKGS+=("$pkg"); fi
    done
    if [ ${#NEED_PKGS[@]} -gt 0 ]; then
      echo "  Installing: ${NEED_PKGS[*]}"
      sudo pacman -S --needed --noconfirm "${NEED_PKGS[@]}"
    else
      echo "  [ok] go, nodejs, npm, jq, curl, python, lsof already installed"
    fi
    ;;
  brew)
    if ! command -v brew &>/dev/null; then
      echo "  [!] Homebrew not installed. Get it from https://brew.sh and rerun."
      exit 1
    fi
    for pkg in go node jq; do
      if ! command -v "$pkg" &>/dev/null; then brew install "$pkg"; fi
    done
    ;;
esac

# Claude Code CLI via npm (global)
if ! command -v claude &>/dev/null; then
  echo "  Installing Claude Code CLI..."
  if [ "$PKG_MGR" = pacman ]; then
    sudo npm i -g @anthropic-ai/claude-code
  else
    npm i -g @anthropic-ai/claude-code
  fi
fi
CLAUDE_BIN="$(command -v claude)"
echo "  [ok] claude at $CLAUDE_BIN"

# ── Build Otto ──────────────────────────────────────────────────────────────
echo ""
echo "  Building otto binary..."
go build -o "$OTTO_BIN" ./cmd/otto
echo "  [ok] $OTTO_BIN"

# ── Step 1: Google OAuth client (manual, one-time) ──────────────────────────
if ! $HAS_GCAL_OAUTH; then
  clear
  cat <<'STEP'

  ┌──────────────────────────────────────────┐
  │  Google Cloud Console (one-time setup)   │
  └──────────────────────────────────────────┘

  Open: https://console.cloud.google.com/

  1. Create or select a project.
  2. APIs & Services → Library — enable all four:
       • Google Calendar API
       • Gmail API
       • Google Drive API
       • Google Docs API
  3. APIs & Services → OAuth consent screen
       Choose External, fill required fields, add YOUR email under Test users.
  4. APIs & Services → Credentials → Create Credentials → OAuth client ID
       Application type: Desktop application → Create → Download JSON.

STEP
  read -p "  Drag the downloaded JSON file here: " GCAL_JSON
  GCAL_JSON=$(echo "$GCAL_JSON" | tr -d "'\"" | sed 's/^ *//;s/ *$//')
  if [ ! -f "$GCAL_JSON" ]; then
    echo "  Can't find that file."; exit 1
  fi
  GCAL_TYPE=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print('installed' if 'installed' in d else 'web' if 'web' in d else 'unknown')" "$GCAL_JSON")
  if [ "$GCAL_TYPE" != installed ]; then
    echo "  Wrong client type ($GCAL_TYPE) — must be Desktop application."; exit 1
  fi
  cp "$GCAL_JSON" "$CLIENT_SECRET_FILE"
  chmod 600 "$CLIENT_SECRET_FILE"
  HAS_GCAL_OAUTH=true
  echo "  [ok] Saved to $CLIENT_SECRET_FILE"
fi

# Reuse OAuth client for Gmail MCP if not already in place.
if ! $HAS_GMAIL_OAUTH; then
  mkdir -p "$HOME/.gmail-mcp"
  cp "$CLIENT_SECRET_FILE" "$GMAIL_OAUTH_PATH"
  chmod 600 "$GMAIL_OAUTH_PATH"
  HAS_GMAIL_OAUTH=true
fi

DESKTOP_CLIENT_ID=$(python3 -c "import json; print(json.load(open('$CLIENT_SECRET_FILE'))['installed']['client_id'])")
DESKTOP_CLIENT_SECRET=$(python3 -c "import json; print(json.load(open('$CLIENT_SECRET_FILE'))['installed']['client_secret'])")

# ── Step 2: Google Calendar OAuth (browser sign-in) ────────────────────────
if ! $HAS_GCAL_AUTHED; then
  clear
  echo ""
  echo "  ┌──────────────────────────────────────────┐"
  echo "  │  Google Calendar — browser sign-in        │"
  echo "  └──────────────────────────────────────────┘"
  echo ""
  echo "  A browser will open. Sign in and click Allow."
  echo ""
  read -p "  Press Enter..."
  GOOGLE_OAUTH_CREDENTIALS="$CLIENT_SECRET_FILE" npx -y @cocal/google-calendar-mcp auth
  HAS_GCAL_AUTHED=true
  echo "  [ok] Calendar connected"
fi

# ── Step 3: Google Drive ────────────────────────────────────────────────────
if ! $HAS_GDRIVE_AUTHED; then
  clear
  echo ""
  echo "  ┌──────────────────────────────────────────┐"
  echo "  │  Google Drive — browser sign-in           │"
  echo "  └──────────────────────────────────────────┘"
  echo ""
  echo "  Will open a browser. The Drive MCP starts a local OAuth listener."
  echo ""
  read -p "  Press Enter..."

  mkdir -p "$HOME/.mcp-gdrive"
  AUTH_LOG="$OTTO_STATE_DIR/gdrive-auth.log"
  : > "$AUTH_LOG"; chmod 600 "$AUTH_LOG"
  GOOGLE_CLIENT_ID="$DESKTOP_CLIENT_ID" \
    GOOGLE_CLIENT_SECRET="$DESKTOP_CLIENT_SECRET" \
    npx -y mcp-gdrive-workspace > "$AUTH_LOG" 2>&1 &
  GDRIVE_PID=$!
  for i in $(seq 1 180); do
    [ -f "$GDRIVE_CREDS_PATH" ] && { HAS_GDRIVE_AUTHED=true; break; }
    sleep 1
  done
  kill "$GDRIVE_PID" 2>/dev/null || true
  wait "$GDRIVE_PID" 2>/dev/null || true
  if $HAS_GDRIVE_AUTHED; then
    echo "  [ok] Drive connected"
  else
    echo ""
    echo "  [!] Drive auth timed out after 180s. See $AUTH_LOG"
    echo "      Rerun ./setup.sh once you've completed the browser flow."
    exit 1
  fi
fi

# ── Step 4: Gmail accounts (one or more) ─────────────────────────────────
clear
echo ""
echo "  ┌──────────────────────────────────────────┐"
echo "  │  Gmail accounts                           │"
echo "  └──────────────────────────────────────────┘"
echo ""

if [ ${#EXISTING_GMAIL_LABELS[@]} -gt 0 ]; then
  echo "  Already authorized: ${EXISTING_GMAIL_LABELS[*]}"
  echo ""
  echo "  Manage accounts (or Enter to keep as-is):"
  echo "    LABEL          add an account (e.g. 'work team')"
  echo "    -LABEL         remove an account + delete its credentials file"
  echo "    +LABEL         add (explicit form)"
  echo "    mix freely:    +work -old-dev"
else
  echo "  What Gmail accounts should Otto access?"
  echo "  Type labels separated by spaces (e.g. 'personal school work')."
  echo "  Each label gets its own browser sign-in."
  echo ""
  echo "  Press Enter for a single account named 'personal'."
fi
echo ""
read -p "  > " GMAIL_INPUT

# Build the final label set from existing accounts, then apply +adds and
# -removes from the input. Bare LABEL is treated as +LABEL for backwards
# compatibility with the previous single-add prompt.
#
# Implemented with regular bash arrays (not `declare -A`) so the script
# runs on macOS's stock bash 3.2 as well as modern bash 5+.
GMAIL_LABELS=("${EXISTING_GMAIL_LABELS[@]}")

contains_label() {
  local needle="$1"
  shift
  local item
  for item in "$@"; do
    [ "$item" = "$needle" ] && return 0
  done
  return 1
}

for token in $GMAIL_INPUT; do
  case "$token" in
    -*)
      lbl="${token#-}"
      [ -z "$lbl" ] && continue
      if contains_label "$lbl" "${GMAIL_LABELS[@]}"; then
        FILTERED=()
        for existing in "${GMAIL_LABELS[@]}"; do
          [ "$existing" = "$lbl" ] && continue
          FILTERED+=("$existing")
        done
        GMAIL_LABELS=("${FILTERED[@]}")
        cred="$HOME/.gmail-mcp/credentials-${lbl}.json"
        if [ -f "$cred" ]; then
          rm -f "$cred"
          echo "  [ok] Removed gmail-${lbl} (deleted $cred)"
        else
          echo "  [ok] Removed gmail-${lbl} (no credentials file existed)"
        fi
      else
        echo "  [skip] gmail-${lbl} not in current account list"
      fi
      ;;
    +*)
      lbl="${token#+}"
      [ -n "$lbl" ] && GMAIL_LABELS+=("$lbl")
      ;;
    *)
      GMAIL_LABELS+=("$token")
      ;;
  esac
done

# Sort + dedupe for stable mcp.json output across runs.
if [ ${#GMAIL_LABELS[@]} -gt 0 ]; then
  GMAIL_LABELS=($(printf '%s\n' "${GMAIL_LABELS[@]}" | sort -u))
fi
if [ ${#GMAIL_LABELS[@]} -eq 0 ]; then
  echo ""
  echo "  [note] No accounts left — defaulting to 'personal'."
  GMAIL_LABELS=("personal")
fi

# Auth any label that doesn't have credentials yet.
for label in "${GMAIL_LABELS[@]}"; do
  CRED_PATH="$HOME/.gmail-mcp/credentials-${label}.json"
  if [ -f "$CRED_PATH" ]; then
    echo "  [ok] gmail-${label} already authorized"
    continue
  fi
  PORT_PID=$(lsof -ti :3000 2>/dev/null | head -1 || true)
  if [ -n "$PORT_PID" ]; then
    echo ""
    echo "  [!] Port 3000 in use by PID $PORT_PID — Gmail auth needs it."
    echo "      Free it and rerun ./setup.sh"
    exit 1
  fi
  echo ""
  echo "  ─── gmail-${label} — sign in with the matching Google account ───"
  echo "  (Use 'Use another account' or an incognito window if needed.)"
  read -p "  Press Enter to start..."
  GMAIL_OAUTH_PATH="$GMAIL_OAUTH_PATH" \
    GMAIL_CREDENTIALS_PATH="$CRED_PATH" \
    npx -y @gongrzhe/server-gmail-autoauth-mcp auth
  echo "  [ok] gmail-${label} authorized"
done

# ── Step 5: Notion token ────────────────────────────────────────────────────
NOTION_TOKEN=""
if ! $HAS_NOTION; then
  clear
  cat <<'STEP'

  ┌──────────────────────────────────────────┐
  │  Notion                                   │
  └──────────────────────────────────────────┘

  1. https://www.notion.so/my-integrations → New integration
  2. Copy the Internal Integration Secret (starts with ntn_)
  3. In Notion, open any page Otto should access:
       ⋯ menu → Connections → add your integration

STEP
  read -p "  Paste your Notion token: " NOTION_TOKEN
  if [ -z "$NOTION_TOKEN" ]; then echo "  No token entered."; exit 1; fi
fi

# ── Step 6: Telegram bot + your user ID ─────────────────────────────────────
TELEGRAM_BOT_TOKEN=""
TELEGRAM_USER_ID=""
if ! $HAS_TELEGRAM; then
  clear
  cat <<'STEP'

  ┌──────────────────────────────────────────┐
  │  Telegram                                 │
  └──────────────────────────────────────────┘

  1. On Telegram, message @BotFather → /newbot → pick name + username
     Copy the token (looks like 123456789:ABC...).
  2. Message @userinfobot to get your numeric user ID.

STEP
  read -p "  Paste bot token: " TELEGRAM_BOT_TOKEN
  read -p "  Paste your user ID: " TELEGRAM_USER_ID
  if [ -z "$TELEGRAM_BOT_TOKEN" ] || [ -z "$TELEGRAM_USER_ID" ]; then
    echo "  Both required."; exit 1
  fi
fi

# ── Step 7: Claude Code authentication ─────────────────────────────────────
# Otto delegates auth to Claude Code itself — whatever scheme `claude` is
# set up with (interactive /login, setup-token, or ANTHROPIC_API_KEY in the
# parent env) is what Otto's subprocesses will inherit. No API key handling
# inside Otto.
if ! $HAS_CLAUDE_AUTHED; then
  clear
  cat <<'STEP'

  ┌──────────────────────────────────────────┐
  │  Claude Code authentication               │
  └──────────────────────────────────────────┘

  Otto reuses Claude Code's existing auth (no separate API key).

  Pick one (whichever you already have or prefer):

    a)  claude /login          — browser-based, for Pro/Max accounts or API console
    b)  claude setup-token     — non-interactive long-lived token (good for headless)
    c)  export ANTHROPIC_API_KEY=sk-ant-...  — set in your shell before running otto

  Run the one you want, then re-run ./setup.sh — it'll detect the auth
  and skip this step.

STEP
  exit 1
fi

# ── Write config.toml (preserve already-set fields) ─────────────────────────
write_toml_field() {
  local key="$1" val="$2" file="$3"
  # Use python+json.dumps to escape the value safely (handles ", \, etc.).
  # Integer detection by regex on the bash side; everything else is a TOML
  # string emitted via json.dumps (TOML strings are JSON-string-compatible).
  python3 - "$key" "$val" "$file" <<'PYEOF'
import json, re, sys
key, val, path = sys.argv[1], sys.argv[2], sys.argv[3]
is_int = re.match(r'^-?\d+$', val) is not None
new = f"{key} = {val}" if is_int else f"{key} = {json.dumps(val)}"
try:
    with open(path) as f:
        data = f.read()
except FileNotFoundError:
    data = ""
pat = re.compile(rf"^{re.escape(key)} *=.*$", re.MULTILINE)
if pat.search(data):
    data = pat.sub(new, data)
else:
    if data and not data.endswith("\n"):
        data += "\n"
    data += new + "\n"
with open(path, "w") as f:
    f.write(data)
PYEOF
}

[ ! -f "$CONFIG_FILE" ] && touch "$CONFIG_FILE"
chmod 600 "$CONFIG_FILE"

[ -n "$TELEGRAM_BOT_TOKEN" ] && write_toml_field telegram_bot_token "$TELEGRAM_BOT_TOKEN" "$CONFIG_FILE"
[ -n "$TELEGRAM_USER_ID" ]   && write_toml_field telegram_allowed_user_id "$TELEGRAM_USER_ID" "$CONFIG_FILE"
[ -n "$NOTION_TOKEN" ]       && write_toml_field notion_api_key "$NOTION_TOKEN" "$CONFIG_FILE"
write_toml_field claude_binary_path "$CLAUDE_BIN" "$CONFIG_FILE"
write_toml_field mcp_config_path "$MCP_FILE" "$CONFIG_FILE"
write_toml_field session_id_path "$OTTO_STATE_DIR/session_id" "$CONFIG_FILE"

# System prompt: copy repo's SYSTEM.md to ~/.config/otto/system_prompt.md on
# first run; never overwrite once it's there so user edits survive re-runs.
if [ ! -f "$SYSTEM_PROMPT_FILE" ] && [ -f "$DIR/SYSTEM.md" ]; then
  cp "$DIR/SYSTEM.md" "$SYSTEM_PROMPT_FILE"
  chmod 600 "$SYSTEM_PROMPT_FILE"
  echo "  [ok] Installed system prompt at $SYSTEM_PROMPT_FILE"
fi
[ -f "$SYSTEM_PROMPT_FILE" ] && write_toml_field system_prompt_path "$SYSTEM_PROMPT_FILE" "$CONFIG_FILE"

# ── Write mcp.json ──────────────────────────────────────────────────────────
# If notion_api_key was set in a previous run, read it back so we can write
# mcp.json without prompting again.
EXISTING_NOTION="$(grep -E '^notion_api_key *=' "$CONFIG_FILE" 2>/dev/null | sed -E 's/^notion_api_key *= *"(.*)"$/\1/')"
[ -z "$NOTION_TOKEN" ] && NOTION_TOKEN="$EXISTING_NOTION"

NOTION_TOKEN_VAL="$NOTION_TOKEN" \
CLIENT_SECRET_FILE="$CLIENT_SECRET_FILE" \
DESKTOP_CLIENT_ID="$DESKTOP_CLIENT_ID" \
DESKTOP_CLIENT_SECRET="$DESKTOP_CLIENT_SECRET" \
GMAIL_OAUTH_PATH="$GMAIL_OAUTH_PATH" \
HOME_DIR="$HOME" \
python3 - "${GMAIL_LABELS[@]}" > "$MCP_FILE" <<'PYEOF'
import json, os, sys
home = os.environ['HOME_DIR']
labels = sys.argv[1:]
config = {"mcpServers": {}}
config["mcpServers"]["notion"] = {
    "command": "npx",
    "args": ["-y", "@notionhq/notion-mcp-server"],
    "env": {"NOTION_TOKEN": os.environ.get('NOTION_TOKEN_VAL', '')},
}
config["mcpServers"]["google-calendar"] = {
    "command": "npx",
    "args": ["-y", "@cocal/google-calendar-mcp"],
    "env": {"GOOGLE_OAUTH_CREDENTIALS": os.environ['CLIENT_SECRET_FILE']},
}
config["mcpServers"]["gdrive"] = {
    "command": "npx",
    "args": ["-y", "mcp-gdrive-workspace"],
    "env": {
        "GOOGLE_CLIENT_ID": os.environ['DESKTOP_CLIENT_ID'],
        "GOOGLE_CLIENT_SECRET": os.environ['DESKTOP_CLIENT_SECRET'],
    },
}
for label in labels:
    config["mcpServers"][f"gmail-{label}"] = {
        "command": "npx",
        "args": ["-y", "@gongrzhe/server-gmail-autoauth-mcp"],
        "env": {
            "GMAIL_OAUTH_PATH": os.environ['GMAIL_OAUTH_PATH'],
            "GMAIL_CREDENTIALS_PATH": f"{home}/.gmail-mcp/credentials-{label}.json",
        },
    }
print(json.dumps(config, indent=2))
PYEOF
chmod 600 "$MCP_FILE"

# ── systemd user unit (Linux only) ──────────────────────────────────────────
if [ "$OS" = Linux ]; then
  SYSTEMD_DIR="$HOME/.config/systemd/user"
  mkdir -p "$SYSTEMD_DIR"
  cp "$DIR/systemd/otto.service" "$SYSTEMD_DIR/otto.service"

  # Enable lingering so it stays up across logout.
  if ! loginctl show-user "$USER" 2>/dev/null | grep -q '^Linger=yes'; then
    sudo loginctl enable-linger "$USER"
  fi

  systemctl --user daemon-reload
  systemctl --user enable otto.service
  systemctl --user restart otto.service

  # Smoke test: wait briefly, then check status.
  sleep 3
  if systemctl --user is-active --quiet otto.service; then
    echo ""
    echo "  [ok] otto is running."
    echo "       Logs:    journalctl --user -u otto -f"
    echo "       Status:  systemctl --user status otto"
    echo ""
    echo "  Send 'hi' to your Telegram bot to test."
  else
    echo "  [!] otto did not start cleanly."
    echo "      Check: journalctl --user -u otto -n 50"
    exit 1
  fi
else
  echo ""
  echo "  [note] macOS — to run otto:"
  echo "         $OTTO_BIN"
  echo "  Or install a launchd plist (not auto-installed on macOS)."
fi

# ── Done ────────────────────────────────────────────────────────────────────
echo ""
echo "  ╔══════════════════════════════════════════╗"
echo "  ║          Otto setup complete!            ║"
echo "  ╚══════════════════════════════════════════╝"
echo ""
echo "  Re-run ./setup.sh anytime to reconfigure or fix things."
echo ""
