#!/usr/bin/env bash
#
# Idempotent agent installer. Run from an operator workstation; it drives the
# target host over SSH.
#
#   deploy/install-agent.sh <ssh-target> <machine-id> <region> [metrics-ip]
#   deploy/install-agent.sh --check <ssh-target> <machine-id> <region>
#
#   deploy/install-agent.sh root@10.0.0.5 sys jp
#   deploy/install-agent.sh ems ori-1 jp
#
# Decides by comparing the installed binary's SHA256 against the one published
# in the release's SHA256SUMS:
#
#   no agent        -> install
#   checksum differs-> upgrade (stop, replace, start)
#   checksum matches-> skip (config drift is still reconciled)
#
# Checksums rather than version strings on purpose: internal/buildinfo states
# its values are for human display only and must not drive upgrade decisions.
# A checksum also catches a locally rebuilt binary wearing a released tag.
#
# Runs from the operator machine because the two things it needs must not live
# on trading hosts: the GitHub token that reads a private repo's release
# assets, and the CA private key that signs agent identities.
set -euo pipefail

REPO="BullionBear/strategon"
CP_URL="${STRATEGON_CP_URL:-https://100.108.10.2:8443}"
REMOTE_DIR="/opt/strategon-agent"
STATE_DIR="/var/lib/strategon-agent"
UNIT="/etc/systemd/system/strategon-agent.service"

cd "$(dirname "$0")/.."
CA_DIR="./ca"
CERT_ROOT="./certs"
CACHE="./.cache/agent-releases"

CHECK_ONLY=false
if [[ "${1:-}" == "--check" ]]; then
  CHECK_ONLY=true
  shift
fi

if [[ $# -lt 3 ]]; then
  sed -n '2,20p' "$0" | sed 's/^# \?//'
  exit 2
fi

TARGET="$1"
MACHINE_ID="$2"
REGION="$3"
METRICS_IP="${4:-}"

# A caller that word-splits badly (zsh does not split unquoted parameters)
# otherwise reaches the remote with an empty machine id and a target containing
# spaces, which fails much later and far less clearly.
for pair in "ssh-target:$TARGET" "machine-id:$MACHINE_ID" "region:$REGION"; do
  name="${pair%%:*}"; value="${pair#*:}"
  [[ -n "$value" ]] || { printf 'error: %s is empty\n' "$name" >&2; exit 2; }
  [[ "$value" == *[[:space:]]* ]] && { printf 'error: %s contains whitespace: %q\n' "$name" "$value" >&2; exit 2; }
done

log()  { printf '  %s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Optional OpenSSH config (written by deploy/install-agents.sh) so fleet
# installs can pass HostName/User/Port/IdentityFile without changing the
# <ssh-target> contract.
SSH_CFG=()
SCP_CFG=()
if [[ -n "${STRATEGON_SSH_CONFIG:-}" ]]; then
  [[ -f "$STRATEGON_SSH_CONFIG" ]] || fail "STRATEGON_SSH_CONFIG not a file: $STRATEGON_SSH_CONFIG"
  SSH_CFG=(-F "$STRATEGON_SSH_CONFIG")
  SCP_CFG=(-F "$STRATEGON_SSH_CONFIG")
fi
ssh_cmd() { ssh "${SSH_CFG[@]}" "$@"; }
scp_cmd() { scp "${SCP_CFG[@]}" "$@"; }

command -v gh >/dev/null || fail "gh CLI not found (needed to read private release assets)"

printf '\n=== %s (machine=%s region=%s) ===\n' "$TARGET" "$MACHINE_ID" "$REGION"

# ---------------------------------------------------------------- remote facts
# One round trip: everything needed to decide, before changing anything.
read -r REMOTE_USER ARCH REMOTE_SHA UNIT_HASH TS_IP USER_UNIT <<<"$(
  ssh_cmd -o BatchMode=yes "$TARGET" '
    printf "%s " "$(id -un)"
    case "$(uname -m)" in
      x86_64)  printf "amd64 " ;;
      aarch64) printf "arm64 " ;;
      *)       printf "unsupported " ;;
    esac
    if [ -f '"$REMOTE_DIR"'/agent ]; then
      printf "%s " "$(sha256sum '"$REMOTE_DIR"'/agent | cut -d" " -f1)"
    else
      printf "none "
    fi
    if [ -f '"$UNIT"' ]; then
      printf "%s " "$(sha256sum '"$UNIT"' | cut -d" " -f1)"
    else
      printf "none "
    fi
    printf "%s " "$(tailscale ip -4 2>/dev/null | head -1 || echo none)"
    if [ -f "$HOME/.config/systemd/user/strategon-agent.service" ]; then
      printf "user-unit\n"
    else
      printf "none\n"
    fi
  '
)" || fail "cannot reach $TARGET over SSH"

[[ "$ARCH" == "unsupported" ]] && fail "unsupported CPU architecture on $TARGET"

# A leftover user-level unit would keep running alongside the system unit this
# script installs, giving two agents the same machine id and two writers for
# one identity. Refuse rather than silently create that.
if [[ "$USER_UNIT" == "user-unit" ]]; then
  cat >&2 <<MSG
error: $TARGET already runs a user-level agent unit.
       Installing the system unit would start a second agent claiming
       machine id "$MACHINE_ID". Remove the old one first, as that user:

         systemctl --user disable --now strategon-agent.service
         rm ~/.config/systemd/user/strategon-agent.service
         rm -rf ~/strategon

       Then re-run this script.
MSG
  exit 1
fi

SUDO=""
[[ "$REMOTE_USER" != "root" ]] && SUDO="sudo"

if [[ -z "$METRICS_IP" ]]; then
  [[ "$TS_IP" == "none" ]] && fail "no Tailscale IP on $TARGET; pass metrics-ip explicitly"
  METRICS_IP="$TS_IP"
fi
log "remote: user=$REMOTE_USER arch=$ARCH metrics=$METRICS_IP"

# --------------------------------------------------------------- release asset
TAG="$(gh release view --repo "$REPO" --json tagName -q .tagName)" \
  || fail "no published release found in $REPO"
ASSET="agent-linux-${ARCH}"
DEST="$CACHE/$TAG"

# Cache by tag: re-running across a fleet downloads each asset once.
if [[ ! -f "$DEST/$ASSET" || ! -f "$DEST/SHA256SUMS" ]]; then
  mkdir -p "$DEST"
  gh release download "$TAG" --repo "$REPO" --pattern "$ASSET" --pattern SHA256SUMS \
    --dir "$DEST" --clobber >/dev/null
fi

WANT_SHA="$(awk -v a="$ASSET" '$2==a || $2=="*"a {print $1}' "$DEST/SHA256SUMS")"
[[ -n "$WANT_SHA" ]] || fail "$ASSET not listed in $TAG SHA256SUMS"

# The published checksum is only meaningful if the local copy still matches it.
GOT_SHA="$(shasum -a 256 "$DEST/$ASSET" 2>/dev/null | cut -d' ' -f1 \
           || sha256sum "$DEST/$ASSET" | cut -d' ' -f1)"
[[ "$GOT_SHA" == "$WANT_SHA" ]] || fail "$ASSET failed checksum verification (cache corrupt?)"
log "release: $TAG ($ASSET verified)"

# ------------------------------------------------------------------- unit file
# Rendered locally so its hash can be compared before touching the host.
render_unit() {
  cat <<EOF
[Unit]
Description=Strategon agent (machine ${MACHINE_ID})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=strategon
Group=strategon
ExecStart=${REMOTE_DIR}/agent \\
  --control-plane ${CP_URL} \\
  --tls-cert ${REMOTE_DIR}/tls/cert.pem \\
  --tls-key ${REMOTE_DIR}/tls/key.pem \\
  --server-ca ${REMOTE_DIR}/tls/ca-cert.pem \\
  --machine-id ${MACHINE_ID} \\
  --region ${REGION} \\
  --base ${STATE_DIR}/strategies \\
  --metrics-listen ${METRICS_IP}:9101
Restart=always
RestartSec=5s

# Cheap hardening. Not a substitute for cgroup confinement of the strategy
# processes this agent spawns, which is configured separately.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
}

WANT_UNIT_HASH="$(render_unit | shasum -a 256 2>/dev/null | cut -d' ' -f1 \
                  || render_unit | sha256sum | cut -d' ' -f1)"

# ---------------------------------------------------------------- decide
BINARY_ACTION=skip
[[ "$REMOTE_SHA" == "none" ]]       && BINARY_ACTION=install
[[ "$REMOTE_SHA" != "none" && "$REMOTE_SHA" != "$WANT_SHA" ]] && BINARY_ACTION=upgrade

UNIT_ACTION=skip
[[ "$UNIT_HASH" != "$WANT_UNIT_HASH" ]] && UNIT_ACTION=write

if [[ "$BINARY_ACTION" == "skip" && "$UNIT_ACTION" == "skip" ]]; then
  log "up to date ($TAG) — nothing to do"
  exit 0
fi

log "binary: $BINARY_ACTION    unit: $UNIT_ACTION"
if $CHECK_ONLY; then
  log "(--check: no changes made)"
  exit 0
fi

# ------------------------------------------------------------------ identity
# Only on first install: reissuing would orphan the identity the control plane
# already knows, and the machine ID is the client-certificate CN.
CERT_DIR="$CERT_ROOT/$MACHINE_ID"
if [[ "$BINARY_ACTION" == "install" && ! -f "$CERT_DIR/cert.pem" ]]; then
  [[ -f "$CA_DIR/ca-key.pem" ]] || fail "CA private key not found at $CA_DIR — cannot issue an identity"
  [[ -x ./bin/strategon-ca ]] || go build -o bin/strategon-ca ./cmd/strategon-ca
  ./bin/strategon-ca sign --ca "$CA_DIR" --cn "$MACHINE_ID" --out "$CERT_DIR" >/dev/null
  log "issued client certificate (CN=$MACHINE_ID)"
fi

# --------------------------------------------------------------------- stage
ssh_cmd -o BatchMode=yes "$TARGET" "rm -rf /tmp/agent-install && mkdir -p /tmp/agent-install"
scp_cmd -q "$DEST/$ASSET" "$TARGET:/tmp/agent-install/agent"
render_unit | ssh_cmd -o BatchMode=yes "$TARGET" "cat > /tmp/agent-install/unit"
if [[ "$BINARY_ACTION" == "install" ]]; then
  scp_cmd -q "$CERT_DIR/cert.pem" "$CERT_DIR/key.pem" "$TARGET:/tmp/agent-install/"
  scp_cmd -q "$CA_DIR/ca-cert.pem" "$TARGET:/tmp/agent-install/ca-cert.pem"
fi

# --------------------------------------------------------------------- apply
# Written to a file rather than piped: `sudo bash -s` consumes stdin, so sudo
# has no terminal left to read a password from. Kept outside the staging
# directory the script itself deletes -- bash reads scripts incrementally, so a
# self-deleting script can fail midway.
APPLY=/tmp/strategon-apply.sh
ssh_cmd -o BatchMode=yes "$TARGET" "cat > $APPLY" <<REMOTE
set -euo pipefail

# Unprivileged service account: the agent spawns and supervises strategy
# processes, so running it as root would hand every strategy root on this host.
id strategon >/dev/null 2>&1 || \
  useradd --system --home-dir $STATE_DIR --create-home --shell /usr/sbin/nologin strategon

install -d -o root -g root      -m 0755 $REMOTE_DIR
install -d -o root -g strategon -m 0750 $REMOTE_DIR/tls
install -d -o strategon -g strategon -m 0750 $STATE_DIR/strategies

# Stop before replacing: a running binary cannot be overwritten in place, and
# swapping underneath a live process invites a partially-written executable.
if [ "$BINARY_ACTION" = "upgrade" ]; then
  systemctl stop strategon-agent.service || true
fi

install -o root -g root -m 0755 /tmp/agent-install/agent $REMOTE_DIR/agent

# Private keys: readable by the service account, writable only by root.
if [ -f /tmp/agent-install/cert.pem ]; then
  install -o root -g strategon -m 0640 /tmp/agent-install/cert.pem    $REMOTE_DIR/tls/cert.pem
  install -o root -g strategon -m 0640 /tmp/agent-install/key.pem     $REMOTE_DIR/tls/key.pem
  install -o root -g strategon -m 0640 /tmp/agent-install/ca-cert.pem $REMOTE_DIR/tls/ca-cert.pem
fi

install -o root -g root -m 0644 /tmp/agent-install/unit $UNIT
rm -rf /tmp/agent-install

echo "$TAG" > $REMOTE_DIR/.installed-release

systemctl daemon-reload
systemctl enable strategon-agent.service >/dev/null 2>&1 || true
systemctl restart strategon-agent.service
REMOTE

if [[ -z "$SUDO" ]]; then
  ssh_cmd -o BatchMode=yes "$TARGET" "bash $APPLY; rm -f $APPLY"
else
  # -t allocates a terminal so sudo can prompt when the host requires a
  # password. Harmless where sudo is passwordless.
  ssh_cmd -t "$TARGET" "sudo bash $APPLY; rm -f $APPLY"
fi

# -------------------------------------------------------------------- verify
sleep 3
STATE="$(ssh_cmd -o BatchMode=yes "$TARGET" "systemctl is-active strategon-agent.service" || true)"
if [[ "$STATE" != "active" ]]; then
  printf 'error: agent is %s on %s\n' "$STATE" "$TARGET" >&2
  ssh_cmd -o BatchMode=yes "$TARGET" "$SUDO journalctl -u strategon-agent -n 20 --no-pager" >&2
  exit 1
fi
log "active on $TAG"
