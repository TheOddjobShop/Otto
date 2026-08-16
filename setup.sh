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
    echo "  [note] macOS detected. Will build the binary, write config, and"
    echo "         install a launchd user agent (~/Library/LaunchAgents/com.otto.bot.plist)."
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
TOTO_PERSONA_FILE="$OTTO_CONFIG_DIR/toto_persona.md"
TOOT_PERSONA_FILE="$OTTO_CONFIG_DIR/toot_persona.md"

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
    install -m 600 "$GMAIL_OAUTH_PATH" "$CLIENT_SECRET_FILE"
  fi
fi
[ -f "$CLIENT_SECRET_FILE" ] && HAS_GCAL_OAUTH=true
[ -f "$GCAL_TOKENS_PATH" ] && HAS_GCAL_AUTHED=true
[ -f "$GMAIL_OAUTH_PATH" ] && HAS_GMAIL_OAUTH=true
[ ${#EXISTING_GMAIL_LABELS[@]} -gt 0 ] && HAS_GMAIL_AUTHED=true
[ -f "$GDRIVE_CREDS_PATH" ] && HAS_GDRIVE_AUTHED=true

# ── Credential validation ───────────────────────────────────────────────────
# Every secret is checked against the service that will actually use it, at the
# moment it is entered — not trusted until Otto fails hours later with an error
# the user has to correlate back to a typo they could not see themselves make
# (the token prompts are silent by necessity).
#
# Two rules the validators follow:
#
#   1. Never echo a secret, and never put one in argv. `ps` is world-readable
#      on a default Linux, so a token in a curl URL is visible to every user on
#      the box. curl reads its URL and headers from a 0600 config file instead.
#   2. Report the SERVICE's own words. "Unauthorized" from Telegram and
#      "API token is invalid" from Notion mean different things, and paraphrasing
#      them into a generic failure throws away the only useful diagnostic.
#
# Set OTTO_SKIP_VALIDATION=1 to bypass every network check (offline installs).

VALIDATION_ERROR=""   # one-line diagnosis from the last failed validator
VALIDATION_NOTE=""    # confirmation detail on success, e.g. the bot's @username

# curl_quiet <config-lines...> — runs curl with url/headers supplied via a
# 0600 config file so nothing sensitive reaches the process list.
# Returns 127 when curl itself is absent, which callers treat as "cannot
# verify" rather than "invalid" — these run before system deps are installed,
# and rejecting a good token for want of curl would be worse than not checking.
curl_quiet() {
  local cfg rc
  command -v curl >/dev/null 2>&1 || return 127
  cfg="$(mktemp)"; chmod 600 "$cfg"
  printf '%s\n' "$@" > "$cfg"
  printf 'silent\nshow-error\nmax-time = 20\n' >> "$cfg"
  curl -K "$cfg" 2>/dev/null; rc=$?
  rm -f "$cfg"
  return $rc
}

# strip_ws — tokens never contain whitespace, and a wrapped or space-padded
# paste is the single most common way these prompts go wrong.
strip_ws() { printf '%s' "$1" | tr -d '[:space:]'; }

validate_telegram_token() {
  local token resp
  token="$(strip_ws "$1")"
  [ -n "$token" ] || { VALIDATION_ERROR="nothing entered."; return 1; }

  # Shape first: a local check gives a far better message than a 401 would,
  # and catches pasting the user ID or BotFather's whole sentence.
  if ! printf '%s' "$token" | grep -qE '^[0-9]{6,12}:[A-Za-z0-9_-]{30,45}$'; then
    VALIDATION_ERROR="that isn't a bot token. Expected digits, a colon, then ~35 characters — like 123456789:AAH… (got ${#token} characters)."
    return 1
  fi
  [ "${OTTO_SKIP_VALIDATION:-}" = 1 ] && { VALIDATION_NOTE="shape ok (network check skipped)"; return 0; }

  resp="$(curl_quiet "url = \"https://api.telegram.org/bot$token/getMe\"")"
  case $? in
    0) ;;
    127) VALIDATION_NOTE="shape ok (curl not installed yet — will verify on the next run)"; return 0 ;;
    *)  VALIDATION_ERROR="could not reach api.telegram.org. Check the network, or re-run with OTTO_SKIP_VALIDATION=1."
        return 1 ;;
  esac
  if printf '%s' "$resp" | grep -q '"ok":true'; then
    VALIDATION_NOTE="@$(printf '%s' "$resp" | sed -n 's/.*"username":"\([^"]*\)".*/\1/p')"
    return 0
  fi
  VALIDATION_ERROR="Telegram rejected it: $(printf '%s' "$resp" | sed -n 's/.*"description":"\([^"]*\)".*/\1/p')"
  case "$resp" in
    *Unauthorized*) VALIDATION_ERROR="$VALIDATION_ERROR
      That means the token is wrong or was revoked. In @BotFather: /mybots → your bot → API Token." ;;
  esac
  return 1
}

validate_telegram_user_id() {
  local id
  id="$(strip_ws "$1")"
  [ -n "$id" ] || { VALIDATION_ERROR="nothing entered."; return 1; }
  printf '%s' "$id" | grep -qE '^[0-9]{5,15}$' || {
    VALIDATION_ERROR="a user ID is all digits (like 123456789). If you pasted a username or the bot token, that's the wrong value."
    return 1
  }
  VALIDATION_NOTE="id $id"
  return 0
}

validate_notion_token() {
  local token resp
  token="$(strip_ws "$1")"
  [ -n "$token" ] || { VALIDATION_ERROR="nothing entered."; return 1; }
  case "$token" in
    ntn_*|secret_*) ;;
    *) VALIDATION_ERROR="a Notion integration secret starts with ntn_ (or secret_ on older integrations)."; return 1 ;;
  esac
  [ "${OTTO_SKIP_VALIDATION:-}" = 1 ] && { VALIDATION_NOTE="shape ok (network check skipped)"; return 0; }

  resp="$(curl_quiet \
    "url = \"https://api.notion.com/v1/users/me\"" \
    "header = \"Authorization: Bearer $token\"" \
    "header = \"Notion-Version: 2022-06-28\"")"
  case $? in
    0) ;;
    127) VALIDATION_NOTE="shape ok (curl not installed yet)"; return 0 ;;
    *)  VALIDATION_ERROR="could not reach api.notion.com."; return 1 ;;
  esac
  if printf '%s' "$resp" | grep -q '"object":"user"'; then
    VALIDATION_NOTE="integration \"$(printf '%s' "$resp" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')\""
    return 0
  fi
  VALIDATION_ERROR="Notion rejected it: $(printf '%s' "$resp" | sed -n 's/.*"message":"\([^"]*\)".*/\1/p')"
  return 1
}

validate_google_client_secret() {
  local file="$1" cid
  [ -s "$file" ] || { VALIDATION_ERROR="file is missing or empty: $file"; return 1; }
  jq -e . "$file" >/dev/null 2>&1 || { VALIDATION_ERROR="not valid JSON. Re-download it from the Google Cloud console."; return 1; }
  # Desktop clients nest under .installed; web clients under .web. Anything
  # else is the wrong artifact — usually a service-account key, which cannot
  # do the user-consent flow Otto needs.
  cid="$(jq -r '.installed.client_id // .web.client_id // empty' "$file" 2>/dev/null)"
  if [ -z "$cid" ]; then
    if jq -e '.type == "service_account"' "$file" >/dev/null 2>&1; then
      VALIDATION_ERROR="that's a service-account key. Otto needs an OAuth *Desktop app* client, which is what asks for your consent in a browser."
    else
      VALIDATION_ERROR="no client_id inside. Expected the OAuth client JSON with an \"installed\" or \"web\" section."
    fi
    return 1
  fi
  [ -n "$(jq -r '.installed.client_secret // .web.client_secret // empty' "$file" 2>/dev/null)" ] || {
    VALIDATION_ERROR="client_id present but client_secret missing — the file is truncated."
    return 1
  }
  VALIDATION_NOTE="client ${cid%%-*}…"
  return 0
}

# prompt_validated <secret|plain> <validator-fn> <prompt> — reads until the
# value validates, or the user chooses to stop. Result lands in PROMPT_RESULT.
PROMPT_RESULT=""
prompt_validated() {
  local mode="$1" validator="$2" prompt="$3" value again
  while :; do
    if [ "$mode" = secret ]; then
      read -rs -p "$prompt" value; echo
    else
      read -r -p "$prompt" value
    fi
    value="$(strip_ws "$value")"
    VALIDATION_ERROR=""; VALIDATION_NOTE=""
    if "$validator" "$value"; then
      PROMPT_RESULT="$value"
      [ -n "$VALIDATION_NOTE" ] && echo "  [ok] verified — $VALIDATION_NOTE" || echo "  [ok] verified"
      return 0
    fi
    echo ""
    echo "  [!] $VALIDATION_ERROR"
    echo ""
    read -r -p "  Try again? [Y/n] " again
    case "$again" in
      [Nn]*) PROMPT_RESULT=""; return 1 ;;
    esac
  done
}

# detect_telegram_user_id <token> — asks the user to message the bot and reads
# their numeric id straight off the update.
#
# Better than sending them to @userinfobot for a number to copy: the id that
# arrives here is by construction the one that will be on their messages, so
# the allowlist cannot be subtly wrong. A wrong allowlist is a miserable bug —
# Otto silently drops every message and looks simply dead.
DETECTED_USER_ID=""
DETECTED_USER_NAME=""
detect_telegram_user_id() {
  local token="$1" resp last id i
  command -v jq >/dev/null 2>&1 || return 1

  # Note where the backlog ends so we react to a fresh message, not one sent
  # months ago — possibly from a different account.
  resp="$(curl_quiet "url = \"https://api.telegram.org/bot$token/getUpdates?offset=-1&timeout=0\"")" || return 1
  case "$resp" in
    *Conflict*)
      # Something else is already long-polling this token; asking again would
      # just fight it.
      VALIDATION_ERROR="another Otto is already polling this bot. Stop it first: systemctl --user stop otto"
      return 1 ;;
  esac
  last="$(printf '%s' "$resp" | jq -r '[.result[].update_id] | max // 0' 2>/dev/null)" || last=0
  [ -n "$last" ] || last=0

  echo ""
  echo "  Now send your bot any message — just say hi."
  echo "  (waiting up to 90 seconds; ctrl-c to enter the ID by hand)"
  for i in $(seq 1 30); do
    resp="$(curl_quiet "url = \"https://api.telegram.org/bot$token/getUpdates?offset=$((last+1))&timeout=3\"")" || return 1
    id="$(printf '%s' "$resp" | jq -r '[.result[].message.from.id] | last // empty' 2>/dev/null)"
    if [ -n "$id" ]; then
      DETECTED_USER_ID="$id"
      DETECTED_USER_NAME="$(printf '%s' "$resp" | jq -r '[.result[].message.from.first_name] | last // empty' 2>/dev/null)"
      return 0
    fi
  done
  VALIDATION_ERROR="no message arrived."
  return 1
}

if [ -f "$CONFIG_FILE" ]; then
  grep -qE '^telegram_bot_token *= *"[^"]+' "$CONFIG_FILE" 2>/dev/null && HAS_TELEGRAM=true
  grep -qE '^notion_api_key *= *"[^"]+' "$CONFIG_FILE" 2>/dev/null && HAS_NOTION=true

  # A value being PRESENT is not the same as it being VALID. Re-check anything
  # already on disk, because the failure it causes lands far from its cause:
  # setup reports success, then Otto exits at boot with "telegram: Unauthorized"
  # and the user has to work backwards to a token they were never shown.
  # Revocation, a half-finished earlier run and an edited file all look
  # identical here otherwise.
  if $HAS_TELEGRAM; then
    EXISTING_TG_TOKEN="$(sed -n 's/^telegram_bot_token *= *"\([^"]*\)".*/\1/p' "$CONFIG_FILE" | head -1)"
    if validate_telegram_token "$EXISTING_TG_TOKEN"; then
      TELEGRAM_TOKEN_NOTE="$VALIDATION_NOTE"
    else
      HAS_TELEGRAM=false
      TELEGRAM_STALE_REASON="$VALIDATION_ERROR"
    fi
    unset EXISTING_TG_TOKEN
  fi
  if $HAS_NOTION; then
    EXISTING_NOTION_TOKEN="$(sed -n 's/^notion_api_key *= *"\([^"]*\)".*/\1/p' "$CONFIG_FILE" | head -1)"
    if ! validate_notion_token "$EXISTING_NOTION_TOKEN"; then
      HAS_NOTION=false
      NOTION_STALE_REASON="$VALIDATION_ERROR"
    fi
    unset EXISTING_NOTION_TOKEN
  fi
fi

# Claude Code stores credentials as an artifact — ~/.claude/.credentials.json
# (interactive `claude /login` or `claude setup-token` on Linux), the macOS
# Keychain ("Claude Code-credentials"), or an apiKeyHelper in settings.json.
# If the user has set ANTHROPIC_API_KEY in their shell env we accept that too.
# We check for a real credential rather than the mere presence of ~/.claude,
# which the CLI creates even when the user quits /login without authenticating.
# Otto inherits whatever auth claude already has, it doesn't manage Anthropic
# credentials itself.
if [ -n "${ANTHROPIC_API_KEY:-}" ] || [ -f "$HOME/.claude/.credentials.json" ]; then
  HAS_CLAUDE_AUTHED=true
elif [ "$OS" = Darwin ] && security find-generic-password -s "Claude Code-credentials" >/dev/null 2>&1; then
  HAS_CLAUDE_AUTHED=true
elif [ -f "$HOME/.claude/settings.json" ] && grep -q '"apiKeyHelper"' "$HOME/.claude/settings.json" 2>/dev/null; then
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
if $HAS_NOTION; then echo "    [ok] Notion token verified"
elif [ -n "${NOTION_STALE_REASON:-}" ]; then echo "    [!] Notion token on file was rejected — will re-ask"
else echo "    • Notion token — needed"; fi
if $HAS_TELEGRAM; then echo "    [ok] Telegram bot verified (${TELEGRAM_TOKEN_NOTE:-ok})"
elif [ -n "${TELEGRAM_STALE_REASON:-}" ]; then echo "    [!] Telegram token on file was rejected — will re-ask"
else echo "    • Telegram bot — needed"; fi
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
    for pkg in go nodejs npm jq curl base-devel cmake git python lsof; do
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
    for pkg in go node jq python3; do
      if ! command -v "$pkg" &>/dev/null; then brew install "$pkg"; fi
    done
    ;;
esac

# ── MCP server version pins ─────────────────────────────────────────────────
# Every community MCP server is fetched with `npx` at BOTH setup time (OAuth
# flows) and on every bot start (mcp.json spawns them). Unpinned, that means
# each `systemctl restart otto` silently pulls whatever the registry serves
# right now, and these processes are handed live OAuth client credentials via
# their environment. A pin turns "whatever is latest today" into a reviewed,
# reproducible dependency, and makes an unexpected upgrade a deliberate act.
#
# --ignore-scripts (applied at every call site) additionally stops npm
# lifecycle hooks from executing on install.
#
# To bump: check the changelog, then update the version here — it flows to
# both the auth invocations below and the generated mcp.json.
MCP_VER_NOTION="2.4.1"
MCP_VER_GCAL="2.6.2"
MCP_VER_GDRIVE="0.1.0"
MCP_VER_GMAIL="1.1.11"

# Claude Code CLI via npm (global).
# Security note: this installs the latest published version from the npm
# registry without a version pin or integrity check.  To lock a specific
# release, replace `@anthropic-ai/claude-code` with
# `@anthropic-ai/claude-code@<version>` and verify the dist-tag or checksum
# from a trusted source before re-pinning.  On Arch Linux the install runs
# under sudo, so a supply-chain compromise would execute as root.
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
# Stamp main.version from git so the auto-updater can compare against GitHub
# release tags. Without this the binary is "dev" and updater.Run short-circuits,
# so Toot never announces a new release. Fall back to "dev" only when git is
# unavailable (e.g. a tarball install).
OTTO_VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
echo ""
echo "  Building otto binary (version=$OTTO_VERSION)..."
go build -ldflags "-X main.version=$OTTO_VERSION" -o "$OTTO_BIN" ./cmd/otto
echo "  [ok] $OTTO_BIN"

OTTO_MEMORY_BIN="$OTTO_BIN_DIR/otto-memory"
echo "  Building otto-memory MCP server..."
go build -ldflags "-X main.version=$OTTO_VERSION" -o "$OTTO_MEMORY_BIN" ./cmd/otto-memory
echo "  [ok] $OTTO_MEMORY_BIN"

# Memory storage locations (live under the state dir, alongside session ids).
OTTO_MEMORY_DIR="$OTTO_STATE_DIR/memory"
OTTO_STATE_DB="$OTTO_STATE_DIR/state.db"
mkdir -p "$OTTO_MEMORY_DIR"

# ── Deployment mode ─────────────────────────────────────────────────────────
# Two very different installs share this script:
#
#   server  — a machine with no screen or speakers. Otto lives on Telegram.
#             Voice notes still work (they need whisper + ffmpeg, not a mic),
#             but there is nothing to install piper or sox for.
#   desktop — a machine you sit at. Everything above plus the microphone,
#             speakers and `otto tui`.
#
# Asking up front means the rest of the script installs exactly what this
# machine can use, instead of downloading ~100 MB of voice models onto a
# headless box that will never play a sound.
#
# OTTO_MODE can be preset in the environment for unattended installs.
if [ -z "${OTTO_MODE:-}" ]; then
  # Guess from the environment so the default is almost always right: a
  # graphical session means a desktop.
  if [ -n "${WAYLAND_DISPLAY:-}${DISPLAY:-}" ] || [ "$OS" = Darwin ]; then
    OTTO_MODE_DEFAULT=desktop
  else
    OTTO_MODE_DEFAULT=server
  fi

  echo ""
  echo "  ───────────────────────────────────────────────"
  echo "    How will you use Otto on this machine?"
  echo "  ───────────────────────────────────────────────"
  echo ""
  echo "    1) Home server — headless, always on."
  echo "       Otto answers Telegram forever. Voice notes work."
  echo "       No microphone or speakers needed."
  echo ""
  echo "    2) Desktop — you sit at this machine."
  echo "       Everything above, plus 'otto tui': a wake word,"
  echo "       spoken replies, and the lightbulb."
  echo ""
  if [ "$OTTO_MODE_DEFAULT" = desktop ]; then
    echo "    Detected a graphical session, so 2 is probably right."
  else
    echo "    No display detected, so 1 is probably right."
  fi
  read -r -p "  Choose [1/2] (enter for the suggestion): " OTTO_MODE_CHOICE
  case "$OTTO_MODE_CHOICE" in
    1) OTTO_MODE=server ;;
    2) OTTO_MODE=desktop ;;
    *) OTTO_MODE="$OTTO_MODE_DEFAULT" ;;
  esac
fi
case "$OTTO_MODE" in
  server)  echo "  [ok] Installing for a headless home server." ;;
  desktop) echo "  [ok] Installing for a desktop, with voice." ;;
  *) echo "  [!] Unknown OTTO_MODE=$OTTO_MODE; treating as desktop."; OTTO_MODE=desktop ;;
esac

# ── Ollama: local embeddings for semantic memory search (optional) ──────────
# When Ollama + an embedding model are present, session_search uses semantic
# retrieval; otherwise it degrades cleanly to keyword (FTS5) search. The whole
# block is best-effort — failures only disable semantic search, never abort.
OTTO_EMBED_URL="http://localhost:11434"
OTTO_EMBED_MODELS="embeddinggemma,nomic-embed-text"
echo ""
echo "  Setting up Ollama for semantic memory (optional)..."
if ! command -v ollama &>/dev/null; then
  case "$PKG_MGR" in
    pacman) sudo pacman -S --needed --noconfirm ollama || echo "  [!] ollama install failed; memory search will use keyword only" ;;
    brew)   brew install ollama || echo "  [!] ollama install failed; memory search will use keyword only" ;;
  esac
fi
if command -v ollama &>/dev/null; then
  # Ensure the server is running so models can be pulled.
  if [ "$OS" = Linux ]; then
    sudo systemctl enable --now ollama 2>/dev/null || true
  else
    brew services start ollama 2>/dev/null || { pgrep -x ollama >/dev/null 2>&1 || (ollama serve >/dev/null 2>&1 &); }
  fi
  # Wait briefly for the HTTP API before pulling.
  for _ in $(seq 1 10); do
    curl -fsS "$OTTO_EMBED_URL/api/tags" >/dev/null 2>&1 && break
    sleep 1
  done
  for m in embeddinggemma nomic-embed-text; do
    if ollama list 2>/dev/null | grep -q "^$m"; then
      echo "  [ok] embedding model present: $m"
    else
      echo "  Pulling embedding model: $m (first time may take a few minutes)..."
      ollama pull "$m" || echo "  [!] pull $m failed; continuing (keyword fallback)"
    fi
  done
  echo "  [ok] Ollama configured for semantic memory"
  # The Claude-outage backstop. Deliberately NOT pulled automatically: the
  # model is ~13 GB, which is a different order of download from the embedding
  # models above, and Otto works fine without it — a Claude failure simply
  # surfaces as an error the way it always did.
  if ollama list 2>/dev/null | grep -q "^gpt-oss"; then
    echo "  [ok] Claude-outage fallback model present: gpt-oss:20b"
  else
    echo "  [note] No fallback model. If Claude Code goes down, Otto has nothing to answer with."
    echo "         To arm the local backstop (~13 GB):  ollama pull gpt-oss:20b"
  fi
else
  echo "  [note] Ollama not installed — memory search will use keyword (FTS5) only,"
  echo "         and there is no local fallback when Claude Code is unreachable."
fi

# ── Voice: local speech-to-text and text-to-speech (optional) ───────────────
# Otto listens and speaks entirely on this machine — whisper.cpp for STT, piper
# for TTS. Same reasoning as Ollama above: no API key, no per-token cost,
# nothing leaves the box.
#
# What gets installed depends on OTTO_MODE. A headless server needs whisper and
# ffmpeg (Telegram voice notes arrive as OGG/Opus files, which need no audio
# hardware at all) but has no use for a microphone, speakers or piper.
#
# Only system binaries are installed here, because those need a package
# manager. The models and the piper binary are plain downloads, fetched by the
# Go binary on the first `otto tui` run with visible progress — so a machine
# that skipped this block still works.
#
# Best-effort throughout: nothing here can abort setup. Without any of it Otto
# is exactly the bot he was before — text only.
echo ""
echo "  Setting up local voice..."

# ffmpeg decodes Telegram's OGG/Opus voice notes. Needed in BOTH modes: a
# headless server has no microphone but still receives voice notes from a phone.
VOICE_PKGS=(ffmpeg)
if [ "$OTTO_MODE" = desktop ]; then
  # sox captures the microphone. Only useful where one exists.
  VOICE_PKGS+=(sox)
fi

for tool in "${VOICE_PKGS[@]}"; do
  if command -v "$tool" &>/dev/null; then
    echo "  [ok] $tool present"
    continue
  fi
  case "$PKG_MGR" in
    pacman) sudo pacman -S --needed --noconfirm "$tool" || echo "  [!] $tool install failed" ;;
    brew)   brew install "$tool" || echo "  [!] $tool install failed" ;;
  esac
done

# whisper.cpp is the one dependency no package manager reliably provides. It is
# NOT in Arch's official repos — only the AUR — so `pacman -S whisper.cpp`
# always fails, and telling the user to "install it from the AUR" leaves them
# stuck if they have no AUR helper. So try every route and, failing all of
# them, build it: it is a small C++ project and the build is genuinely quick.
#
# The CLI is `whisper-cli` on current versions and `whisper` on older ones;
# probe for either before doing anything.
install_whisper() {
  if command -v whisper-cli &>/dev/null || command -v whisper &>/dev/null; then
    echo "  [ok] whisper CLI present ($(command -v whisper-cli 2>/dev/null || command -v whisper))"
    return 0
  fi

  # 1. The package manager, in case it ever lands in a repo.
  case "$PKG_MGR" in
    brew)   brew install whisper-cpp 2>/dev/null && { echo "  [ok] installed whisper-cpp via brew"; return 0; } ;;
    pacman) sudo pacman -S --needed --noconfirm whisper.cpp 2>/dev/null && { echo "  [ok] installed whisper.cpp via pacman"; return 0; } ;;
  esac

  # 2. An AUR helper, if one happens to be installed.
  if [ "$PKG_MGR" = pacman ]; then
    for helper in yay paru; do
      if command -v "$helper" &>/dev/null; then
        echo "  Installing whisper.cpp from the AUR with $helper..."
        "$helper" -S --needed --noconfirm whisper.cpp 2>/dev/null \
          && { echo "  [ok] installed whisper.cpp from the AUR"; return 0; }
        echo "  [!] $helper could not install it; falling back to a source build."
        break
      fi
    done
  fi

  # 3. Build it. Statically linked, so the result is one self-contained file
  #    that can simply be dropped in ~/.local/bin with nothing else to install.
  echo ""
  echo "  whisper.cpp isn't available from a package manager on this system."
  echo "  It can be built from source — a few minutes, no lasting dependencies."
  read -r -p "  Build it now? [Y/n] " BUILD_WHISPER
  case "$BUILD_WHISPER" in
    [Nn]*)
      echo "  [skip] no speech-to-text. Voice notes and 'otto tui' will decline politely."
      echo "         Install it later, then re-run ./setup.sh."
      return 1 ;;
  esac

  for tool in git cmake; do
    command -v "$tool" &>/dev/null || { echo "  [!] $tool is required to build whisper.cpp"; return 1; }
  done

  WHISPER_SRC="$OTTO_STATE_DIR/whisper.cpp"
  echo "  Fetching source into $WHISPER_SRC..."
  rm -rf "$WHISPER_SRC"
  git clone --depth 1 https://github.com/ggerganov/whisper.cpp "$WHISPER_SRC" 2>&1 | sed 's/^/    /' || {
    echo "  [!] clone failed"; return 1; }

  echo "  Building (this is the slow part)..."
  # BUILD_SHARED_LIBS=OFF is what makes the result a single self-contained
  # binary; a shared build would need libwhisper/libggml installed alongside it
  # and would break the moment the source tree is deleted.
  cmake -S "$WHISPER_SRC" -B "$WHISPER_SRC/build" \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_SHARED_LIBS=OFF \
        -DWHISPER_BUILD_TESTS=OFF >/dev/null 2>&1 || { echo "  [!] cmake configure failed"; return 1; }
  cmake --build "$WHISPER_SRC/build" --config Release \
        -j"$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)" >/dev/null 2>&1 || {
    echo "  [!] build failed. Try it by hand to see the error:"
    echo "        cmake --build $WHISPER_SRC/build --config Release"
    return 1; }

  if [ ! -x "$WHISPER_SRC/build/bin/whisper-cli" ]; then
    echo "  [!] build finished but produced no whisper-cli"; return 1
  fi
  install -m 755 "$WHISPER_SRC/build/bin/whisper-cli" "$OTTO_BIN_DIR/whisper-cli"
  # The binary is static, so the ~1 GB source tree has no further purpose.
  rm -rf "$WHISPER_SRC"
  echo "  [ok] built and installed $OTTO_BIN_DIR/whisper-cli"
}
install_whisper || true

if [ "$OTTO_MODE" = desktop ]; then
  # Playback. sox's `play` always works as a fallback, so this only matters on
  # a desktop where paplay/aplay give better latency and mixer behaviour.
  if command -v paplay &>/dev/null || command -v aplay &>/dev/null \
    || command -v play &>/dev/null || command -v afplay &>/dev/null; then
    echo "  [ok] audio playback available"
  else
    case "$PKG_MGR" in
      pacman) sudo pacman -S --needed --noconfirm libpulse || echo "  [!] no audio player — install libpulse (paplay) or alsa-utils (aplay)" ;;
      brew)   : ;; # afplay ships with macOS
    esac
  fi

  # Pre-fetch the models now rather than on first launch. The whole point of
  # this script is that the machine is ready when it finishes; a half-gigabyte
  # download the first time you say "otto" is not that.
  echo ""
  echo "  Downloading voice models (~500 MB, one time)..."
  if "$OTTO_BIN" voice-fetch 2>&1 | sed 's/^/  /'; then
    echo "  [ok] voice models ready"
  else
    echo "  [!] model download incomplete — 'otto tui' will retry on first run"
  fi
else
  echo "  [ok] server mode: skipping microphone, speakers and voice models."
  echo "       Telegram voice notes still work (whisper + ffmpeg above)."
fi

echo ""
echo "  Voice check:"
"$OTTO_BIN" voice-doctor 2>&1 | sed 's/^/  /' || true

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
  read -r -p "  Drag the downloaded JSON file here: " GCAL_JSON
  GCAL_JSON=$(echo "$GCAL_JSON" | tr -d "'\"" | sed 's/^ *//;s/ *$//')
  # Terminal/iTerm backslash-escape spaces and other specials when a file is
  # dragged in (e.g. `My\ Downloads`, `client_secret\ \(1\).json`). read -r
  # keeps those backslashes literally, so strip a backslash before any char to
  # recover the real path.
  GCAL_JSON=$(printf '%s' "$GCAL_JSON" | sed 's/\\\(.\)/\1/g')
  # Expand a leading ~ to $HOME so paths typed as ~/… work correctly.
  # Double-quotes suppress shell tilde expansion, so we do it explicitly.
  GCAL_JSON="${GCAL_JSON/#\~/$HOME}"
  if [ ! -f "$GCAL_JSON" ]; then
    echo "  Can't find that file."; exit 1
  fi
  GCAL_TYPE=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print('installed' if 'installed' in d else 'web' if 'web' in d else 'unknown')" "$GCAL_JSON")
  if [ "$GCAL_TYPE" != installed ]; then
    echo "  Wrong client type ($GCAL_TYPE) — must be Desktop application."; exit 1
  fi
  install -m 600 "$GCAL_JSON" "$CLIENT_SECRET_FILE"
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

# Extract the Desktop OAuth client credentials for the Drive and Calendar MCP
# servers.  These values are written verbatim into mcp.json (chmod 600) as
# environment variables passed to the npx processes.  This is a pragmatic
# tradeoff for a single-user personal bot: the file is owner-readable only.
# A more hardened approach would reference CLIENT_SECRET_FILE directly in
# each MCP server config (so the secret never lands in mcp.json) or store the
# values in macOS Keychain and inject them via launchd EnvironmentVariables.
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
  GOOGLE_OAUTH_CREDENTIALS="$CLIENT_SECRET_FILE" npx --ignore-scripts -y "@cocal/google-calendar-mcp@$MCP_VER_GCAL" auth
  # Verify the OAuth dance completed — the MCP package writes a tokens file on
  # success.  Some CLI auth helpers exit 0 even when the browser flow was
  # cancelled, so we check the artifact rather than the exit code.
  if [ ! -f "$GCAL_TOKENS_PATH" ]; then
    echo "  [!] Calendar token not found — auth may have failed."
    echo "      Rerun ./setup.sh to retry."
    exit 1
  fi
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
    npx --ignore-scripts -y "mcp-gdrive-workspace@$MCP_VER_GDRIVE" > "$AUTH_LOG" 2>&1 &
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
read -r -p "  > " GMAIL_INPUT

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
    npx --ignore-scripts -y "@gongrzhe/server-gmail-autoauth-mcp@$MCP_VER_GMAIL" auth
  # The auth helper can exit 0 even if the browser flow was cancelled, so we
  # check the credentials artifact rather than the exit code (mirrors the
  # calendar/Drive steps).
  if [ ! -f "$CRED_PATH" ]; then
    echo "  [!] gmail-${label} credentials not written — auth may have been cancelled."
    echo "      Rerun ./setup.sh to retry."
    exit 1
  fi
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
  if [ -n "${NOTION_STALE_REASON:-}" ]; then
    echo "  The token already in config.toml no longer works:"
    echo "    $NOTION_STALE_REASON"
    echo ""
  fi
  # Notion is optional: Otto loses the Notion MCP without it but is otherwise
  # fine, so a user who cannot get one can decline and carry on.
  if prompt_validated secret validate_notion_token "  Paste your Notion token: "; then
    NOTION_TOKEN="$PROMPT_RESULT"
  else
    NOTION_TOKEN=""
    echo "  [skip] continuing without Notion — re-run ./setup.sh to add it later."
  fi
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

  Your user ID is detected automatically — no second bot needed.

STEP
  if [ -n "${TELEGRAM_STALE_REASON:-}" ]; then
    echo "  The token already in config.toml no longer works:"
    echo "    $TELEGRAM_STALE_REASON"
    echo ""
  fi

  # Checked against getMe before it is written anywhere. The prompt is silent,
  # so a truncated paste is invisible — verifying here is the only way the user
  # ever learns they mistyped it.
  if ! prompt_validated secret validate_telegram_token "  Paste bot token: "; then
    echo "  Can't continue without a working bot token."
    exit 1
  fi
  TELEGRAM_BOT_TOKEN="$PROMPT_RESULT"

  # Read the user ID off a real message rather than asking them to copy a
  # number from @userinfobot. The id that arrives here is by construction the
  # one that will be on their messages, so the allowlist cannot be subtly
  # wrong — and a wrong allowlist is a miserable bug, because Otto silently
  # drops every message and simply looks dead.
  TELEGRAM_USER_ID=""
  if detect_telegram_user_id "$TELEGRAM_BOT_TOKEN"; then
    TELEGRAM_USER_ID="$DETECTED_USER_ID"
    echo "  [ok] got it${DETECTED_USER_NAME:+ — hello, $DETECTED_USER_NAME} (id $TELEGRAM_USER_ID)"
  else
    # Assigned first rather than inlined as ${VAR:-...}: an apostrophe inside
    # that expansion is parsed as an opening quote even within double quotes.
    DETECT_FAIL="${VALIDATION_ERROR:-no message arrived}"
    echo "  [!] $DETECT_FAIL"
    echo "      Message @userinfobot on Telegram; it replies with your numeric ID."
    echo ""
    if ! prompt_validated plain validate_telegram_user_id "  Paste your user ID: "; then
      echo "  Can't continue without a user ID — Otto would ignore every message."
      exit 1
    fi
    TELEGRAM_USER_ID="$PROMPT_RESULT"
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
write_toml_field memory_dir "$OTTO_MEMORY_DIR" "$CONFIG_FILE"
write_toml_field state_db_path "$OTTO_STATE_DB" "$CONFIG_FILE"

# System prompt: SYSTEM.md is the source of truth. Every setup.sh run copies
# it to ~/.config/otto/system_prompt.md, overwriting any previous version.
# Edit SYSTEM.md (in the repo) → re-run ./setup.sh → restart otto.
if [ -f "$DIR/SYSTEM.md" ]; then
  cp "$DIR/SYSTEM.md" "$SYSTEM_PROMPT_FILE"
  chmod 600 "$SYSTEM_PROMPT_FILE"
  echo "  [ok] Synced system prompt: $DIR/SYSTEM.md → $SYSTEM_PROMPT_FILE"
fi
[ -f "$SYSTEM_PROMPT_FILE" ] && write_toml_field system_prompt_path "$SYSTEM_PROMPT_FILE" "$CONFIG_FILE"

# Toto persona: TOTO.md is the source of truth for the cat-themed fallback
# persona. Same overwrite-on-every-run pattern as SYSTEM.md.
if [ -f "$DIR/TOTO.md" ]; then
  cp "$DIR/TOTO.md" "$TOTO_PERSONA_FILE"
  chmod 600 "$TOTO_PERSONA_FILE"
  echo "  [ok] Synced toto persona: $DIR/TOTO.md → $TOTO_PERSONA_FILE"
fi
[ -f "$TOTO_PERSONA_FILE" ] && write_toml_field toto_persona_path "$TOTO_PERSONA_FILE" "$CONFIG_FILE"
write_toml_field toto_session_id_path "$OTTO_STATE_DIR/toto_session_id" "$CONFIG_FILE"

# Toot persona: TOOT.md is the source of truth for the owl-themed
# release-notes courier. Same overwrite-on-every-run pattern as
# SYSTEM.md / TOTO.md.
if [ -f "$DIR/TOOT.md" ]; then
  cp "$DIR/TOOT.md" "$TOOT_PERSONA_FILE"
  chmod 600 "$TOOT_PERSONA_FILE"
  echo "  [ok] Synced toot persona: $DIR/TOOT.md → $TOOT_PERSONA_FILE"
fi
[ -f "$TOOT_PERSONA_FILE" ] && write_toml_field toot_persona_path "$TOOT_PERSONA_FILE" "$CONFIG_FILE"
write_toml_field toot_session_id_path "$OTTO_STATE_DIR/toot_session_id" "$CONFIG_FILE"

# ── Write mcp.json ──────────────────────────────────────────────────────────
# If notion_api_key was set in a previous run, read it back so we can write
# mcp.json without prompting again.
EXISTING_NOTION="$(CONFIG_FILE="$CONFIG_FILE" python3 - <<'PYEOF'
import json, os, re
try:
    with open(os.environ["CONFIG_FILE"]) as f:
        data = f.read()
except FileNotFoundError:
    data = ""
m = re.search(r'^notion_api_key *= *(.*)$', data, re.MULTILINE)
if m:
    raw = m.group(1).strip()
    try:
        print(json.loads(raw), end="")
    except Exception:
        print(raw.strip('"'), end="")
PYEOF
)"
[ -z "$NOTION_TOKEN" ] && NOTION_TOKEN="$EXISTING_NOTION"

# Pre-create mcp.json with owner-only perms so the OAuth/Notion secrets are
# never world-readable during the write window (the > redirect below truncates
# without changing the mode). Mirrors the config.toml touch+chmod pattern.
install -m 600 /dev/null "$MCP_FILE"

NOTION_TOKEN_VAL="$NOTION_TOKEN" \
CLIENT_SECRET_FILE="$CLIENT_SECRET_FILE" \
DESKTOP_CLIENT_ID="$DESKTOP_CLIENT_ID" \
DESKTOP_CLIENT_SECRET="$DESKTOP_CLIENT_SECRET" \
GMAIL_OAUTH_PATH="$GMAIL_OAUTH_PATH" \
OTTO_MEMORY_BIN="$OTTO_MEMORY_BIN" \
OTTO_MEMORY_DIR="$OTTO_MEMORY_DIR" \
OTTO_STATE_DB="$OTTO_STATE_DB" \
OTTO_EMBED_URL="$OTTO_EMBED_URL" \
OTTO_EMBED_MODELS="$OTTO_EMBED_MODELS" \
MCP_VER_NOTION="$MCP_VER_NOTION" \
MCP_VER_GCAL="$MCP_VER_GCAL" \
MCP_VER_GDRIVE="$MCP_VER_GDRIVE" \
MCP_VER_GMAIL="$MCP_VER_GMAIL" \
HOME_DIR="$HOME" \
python3 - "${GMAIL_LABELS[@]}" > "$MCP_FILE" <<'PYEOF'
import json, os, sys
home = os.environ['HOME_DIR']
labels = sys.argv[1:]
config = {"mcpServers": {}}
config["mcpServers"]["otto-memory"] = {
    "command": os.environ['OTTO_MEMORY_BIN'],
    "args": [
        "--memory-dir", os.environ['OTTO_MEMORY_DIR'],
        "--state-db", os.environ['OTTO_STATE_DB'],
        "--embed-url", os.environ['OTTO_EMBED_URL'],
        "--embed-models", os.environ['OTTO_EMBED_MODELS'],
    ],
}
config["mcpServers"]["notion"] = {
    "command": "npx",
    # Two layers of supply-chain hardening on every community server below:
    # the exact version is pinned (MCP_VER_* in setup.sh — otherwise each bot
    # restart re-resolves "latest" and silently runs whatever the registry
    # serves), and --ignore-scripts blocks npm lifecycle hooks at install.
    # These processes receive live OAuth client credentials via their env.
    "args": ["--ignore-scripts", "-y", f"@notionhq/notion-mcp-server@{os.environ['MCP_VER_NOTION']}"],
    "env": {"NOTION_TOKEN": os.environ.get('NOTION_TOKEN_VAL', '')},
}
config["mcpServers"]["google-calendar"] = {
    "command": "npx",
    "args": ["--ignore-scripts", "-y", f"@cocal/google-calendar-mcp@{os.environ['MCP_VER_GCAL']}"],
    "env": {"GOOGLE_OAUTH_CREDENTIALS": os.environ['CLIENT_SECRET_FILE']},
}
config["mcpServers"]["gdrive"] = {
    "command": "npx",
    "args": ["--ignore-scripts", "-y", f"mcp-gdrive-workspace@{os.environ['MCP_VER_GDRIVE']}"],
    "env": {
        "GOOGLE_CLIENT_ID": os.environ['DESKTOP_CLIENT_ID'],
        "GOOGLE_CLIENT_SECRET": os.environ['DESKTOP_CLIENT_SECRET'],
    },
}
for label in labels:
    config["mcpServers"][f"gmail-{label}"] = {
        "command": "npx",
        "args": ["--ignore-scripts", "-y", f"@gongrzhe/server-gmail-autoauth-mcp@{os.environ['MCP_VER_GMAIL']}"],
        "env": {
            "GMAIL_OAUTH_PATH": os.environ['GMAIL_OAUTH_PATH'],
            "GMAIL_CREDENTIALS_PATH": f"{home}/.gmail-mcp/credentials-{label}.json",
        },
    }
print(json.dumps(config, indent=2))
PYEOF
chmod 600 "$MCP_FILE"

# ── Seed the curated memory core ────────────────────────────────────────────
# A fresh install starts with an empty Tier-1 core, so Otto knows nothing
# about his own deployment until the user happens to tell him. Seed MEMORY.md
# with environment facts setup.sh has just established first-hand, and offer
# to seed USER.md with a name.
#
# Strictly additive and idempotent: a file that already exists and is
# non-empty is NEVER touched, so re-running setup.sh can't clobber memory
# Otto has curated since. Both files are one-fact-per-line with no headers —
# that is the format internal/memory parses, dedupes and caps against; a
# markdown heading here would burn cap and confuse line-based dedup.
#
# Content constraints (see internal/memory/scan.go): no credentials, no
# newlines within an entry. These lines are injected into EVERY prompt for
# all three personas, so keep them few and dense.
seed_memory_file() {
  # $1 = path, remaining args = one entry per line
  local path="$1"; shift
  [ -s "$path" ] && return 0
  printf '%s\n' "$@" > "$path"
  chmod 600 "$path"
  echo "  [ok] seeded $(basename "$path")"
}

echo ""
echo "  Seeding memory core..."

if [ "$OS" = Linux ]; then
  OTTO_SERVICE_DESC="a systemd --user service (otto.service)"
else
  OTTO_SERVICE_DESC="a launchd user agent (com.otto.bot)"
fi

# The MCP server names exactly as written into mcp.json above.
MCP_NAMES="otto-memory, notion, google-calendar, gdrive"
for label in "${GMAIL_LABELS[@]}"; do
  MCP_NAMES="$MCP_NAMES, gmail-$label"
done

seed_memory_file "$OTTO_MEMORY_DIR/MEMORY.md" \
  "Otto runs on $OS as $OTTO_SERVICE_DESC, installed from the Otto repo by setup.sh." \
  "Otto's config is in ~/.config/otto/; his memory files, session ids and state.db are in ~/.local/state/otto/." \
  "Scheduled scripts and automations Otto writes belong in ~/.config/otto/scripts/, never inside the Otto source repository." \
  "Otto's connected MCP servers are: $MCP_NAMES."

# USER.md: only seeded if the user offers a name. An empty file is left
# absent rather than filled with a placeholder — every line here costs tokens
# on every single turn, so "(nothing recorded yet)" would be pure overhead.
if [ ! -s "$OTTO_MEMORY_DIR/USER.md" ]; then
  echo ""
  echo "  Otto keeps a short profile of you that he reads on every message."
  echo "  He'll learn more as you talk; this is just a first line."
  read -r -p "  What should Otto call you? (enter to skip): " OTTO_USER_NAME
  # Strip characters that would break the one-fact-per-line format or trip
  # the memory security scan.
  OTTO_USER_NAME="$(printf '%s' "$OTTO_USER_NAME" | tr -d '\n\r' | cut -c1-60)"
  if [ -n "$OTTO_USER_NAME" ]; then
    seed_memory_file "$OTTO_MEMORY_DIR/USER.md" \
      "The user's name is $OTTO_USER_NAME."
  else
    echo "  [skip] USER.md — Otto will build it as you talk."
  fi
fi

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
    echo "  ═══════════════════════════════════════════════"
    echo "    Otto is running, and will stay running."
    echo "  ═══════════════════════════════════════════════"
    echo ""
    echo "  Lingering is enabled, so he survives logout and reboot."
    echo ""
    echo "  Try it now — send your Telegram bot:"
    echo "    hi"
    echo "    remember that I use fish, not bash"
    echo "    /status"
    echo ""
    echo "  Hold the mic button and speak — he transcribes it locally."
    echo ""
    if [ "$OTTO_MODE" = desktop ]; then
      echo "  For voice at this machine:"
      echo "    otto tui"
      echo ""
      echo "  Say \"otto\" and he answers out loud. He stops the background"
      echo "  service while the UI is open and starts it again when you quit,"
      echo "  so you never have to think about it."
      echo ""
    fi
    echo "  Logs:    journalctl --user -u otto -f"
    echo "  Status:  systemctl --user status otto"
    echo "  Health:  otto voice-doctor"
  else
    echo "  [!] otto did not start cleanly."
    echo "      Check: journalctl --user -u otto -n 50"
    exit 1
  fi
else
  # ── launchd user agent (macOS) ────────────────────────────────────────────
  LAUNCHD_DIR="$HOME/Library/LaunchAgents"
  LAUNCHD_PLIST="$LAUNCHD_DIR/com.otto.bot.plist"
  LAUNCHD_LABEL="com.otto.bot"
  LAUNCHD_TARGET="gui/$(id -u)/$LAUNCHD_LABEL"
  mkdir -p "$LAUNCHD_DIR" "$HOME/Library/Logs"

  # Substitute placeholders into the canonical template.
  sed -e "s|__OTTO_BIN__|$OTTO_BIN|g" \
      -e "s|__HOME__|$HOME|g" \
      "$DIR/launchd/com.otto.bot.plist" > "$LAUNCHD_PLIST"

  # Bootout any existing instance (ignore failure if not loaded). bootout is
  # asynchronous, so poll until the label is really gone before re-bootstrapping
  # — bootstrapping a label that is still unloading returns EIO (error 5).
  launchctl bootout "$LAUNCHD_TARGET" 2>/dev/null || true
  for _ in 1 2 3 4 5; do
    launchctl print "$LAUNCHD_TARGET" >/dev/null 2>&1 || break
    sleep 1
  done

  # A label left in launchd's disabled list (e.g. by an older uninstall or a
  # manual `launchctl bootout`) makes bootstrap fail with the otherwise-cryptic
  # "Bootstrap failed: 5: Input/output error", even with a valid plist and
  # binary. Clear that sticky disabled state first; `enable` is a harmless
  # no-op when the label is already enabled.
  launchctl enable "$LAUNCHD_TARGET" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$LAUNCHD_PLIST"
  launchctl kickstart -k "$LAUNCHD_TARGET"

  # Smoke test: wait briefly, then check status.
  # `launchctl print` exits 0 even for registered-but-crashed jobs, so we
  # parse the state field directly — the same way systemctl --user is-active
  # distinguishes running from stopped on the Linux path.
  sleep 3
  if launchctl print "$LAUNCHD_TARGET" 2>/dev/null | grep -q 'state = running'; then
    echo ""
    echo "  [ok] otto is running."
    echo "       Logs:    tail -f ~/Library/Logs/otto.log"
    echo "       Status:  launchctl print $LAUNCHD_TARGET"
    echo "       Restart: launchctl kickstart -k $LAUNCHD_TARGET"
    echo ""
    echo "  Send 'hi' to your Telegram bot to test."
  else
    echo "  [!] otto did not start cleanly."
    echo "      Check: tail -n 50 ~/Library/Logs/otto.log"
    exit 1
  fi
fi

# ── Done ────────────────────────────────────────────────────────────────────
echo ""
echo "  ╔══════════════════════════════════════════╗"
echo "  ║          Otto setup complete!            ║"
echo "  ╚══════════════════════════════════════════╝"
echo ""
echo "  Re-run ./setup.sh anytime to reconfigure or fix things."
echo ""
