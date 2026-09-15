#!/bin/sh
# CDAMP installer.
#
# Builds cdampd from source (no prebuilt binaries are published) and sets
# up a working local instance: a default config, a data directory, and a
# freshly generated signing-key passphrase. Safe to re-run: an existing
# checkout is updated in place, and an existing config/passphrase is never
# overwritten.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/bashful8/cdamp/main/install.sh | sh
#
# Environment overrides:
#   CDAMP_INSTALL_DIR   where source, config, and data live (default: ~/.cdamp)
#   CDAMP_BIN_DIR        where the cdampd binary is installed (default: ~/.local/bin)

set -eu

REPO_URL="https://github.com/bashful8/cdamp.git"
REPO_TARBALL="https://github.com/bashful8/cdamp/archive/refs/heads/main.tar.gz"

INSTALL_DIR="${CDAMP_INSTALL_DIR:-$HOME/.cdamp}"
BIN_DIR="${CDAMP_BIN_DIR:-$HOME/.local/bin}"
SRC_DIR="$INSTALL_DIR/src"
DATA_DIR="$INSTALL_DIR/data"
CONFIG_PATH="$INSTALL_DIR/cdampd.yaml"
ENV_PATH="$INSTALL_DIR/cdampd.env"

info() { printf '==> %s\n' "$1"; }
warn() { printf 'warning: %s\n' "$1" >&2; }
die()  { printf 'error: %s\n' "$1" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- 1. prerequisites -------------------------------------------------------

have go || die "Go was not found on PATH. Install it from https://go.dev/dl/ and re-run this script."
info "found $(go version)"

mkdir -p "$INSTALL_DIR" "$DATA_DIR" "$BIN_DIR"

# --- 2. get the source -------------------------------------------------------
#
# If this script is itself sitting inside a cdamp checkout (e.g. you already
# cloned the repo and are running ./install.sh directly), build from that
# checkout instead of fetching a second copy.

SCRIPT_PATH="$0"
case "$SCRIPT_PATH" in
  /*) SCRIPT_DIR=$(dirname "$SCRIPT_PATH") ;;
  *)  SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$SCRIPT_PATH")" 2>/dev/null && pwd) || SCRIPT_DIR="" ;;
esac

if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/go.mod" ] && grep -q '^module cdamp$' "$SCRIPT_DIR/go.mod" 2>/dev/null; then
  SRC_DIR="$SCRIPT_DIR"
  info "running from an existing cdamp checkout: $SRC_DIR"
elif [ -d "$SRC_DIR/.git" ]; then
  info "updating existing checkout in $SRC_DIR"
  git -C "$SRC_DIR" fetch --depth 1 origin main
  git -C "$SRC_DIR" reset --hard origin/main
elif [ -d "$SRC_DIR" ]; then
  info "using existing source directory (not git-managed): $SRC_DIR"
elif have git; then
  info "cloning $REPO_URL"
  git clone --depth 1 "$REPO_URL" "$SRC_DIR"
else
  have curl || die "neither git nor curl was found; install one of them and re-run."
  have tar || die "tar was not found; install it and re-run."
  info "git not found; downloading a source tarball instead"
  mkdir -p "$SRC_DIR"
  curl -fsSL "$REPO_TARBALL" | tar -xz -C "$SRC_DIR" --strip-components=1
fi

# --- 3. build -----------------------------------------------------------------

info "building cdampd (compiling from source — this may take a minute the first time)"
(
  cd "$SRC_DIR"
  go build -o "$BIN_DIR/cdampd" ./cmd/cdampd
)
info "installed: $BIN_DIR/cdampd"

# --- 4. first-run config -------------------------------------------------------

if [ ! -f "$CONFIG_PATH" ]; then
  info "writing a default config to $CONFIG_PATH"
  cat > "$CONFIG_PATH" <<CFG
# CDAMP daemon config. See $SRC_DIR/02-ARCHITECTURE.md's Config section
# (in the project source) for the full field reference.
#
# "domain" identifies this instance to the outside world and must match
# the TLS certificate of whatever reverse proxy sits in front of cdampd
# once you expose it beyond localhost. Change it before going live.
domain: localhost
listen_addr: "127.0.0.1:8443"
admin_bind_addr: "127.0.0.1:8444"
sqlite_path: "$DATA_DIR/cdampd.db"
signing_key_passphrase_env: "CDAMPD_KEY_PASSPHRASE"
rate_limit: { per_domain_rps: 5, burst: 20 }
retry_schedule: [1m, 5m, 30m, 2h, 12h]
message: { max_body_bytes: 262144, default_ttl: 168h }
archive_after: 2160h
key_rotation_grace: 720h
directory_cache_ttl: 1h
CFG
else
  info "config already exists at $CONFIG_PATH, leaving it untouched"
fi

if [ ! -f "$ENV_PATH" ]; then
  info "generating a signing-key passphrase"
  if have openssl; then
    passphrase=$(openssl rand -hex 32)
  elif [ -r /dev/urandom ]; then
    passphrase=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
  else
    die "neither openssl nor /dev/urandom is available to generate a passphrase; set CDAMPD_KEY_PASSPHRASE yourself."
  fi
  printf 'export CDAMPD_KEY_PASSPHRASE=%s\n' "$passphrase" > "$ENV_PATH"
  chmod 600 "$ENV_PATH"
else
  info "passphrase file already exists at $ENV_PATH, leaving it untouched"
fi

# --- 5. summary -----------------------------------------------------------------

need_path=0
case ":${PATH:-}:" in
  *":$BIN_DIR:"*) ;;
  *) need_path=1 ;;
esac

echo
echo "cdamp installed."
echo
echo "  binary  $BIN_DIR/cdampd"
echo "  config  $CONFIG_PATH"
echo "  data    $DATA_DIR"
echo
if [ "$need_path" = "1" ]; then
  echo "Add the binary directory to your PATH:"
  echo
  echo "    export PATH=\"$BIN_DIR:\$PATH\""
  echo
fi
echo "Start your instance:"
echo
echo "    . $ENV_PATH"
echo "    cdampd --config $CONFIG_PATH"
echo
echo "On first boot cdampd prints a one-time admin bootstrap credential —"
echo "save it, it is never shown again. Use it to create your first agent:"
echo
echo "    curl -X POST http://127.0.0.1:8444/admin/agents \\"
echo "      --cookie \"cdampd_admin=<the bootstrap credential>\" \\"
echo "      -d '{\"name\":\"myagent\"}'"
echo
echo "Before exposing this instance to the internet, edit domain/listen_addr"
echo "in $CONFIG_PATH and put a TLS-terminating reverse proxy in front of it —"
echo "see the project README for the full picture."
