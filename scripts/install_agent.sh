#!/usr/bin/env bash
# Install/update Prostometrics Agent and run it via systemd.
#
# Design goals (simplified, deterministic, minimal host impact):
# - Uses system Go if >= GO_MIN_VERSION.
# - Otherwise downloads a fixed temporary Go toolchain from dl.google.com, uses it to build, and deletes it.
# - NO apt-get fallback (no package install/remove; avoids modifying host package state).
# - Idempotent runtime: replaces the systemd unit and restarts the service.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/prostoteam/prostometrics-agent/main/scripts/install_agent.sh | sudo bash -s -- --workload my-host --verbose
#
# Optional env overrides:
#   GO_MIN_VERSION=1.23.0
#   GO_BOOTSTRAP_VERSION=1.23.12
#   BIN_NAME=prostometrics-agent
#   INSTALL_DIR=/usr/local/bin
#   SERVICE_NAME=prostometrics-agent
#   SYSTEMD_SCOPE=system|user
#   AGENT_ENV_FILE=/etc/prostometrics/agent.env
#   PROSTOMETRICS_API_KEY=123_xxx
#   PROSTOMETRICS_HOST_DEFAULT=prostometrics.ru
#   PROSTOMETRICS_VERSION=latest
#   GOFLAGS="-buildvcs=false"

set -euo pipefail
IFS=$'\n\t'

# Install/runtime defaults
BIN_NAME="${BIN_NAME:-prostometrics-agent}"
SERVICE_NAME="${SERVICE_NAME:-prostometrics-agent}"
SYSTEMD_SCOPE="${SYSTEMD_SCOPE:-}"
AGENT_ENV_FILE="${AGENT_ENV_FILE:-}"
PROSTOMETRICS_API_KEY="${PROSTOMETRICS_API_KEY:-}"

PROSTOMETRICS_HOST_DEFAULT="${PROSTOMETRICS_HOST_DEFAULT:-prostometrics.ru}"

# Build defaults
GO_MIN_VERSION="${GO_MIN_VERSION:-1.23.0}"
# Fixed bootstrap Go version to download if system Go is too old.
# Pick any known-good >= GO_MIN_VERSION.
GO_BOOTSTRAP_VERSION="${GO_BOOTSTRAP_VERSION:-1.23.12}"

PROSTOMETRICS_VERSION="${PROSTOMETRICS_VERSION:-latest}"
GOFLAGS="${GOFLAGS:--buildvcs=false}"

MODULE_PATH="github.com/prostoteam/prostometrics-agent/cmd/prostometrics-agent"
DEFAULT_BIN_NAME="prostometrics-agent"

# Installer parses this and forwards it to the agent.
WORKLOAD="${WORKLOAD:-}"

# Any other args are passed through to the agent verbatim.
AGENT_ARGS=()

err() { echo "prostometrics-install: $*" >&2; exit 1; }
have_cmd() { command -v "$1" >/dev/null 2>&1; }
need_cmd() { have_cmd "$1" || err "missing required command: $1"; }
is_root() { [ "$(id -u)" -eq 0 ]; }

usage() {
  cat <<EOF
Usage:
  $(basename "$0") [--workload <value>] [--system|--user] [<agent-args...>]

Options (installer):
  -w, --workload   Optional workload scope passed to agent at runtime
  --system         Install a system service (default when running as root)
  --user           Install a user service (default when running as non-root)
  --api-key-file   Path to env file with PROSTOMETRICS_API_KEY (scope default if omitted)
  -h, --help       Show this help

Anything else is treated as an agent argument and passed through, e.g.:
  $(basename "$0") --workload firstvds-proxy --verbose --interval=10s
EOF
}

trim_ws() { printf '%s' "$1" | tr -d '\r' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//'; }

download() {
  local url="$1" out="$2"
  url="$(trim_ws "$url")"
  [ -n "$url" ] || err "download called with empty URL"

  if have_cmd curl; then
    curl -fL --retry 3 --retry-delay 1 --connect-timeout 10 -sS "$url" -o "$out" || return 1
  elif have_cmd wget; then
    wget --tries=3 --timeout=10 -qO "$out" "$url" || return 1
  else
    err "need curl or wget"
  fi
}

version_ge() {
  # true if $1 >= $2 (semver-ish using sort -V)
  [ "$(printf '%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

# Verify downloaded file looks like gzip (tar.gz). If not, likely proxied/HTML.
is_gzip_file() {
  local f="$1"
  local magic
  magic="$(head -c 2 "$f" 2>/dev/null | od -An -tx1 | tr -d ' \n')"
  [ "$magic" = "1f8b" ]
}

ensure_linux_arch() {
  [ "$(uname -s)" = "Linux" ] || err "unsupported OS (Linux only)"
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) err "unsupported arch (amd64/arm64 only)" ;;
  esac
}

ensure_install_dir() {
  mkdir -p "$INSTALL_DIR"
  [ -d "$INSTALL_DIR" ] || err "install dir is not a directory: $INSTALL_DIR"
  [ -w "$INSTALL_DIR" ] || err "install dir is not writable: $INSTALL_DIR"
}

init_systemd_defaults() {
  if [ -z "$SYSTEMD_SCOPE" ]; then
    if is_root; then
      SYSTEMD_SCOPE="system"
    else
      SYSTEMD_SCOPE="user"
    fi
  fi

  case "$SYSTEMD_SCOPE" in
    system|user) ;;
    *) err "SYSTEMD_SCOPE must be 'system' or 'user'" ;;
  esac

  if [ "$SYSTEMD_SCOPE" = "system" ]; then
    INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
    UNIT_DIR="${UNIT_DIR:-/etc/systemd/system}"
    AGENT_ENV_FILE="${AGENT_ENV_FILE:-/etc/prostometrics/agent.env}"
    UNIT_WANTED_BY="multi-user.target"
  else
    INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
    UNIT_DIR="${UNIT_DIR:-$HOME/.config/systemd/user}"
    AGENT_ENV_FILE="${AGENT_ENV_FILE:-$HOME/.config/prostometrics/agent.env}"
    UNIT_WANTED_BY="default.target"
  fi

  BIN_PATH="$INSTALL_DIR/$BIN_NAME"
  SERVICE_FILE="$UNIT_DIR/$SERVICE_NAME.service"
  SYSTEMCTL_CMD=(systemctl)
  JOURNALCTL_CMD=(journalctl)
  if [ "$SYSTEMD_SCOPE" = "user" ]; then
    SYSTEMCTL_CMD+=(--user)
    JOURNALCTL_CMD+=(--user)
  fi
}

ensure_unit_dir() {
  mkdir -p "$UNIT_DIR"
  [ -d "$UNIT_DIR" ] || err "unit dir is not a directory: $UNIT_DIR"
}

is_valid_api_key() {
  local key="$1"
  [ -n "$key" ] || return 1
  return 0
}

sanitize_api_key() {
  local key="$1"
  key="$(printf '%s' "$key" | awk '{gsub(/\033\[[0-9;?]*[ -\/]*[@-~]/, ""); gsub(/\033[@-_]/, ""); printf "%s", $0}')"
  key="$(printf '%s' "$key" | LC_ALL=C tr -d '\000-\037\177')"
  key="$(printf '%s' "$key" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
  printf '%s' "$key"
}

validate_api_key_or_fail() {
  local key="$1"
  if ! is_valid_api_key "$key"; then
    err "invalid API key"
  fi
}

build_probe_url() {
  local host url
  host="$(trim_ws "$PROSTOMETRICS_HOST_DEFAULT")"
  [ -n "$host" ] || err "PROSTOMETRICS_HOST_DEFAULT is empty"

  if [[ "$host" == http://* || "$host" == https://* ]]; then
    url="$host"
  else
    url="https://$host"
  fi

  url="${url%/}"

  if [[ "$url" == */api/i/probe ]]; then
    printf '%s' "$url"
    return 0
  fi
  if [[ "$url" == */api/i/batch ]]; then
    printf '%s/api/i/probe' "${url%/api/i/batch}"
    return 0
  fi
  if [[ "$url" == */api/i ]]; then
    printf '%s/probe' "$url"
    return 0
  fi
  printf '%s/api/i/probe' "$url"
}

probe_api_key_status() {
  local url="$1"

  if have_cmd curl; then
    curl -sS -o /dev/null -w '%{http_code}' \
      --connect-timeout 5 \
      --max-time 10 \
      --request POST \
      --header "Authorization: $PROSTOMETRICS_API_KEY" \
      "$url"
    return $?
  fi

  if have_cmd wget; then
    wget --server-response --quiet \
      --method=POST \
      --header="Authorization: $PROSTOMETRICS_API_KEY" \
      --output-document=/dev/null \
      "$url" 2>&1 | awk '/^  HTTP\/[0-9.]+ [0-9]+/{code=$2} END{print code}'
    return 0
  fi

  return 1
}

verify_api_key_or_fail() {
  local probe_url status
  probe_url="$(build_probe_url)"
  printf 'prostometrics-install: checking API key...\n'
  status="$(probe_api_key_status "$probe_url")" || err "could not verify API key: network/probe request failed"

  if [ "$status" = "204" ]; then
    printf 'prostometrics-install: API key is valid.\n'
    return 0
  fi
  if [ "$status" = "401" ]; then
    err "API key is invalid or inactive"
  fi

  err "could not verify API key: probe returned HTTP ${status}"
}

prompt_api_key_from_tty() {
  local key
  [ -r /dev/tty ] || return 1
  [ -w /dev/tty ] || return 1
  printf "prostometrics-install: 👋🏻 Paste here the API key you generated on the /settings page:" > /dev/tty
  IFS= read -r -s key < /dev/tty || return 1
  printf "\n" > /dev/tty
  PROSTOMETRICS_API_KEY="$key"
  return 0
}

resolve_api_key() {
  if [ -n "$PROSTOMETRICS_API_KEY" ]; then
    PROSTOMETRICS_API_KEY="$(sanitize_api_key "$PROSTOMETRICS_API_KEY")"
    validate_api_key_or_fail "$PROSTOMETRICS_API_KEY"
    verify_api_key_or_fail
    return 0
  fi

  if prompt_api_key_from_tty; then
    PROSTOMETRICS_API_KEY="$(sanitize_api_key "$PROSTOMETRICS_API_KEY")"
    validate_api_key_or_fail "$PROSTOMETRICS_API_KEY"
    verify_api_key_or_fail
    return 0
  fi

  err "API key is required. Run interactively to be prompted, or set PROSTOMETRICS_API_KEY for non-interactive installs."
}

write_api_key_env_file() {
  local env_dir tmp_file old_umask
  env_dir="$(dirname "$AGENT_ENV_FILE")"
  mkdir -p "$env_dir"
  tmp_file="$TMPDIR/agent.env"
  old_umask="$(umask)"
  umask 077
  printf 'PROSTOMETRICS_API_KEY=%s\n' "$PROSTOMETRICS_API_KEY" > "$tmp_file"
  if have_cmd install; then
    install -m 0600 -T "$tmp_file" "$AGENT_ENV_FILE"
  else
    cp -f "$tmp_file" "$AGENT_ENV_FILE"
    chmod 0600 "$AGENT_ENV_FILE"
  fi
  umask "$old_umask"
}

use_system_go_if_ok() {
  have_cmd go || return 1
  local gv
  gv="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
  [ -n "${gv:-}" ] || return 1
  if version_ge "$gv" "$GO_MIN_VERSION"; then
    return 0
  fi
  echo "prostometrics-install: system Go $gv found, but $GO_MIN_VERSION+ is required; will use a temporary Go toolchain."
  return 1
}

setup_temp_go_or_fail() {
  need_cmd tar

  local arch fname url tgz
  arch="$(ensure_linux_arch)"

  fname="go${GO_BOOTSTRAP_VERSION}.linux-${arch}.tar.gz"
  url="https://dl.google.com/go/${fname}"
  tgz="$TMPDIR/go.tgz"

  echo "prostometrics-install: downloading temporary Go toolchain: ${fname}"
  download "$url" "$tgz" || err "failed to download ${url}"

  if ! is_gzip_file "$tgz"; then
    echo "prostometrics-install: downloaded content is not a tar.gz (likely blocked/proxied). First line:"
    head -n 1 "$tgz" | sed 's/^/  /'
    err "cannot obtain a valid Go toolchain"
  fi

  rm -rf "$TMPDIR/go"
  tar -C "$TMPDIR" -xzf "$tgz"
  [ -x "$TMPDIR/go/bin/go" ] || err "temporary Go install failed: $TMPDIR/go/bin/go not found"

  export GOROOT="$TMPDIR/go"
  export PATH="$GOROOT/bin:$PATH"

  go version >/dev/null 2>&1 || err "temporary Go toolchain is not runnable"

  # Ensure minimum satisfied (defensive)
  local gv
  gv="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
  if ! version_ge "$gv" "$GO_MIN_VERSION"; then
    err "temporary Go $gv is still < $GO_MIN_VERSION; set GO_BOOTSTRAP_VERSION accordingly"
  fi
}

install_agent() {
  local gobin_tmp final_tmp

  gobin_tmp="$TMPDIR/gobin"
  mkdir -p "$gobin_tmp"

  # Keep Go caches isolated (no user pollution).
  export GOPATH="$TMPDIR/gopath"
  export GOMODCACHE="$TMPDIR/gomodcache"
  export GOCACHE="$TMPDIR/gocache"
  mkdir -p "$GOPATH" "$GOMODCACHE" "$GOCACHE"

  echo "prostometrics-install: installing ${MODULE_PATH}@${PROSTOMETRICS_VERSION}"
  GOBIN="$gobin_tmp" GOFLAGS="$GOFLAGS" go install "${MODULE_PATH}@${PROSTOMETRICS_VERSION}"

  final_tmp="$gobin_tmp/$DEFAULT_BIN_NAME"
  [ -f "$final_tmp" ] || err "expected built binary not found: $final_tmp"

  if have_cmd install; then
    install -m 0755 -T "$final_tmp" "$BIN_PATH"
  else
    cp -f "$final_tmp" "$BIN_PATH"
    chmod 0755 "$BIN_PATH"
  fi

  echo "prostometrics-install: installed: $BIN_PATH"
}

systemd_quote() {
  local s="$1"
  if [[ "$s" =~ ^[A-Za-z0-9_./:@%+=,-]+$ ]]; then
    printf '%s' "$s"
    return 0
  fi
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/ }"
  s="${s//$'\r'/ }"
  s="${s//$'\t'/ }"
  printf '"%s"' "$s"
}

build_exec_start() {
  local args=() escaped cmd
  if [ -n "${WORKLOAD:-}" ]; then
    args+=( "--workload" "$WORKLOAD" )
  fi
  if [ "${#AGENT_ARGS[@]}" -gt 0 ]; then
    args+=( "${AGENT_ARGS[@]}" )
  fi

  cmd="$(systemd_quote "$BIN_PATH")"
  for arg in "${args[@]}"; do
    cmd+=" $(systemd_quote "$arg")"
  done
  printf '%s' "$cmd"
}

write_unit() {
  local exec_start host_env env_file
  exec_start="$(build_exec_start)"
  host_env="$(systemd_quote "PROSTOMETRICS_HOST=$PROSTOMETRICS_HOST_DEFAULT")"
  env_file="$(systemd_quote "$AGENT_ENV_FILE")"

  cat >"$SERVICE_FILE" <<EOF
[Unit]
Description=Prostometrics Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$exec_start
Environment=$host_env
EnvironmentFile=$env_file
Restart=always
RestartSec=2
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectControlGroups=true
ProtectKernelTunables=true
ProtectKernelModules=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=$UNIT_WANTED_BY
EOF
}

reload_systemd() {
  "${SYSTEMCTL_CMD[@]}" daemon-reload
}

enable_and_restart_service() {
  "${SYSTEMCTL_CMD[@]}" enable "$SERVICE_NAME"
  "${SYSTEMCTL_CMD[@]}" restart "$SERVICE_NAME"
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      -w|--workload)
        [ $# -ge 2 ] || err "missing value for $1"
        WORKLOAD="$2"
        shift 2
        ;;
      --system)
        SYSTEMD_SCOPE="system"
        shift
        ;;
      --user)
        SYSTEMD_SCOPE="user"
        shift
        ;;
      --api-key-file)
        [ $# -ge 2 ] || err "missing value for $1"
        AGENT_ENV_FILE="$2"
        shift 2
        ;;
      -h|--help)
        usage; exit 0
        ;;
      *)
        # Unknown to installer => pass through to agent
        AGENT_ARGS+=( "$1" )
        shift
        ;;
    esac
  done
}

main() {
  need_cmd uname
  need_cmd awk
  need_cmd sed
  need_cmd sort
  need_cmd head
  need_cmd mktemp
  need_cmd tar
  need_cmd systemctl

  parse_args "$@"
  init_systemd_defaults
  ensure_install_dir
  ensure_unit_dir

  TMPDIR="$(mktemp -d -t prostometrics-install.XXXXXX)"
  export TMPDIR

  trap 'chmod -R u+rwX "$TMPDIR" 2>/dev/null || true; rm -rf "$TMPDIR" 2>/dev/null || true' EXIT INT TERM

  if ! use_system_go_if_ok; then
    setup_temp_go_or_fail
  fi

  resolve_api_key
  install_agent
  write_api_key_env_file
  write_unit
  reload_systemd
  enable_and_restart_service

  cat <<EOF
prostometrics-install: done.

Binary:
  $BIN_PATH
Service:
  $SERVICE_NAME ($SYSTEMD_SCOPE)

Unit file:
  $SERVICE_FILE

Environment file:
  $AGENT_ENV_FILE

Default host:
  $PROSTOMETRICS_HOST_DEFAULT

To view status:
  $(printf '%q ' "${SYSTEMCTL_CMD[@]}")status $SERVICE_NAME

To view logs:
  $(printf '%q ' "${JOURNALCTL_CMD[@]}")-u $SERVICE_NAME -f

To stop:
  $(printf '%q ' "${SYSTEMCTL_CMD[@]}")stop $SERVICE_NAME
EOF

  if [ "$SYSTEMD_SCOPE" = "user" ]; then
    cat <<EOF

Note: user services start on boot only if lingering is enabled:
  loginctl enable-linger $USER
EOF
  fi
}

main "$@"
