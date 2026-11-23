#!/usr/bin/env bash
# BrewDat Evidence Collection: install, update and restart on RHEL 8 or 9 (also Rocky and Alma).
#
# First install (one command):
#   curl -fsSL https://raw.githubusercontent.com/black0405/brewdat-evidence-collection/main/deploy.sh | sudo ADMIN_EMAIL=you@company.com bash
# Every update after that (pulls the latest commit, tests, builds, restarts):
#   sudo /opt/brewdat/deploy.sh
#
# Optional variables: REPO_URL, BRANCH (main), ADMIN_EMAIL, BREWDAT_PORT (8080, first install only).
set -euo pipefail

# Everything lives in main so bash reads the whole script before running it.
# That keeps a half downloaded script from doing anything, and lets git replace this file mid run.
main() {
  local APP_DIR=/opt/brewdat DATA_DIR=/var/lib/brewdat ETC_DIR=/etc/brewdat CACHE_DIR=/var/cache/brewdat
  local ENV_FILE=/etc/brewdat/brewdat.env BIN=/usr/local/bin/brewdat UNIT=/etc/systemd/system/brewdat.service
  local BRANCH="${BRANCH:-main}" REPO_URL="${REPO_URL:-https://github.com/black0405/brewdat-evidence-collection.git}" ADMIN_EMAIL="${ADMIN_EMAIL:-admin@brewdat.local}"
  local GO_VERSION=1.27.4
  local GO_SHA_amd64=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840
  local GO_SHA_arm64=3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92
  local GO=/usr/local/go/bin/go

  [[ $EUID -eq 0 ]] || die "Run this as root, for example: sudo $0"
  . /etc/os-release
  [[ " ${ID:-} ${ID_LIKE:-} " =~ \ (rhel|centos|fedora)\  ]] || die "This script supports RHEL, Rocky and Alma Linux only."

  local ARCH
  case "$(uname -m)" in
    x86_64) ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
    *) die "Unsupported CPU architecture $(uname -m)." ;;
  esac
  local mem_kb
  mem_kb=$(awk '/^(MemTotal|SwapTotal):/ {s += $2} END {print s}' /proc/meminfo)
  (( mem_kb >= 1900000 )) || warn "Less than 2 GB of RAM plus swap. The first build may fail; add swap if it does."

  step "Installing system packages"
  local pkgs=(git tar ca-certificates policycoreutils)
  command -v curl &>/dev/null || pkgs+=(curl) # minimal images ship curl-minimal, which conflicts with curl
  dnf -y -q install "${pkgs[@]}" >/dev/null

  if [[ "$(cat /usr/local/go/VERSION 2>/dev/null | head -n1)" != "go$GO_VERSION" ]]; then
    step "Installing Go $GO_VERSION"
    local tgz=/tmp/go$GO_VERSION.tar.gz sha_var="GO_SHA_$ARCH"
    curl -fsSL -o "$tgz" "https://go.dev/dl/go$GO_VERSION.linux-$ARCH.tar.gz"
    echo "${!sha_var}  $tgz" | sha256sum -c --quiet || die "Go download checksum does not match."
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tgz"
    rm -f "$tgz"
  fi

  step "Fetching the latest code from $BRANCH"
  if [[ -d $APP_DIR/.git ]]; then
    [[ -n $REPO_URL ]] && git -C "$APP_DIR" remote set-url origin "$REPO_URL"
    git -C "$APP_DIR" fetch -q --depth 1 origin "$BRANCH"
    git -C "$APP_DIR" reset -q --hard FETCH_HEAD
    git -C "$APP_DIR" clean -qfd
  else
    [[ -n $REPO_URL ]] || die "Set REPO_URL to your GitHub repository, for example REPO_URL=https://github.com/black0405/brewdat-evidence-collection.git"
    git clone -q --depth 1 -b "$BRANCH" "$REPO_URL" "$APP_DIR"
  fi
  chmod 0755 "$APP_DIR/deploy.sh"
  local COMMIT
  COMMIT=$(git -C "$APP_DIR" log -1 --format='%h %s')

  step "Testing and building"
  mkdir -p "$CACHE_DIR"
  (
    cd "$APP_DIR"
    export CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath GOCACHE=$CACHE_DIR/build GOMODCACHE=$CACHE_DIR/mod
    "$GO" test ./...
    "$GO" build -ldflags "-s -w" -o "$BIN.new" .
  ) || die "Build or tests failed. The running service was not touched."

  step "Preparing user, folders and settings"
  id brewdat &>/dev/null || useradd --system --home-dir "$DATA_DIR" --shell /sbin/nologin brewdat
  install -d -o brewdat -g brewdat -m 0750 "$DATA_DIR" "$DATA_DIR/files" "$DATA_DIR/tmp" "$DATA_DIR/backup"
  install -d -o root -g brewdat -m 0750 "$ETC_DIR"
  if [[ ! -f $ENV_FILE ]]; then
    local port="${BREWDAT_PORT:-8080}"
    cat >"$ENV_FILE" <<EOF
# BrewDat Evidence Collection server settings. Apply changes with: sudo systemctl restart brewdat
# SMTP and the email schedule are set in the portal under Email and schedule.
BREWDAT_ADDR=:$port
BREWDAT_DATA=$DATA_DIR
BREWDAT_BASE_URL=http://$(hostname -f 2>/dev/null || cat /proc/sys/kernel/hostname):$port
# For HTTPS set both paths (and usually BREWDAT_ADDR=:443), files readable by group brewdat.
BREWDAT_TLS_CERT=
BREWDAT_TLS_KEY=
# Set to 1 when a load balancer or proxy terminates HTTPS in front of the portal.
BREWDAT_BEHIND_PROXY=0
BREWDAT_MAX_UPLOAD_MB=25
# Time zone used for the monthly schedule, for example TZ=Asia/Kolkata. Defaults to the server zone.
#TZ=
EOF
    chown root:brewdat "$ENV_FILE"
    chmod 0640 "$ENV_FILE"
  fi

  cat >"$UNIT" <<'EOF'
[Unit]
Description=BrewDat Evidence Collection
After=network-online.target
Wants=network-online.target

[Service]
User=brewdat
Group=brewdat
EnvironmentFile=/etc/brewdat/brewdat.env
Environment=GOMEMLIMIT=200MiB TMPDIR=/var/lib/brewdat/tmp
ExecStart=/usr/local/bin/brewdat
Restart=on-failure
RestartSec=3
LimitNOFILE=8192
UMask=0027
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/brewdat
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  mv -f "$BIN.new" "$BIN"

  if command -v selinuxenabled &>/dev/null && selinuxenabled; then
    step "Applying SELinux labels"
    restorecon -R "$BIN" "$DATA_DIR" "$ETC_DIR" >/dev/null
  fi

  local ADMIN_LINE=""
  if [[ ! -f $ETC_DIR/.admin_created ]]; then
    step "Creating the first admin"
    ADMIN_LINE=$(runuser -u brewdat -- env BREWDAT_DATA="$DATA_DIR" "$BIN" admin "$ADMIN_EMAIL")
    touch "$ETC_DIR/.admin_created"
  fi

  local PORT SCHEME=http
  PORT=$(sed -n 's/^BREWDAT_ADDR=.*:\([0-9][0-9]*\)$/\1/p' "$ENV_FILE")
  PORT=${PORT:-8080}
  grep -q '^BREWDAT_TLS_CERT=.\+' "$ENV_FILE" && SCHEME=https
  if systemctl is-active --quiet firewalld; then
    step "Opening port $PORT in firewalld"
    firewall-cmd -q --permanent --add-port="$PORT/tcp"
    firewall-cmd -q --reload
  fi

  step "Starting the service"
  systemctl enable -q brewdat
  systemctl restart brewdat
  local ok=0
  for _ in $(seq 1 30); do
    if curl -fsk -o /dev/null "$SCHEME://127.0.0.1:$PORT/healthz"; then ok=1; break; fi
    sleep 0.5
  done
  if (( ! ok )); then
    journalctl -u brewdat -n 40 --no-pager || true
    die "The service did not come up. The log is above."
  fi

  local URL
  URL=$(sed -n 's/^BREWDAT_BASE_URL=//p' "$ENV_FILE")
  printf '\n\033[1;33mBrewDat Evidence Collection is running\033[0m\n'
  printf '  Address:  %s\n  Version:  %s\n' "${URL:-$SCHEME://127.0.0.1:$PORT}" "$COMMIT"
  [[ -n $ADMIN_LINE ]] && printf '  %s\n  Sign in and choose your own password. This is shown only once.\n' "$ADMIN_LINE"
  printf '  Update later with: sudo %s/deploy.sh\n\n' "$APP_DIR"
}

step() { printf '\033[1;33m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;31mWarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31mStopped:\033[0m %s\n' "$*" >&2; exit 1; }

main "$@"
