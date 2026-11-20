#!/usr/bin/env bash
# End-to-end tests for the timothywashburn.ansibled roles, on Docker.
#
#   collection/tests/run.sh
#
# What it does:
#   - makes a throwaway Ansible toolchain with uv (or uses $ANSIBLE_VENV),
#   - builds and installs the collection from this checkout (what `type: git` does),
#   - starts containers: Rocky Linux 9 and Debian trixie targets (sshd, sudo,
#     python3), an SSH client, and docker:dind as the daemon host,
#   - builds the ansibled image from the Containerfile and loads it into dind,
#   - runs the roles (check mode on fresh hosts, apply, re-apply, check mode
#     again) and checks SSH/sudo, from= restrictions, file modes, secret
#     persistence, overlay pruning, restarts, health, `ansibled check-config`,
#     webhook registration against fake_gitlab.py, and state=absent.
#
# Everything it creates is named ansibled-roles-test-* and removed on exit.
# Base images it pulls (rockylinux/rockylinux:9, debian:trixie, docker:dind,
# and the Containerfile's build images) are left in place.
#
# Environment:
#   ANSIBLE_VENV      existing virtualenv with ansible-core (+ python-gitlab) to use
#   ANSIBLE_CORE      pip requirement for the throwaway venv (default: ansible-core)
#   ANSIBLE_DEPS_PATH collections path that already has the dependencies
#                     (community.docker/general/crypto, ansible.posix); otherwise
#                     they are installed from Galaxy
#   KEEP=1            leave the containers running for inspection
#   ONLY=target       only test the ansibled_target role (no dind, no image build)
#   WORK              work directory (default: a new temp dir)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
P=ansibled-roles-test
WORK=${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/$P.XXXXXX")}
KEEP=${KEEP:-0}
ONLY=${ONLY:-}
FG_PORT=18080
FG_TOKEN=test-hook-token

cleanup() {
  [ -n "${FG_PID:-}" ] && kill "$FG_PID" 2>/dev/null || true
  if [ "$KEEP" = 1 ]; then echo "KEEP=1: containers left running, work dir $WORK"; return; fi
  docker rm -f -v $P-rocky9 $P-trixie $P-client $P-dind >/dev/null 2>&1 || true
  docker network rm $P-net >/dev/null 2>&1 || true
  docker rmi $P-image:dev >/dev/null 2>&1 || true
}
trap cleanup EXIT

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }

# ---------------------------------------------------------------- toolchain
step "toolchain in $WORK"
if [ -z "${ANSIBLE_VENV:-}" ]; then
  uv venv -q "$WORK/venv" --python 3.12
  uv pip install -q -p "$WORK/venv" "${ANSIBLE_CORE:-ansible-core}" python-gitlab requests
  ANSIBLE_VENV=$WORK/venv
fi
export PATH="$ANSIBLE_VENV/bin:$PATH"
unset ANSIBLE_CONFIG ANSIBLE_BECOME_PASSWORD_FILE ANSIBLE_VAULT_PASSWORD_FILE ANSIBLE_INVENTORY
export ANSIBLE_HOST_KEY_CHECKING=False ANSIBLE_PYTHON_INTERPRETER=auto_silent ANSIBLE_NOCOLOR=1 \
  ANSIBLE_RETRY_FILES_ENABLED=0
ansible --version | head -1

ansible-galaxy collection build "$REPO/collection" --output-path "$WORK/dist" --force >/dev/null
if [ -n "${ANSIBLE_DEPS_PATH:-}" ]; then
  export ANSIBLE_COLLECTIONS_PATH=$WORK/collections:$ANSIBLE_DEPS_PATH
  ansible-galaxy collection install --no-deps -p "$WORK/collections" "$WORK"/dist/timothywashburn-ansibled-*.tar.gz >/dev/null
else
  export ANSIBLE_COLLECTIONS_PATH=$WORK/collections
  ansible-galaxy collection install -p "$WORK/collections" "$WORK"/dist/timothywashburn-ansibled-*.tar.gz >/dev/null
fi
ansible-galaxy collection list 2>/dev/null | grep -E 'timothywashburn|community\.(docker|general|crypto)|ansible\.posix'

# --------------------------------------------------------------- containers
step "containers"
docker network create $P-net >/dev/null
docker run -d --name $P-rocky9 --hostname rocky9 --network $P-net rockylinux/rockylinux:9 sleep infinity >/dev/null
docker run -d --name $P-trixie --hostname trixie --network $P-net debian:trixie sleep infinity >/dev/null
docker run -d --name $P-client --hostname client --network $P-net debian:trixie sleep infinity >/dev/null
[ "$ONLY" = target ] || docker run -d --privileged --name $P-dind --hostname dind --network $P-net -e DOCKER_TLS_CERTDIR= docker:dind >/dev/null
docker exec $P-rocky9 sh -c 'dnf -y -q install openssh-server sudo python3 >/dev/null && ssh-keygen -A >/dev/null && /usr/sbin/sshd' &
docker exec $P-trixie sh -c 'apt-get -qq update && DEBIAN_FRONTEND=noninteractive apt-get -qq install -y openssh-server sudo python3 >/dev/null && mkdir -p /run/sshd && /usr/sbin/sshd' &
docker exec $P-client sh -c 'apt-get -qq update && DEBIAN_FRONTEND=noninteractive apt-get -qq install -y openssh-client >/dev/null && ssh-keygen -q -t ed25519 -N "" -f /root/k' &
[ "$ONLY" = target ] || docker exec $P-dind sh -c 'apk add -q python3 openssh-keygen && for i in $(seq 60); do docker info >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1' &
wait
ip() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$P-$1"; }
ROCKY_IP=$(ip rocky9); TRIXIE_IP=$(ip trixie); CLIENT_IP=$(ip client); DIND_IP=$([ "$ONLY" = target ] || ip dind)
CLIENT_KEY=$(docker exec $P-client cat /root/k.pub)
echo "rocky9=$ROCKY_IP trixie=$TRIXIE_IP client=$CLIENT_IP dind=$DIND_IP"

if [ "$ONLY" != target ]; then
step "ansibled image (Containerfile) -> dind"
docker build -q -f "$REPO/Containerfile" -t $P-image:dev "$REPO" >/dev/null
docker save $P-image:dev | docker exec -i $P-dind docker load
fi

# ------------------------------------------------------------------ helpers
play() {
  echo "+ ansible-playbook $*"
  ansible-playbook -i "$HERE/inventory.yml" "$@" >"$WORK/last.log" 2>&1 || true
  sed -n '/^PLAY RECAP/,$p' "$WORK/last.log" | grep -v '^PLAY RECAP' | grep . || { tail -30 "$WORK/last.log"; fail "no recap"; }
}
recap() { sed -n '/^PLAY RECAP/,$p' "$WORK/last.log" | grep -v '^PLAY RECAP'; }
assert_ok() {
  if recap | grep -qE 'failed=[1-9]|unreachable=[1-9]'; then
    grep -E 'fatal|FAILED|ERROR' "$WORK/last.log" | head -10; fail "$1"
  fi
  pass "$1"
}
assert_unchanged() {
  assert_ok "$1"
  if recap | grep -qE 'changed=[1-9]'; then grep -B3 '^changed' "$WORK/last.log" | head -40; fail "$1: changed"; fi
  pass "$1: changed=0"
}
assert_failed_with() {
  grep -q "$2" "$WORK/last.log" || { tail -20 "$WORK/last.log"; fail "$1: expected '$2'"; }
  recap | grep -qE 'failed=[1-9]' || fail "$1: expected a failure"
  pass "$1"
}
SSH_OPTS="-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5"
ssh_client() { docker exec $P-client ssh -i /root/k $SSH_OPTS "ansibled@$1" 'sudo -n true'; }
ssh_daemon() { docker exec $P-dind docker exec ansibled-testsite ssh -i /etc/ansibled/secrets/ssh_key $SSH_OPTS "ansibled@$1" 'sudo -n true'; }
dind() { docker exec $P-dind sh -c "$*"; }
started_at() { dind 'docker inspect -f "{{.State.StartedAt}}" ansibled-testsite'; }

TARGET="$HERE/target.yml"
DAEMON=("$HERE/daemon.yml" -e test_image=$P-image:dev)

# ---------------------------------------------------------- ansibled_target
step "ansibled_target"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e '{"test_from":["10.9.9.9"]}' --check
assert_ok "check mode on fresh hosts"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e '{"test_from":["10.9.9.9"]}'
assert_ok "apply with from=10.9.9.9"
for h in $ROCKY_IP $TRIXIE_IP; do ssh_client "$h" 2>/dev/null && fail "login from $CLIENT_IP to $h should be refused"; done
pass "login from an address outside ansibled_target_from is refused"

FROM="{\"test_from\":[\"$CLIENT_IP\",\"10.0.0.0/24\"]}"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e "$FROM"
assert_ok "apply with from=client"
for h in $ROCKY_IP $TRIXIE_IP; do ssh_client "$h" || fail "ssh + sudo -n true to $h"; done
pass "ssh as ansibled + sudo -n true works on both targets"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e "$FROM"
assert_unchanged "second apply"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e "$FROM" --check
assert_unchanged "check mode after apply"

for h in rocky9 trixie; do
  got=$(docker exec $P-$h sh -c 'stat -c "%a %U" /var/lib/ansibled /var/lib/ansibled/.ssh /var/lib/ansibled/.ssh/authorized_keys /etc/sudoers.d/ansibled | tr "\n" ,; getent passwd ansibled | cut -d: -f7; grep "^ansibled:" /etc/shadow | cut -d: -f2 | cut -c1')
  [ "$got" = "$(printf '700 ansibled,700 ansibled,600 ansibled,440 root,/bin/bash\n!')" ] || fail "$h layout: $got"
done
pass "home/.ssh/authorized_keys/sudoers modes, shell, locked password"

docker exec $P-trixie sh -c 'echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAK stray" >> /var/lib/ansibled/.ssh/authorized_keys'
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e "$FROM" -l "$P-trixie"
assert_ok "re-apply after a stray key was added"
docker exec $P-trixie grep -q stray /var/lib/ansibled/.ssh/authorized_keys && fail "stray key not removed"
pass "authorized_keys is exclusive"

play "$TARGET" -e "test_pubkey=''" -l "$P-trixie" --check
assert_failed_with "missing key is rejected" "ansibled_target_public_key is required"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e test_state=bogus -l "$P-trixie" --check
assert_failed_with "bad state is rejected by argument_specs" "Validation of arguments failed"

play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e test_state=absent
assert_ok "state=absent"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e test_state=absent
assert_unchanged "state=absent again"
play "$TARGET" -e "test_pubkey='$CLIENT_KEY'" -e test_state=absent --check
assert_unchanged "state=absent check mode"
for h in rocky9 trixie; do
  docker exec $P-$h sh -c 'getent passwd ansibled || getent group ansibled || test -e /var/lib/ansibled || test -e /etc/sudoers.d/ansibled' && fail "$h: leftovers after absent"
done
pass "user, group, home and sudoers removed"

[ "$ONLY" = target ] && { echo; echo "ONLY=target: done"; exit 0; }

# ----------------------------------------------------------------- ansibled
step "ansibled"
play "${DAEMON[@]}" -e @"$HERE/secrets.yml" --check
assert_ok "check mode on a fresh host"
play "${DAEMON[@]}" -e @"$HERE/secrets.yml"
assert_ok "first deploy"

for i in $(seq 40); do
  [ "$(dind 'docker inspect -f "{{.State.Health.Status}}" ansibled-testsite')" = healthy ] && break
  [ "$i" = 40 ] && { dind 'docker logs ansibled-testsite' | tail -20; fail "container not healthy"; }
  sleep 3
done
pass "container is healthy"
dind 'docker exec ansibled-testsite ansibled check-config -config /etc/ansibled/ansibled.yml' || fail "check-config"
pass "ansibled check-config accepts the rendered config and secret files"

got=$(dind 'cd /opt/ansibled && find . -path "./data/*" -prune -o -print | sort | xargs stat -c "%a %u:%g %n"')
want='755 0:0 .
644 0:0 ./compose.yml
700 10001:10001 ./data
750 0:10001 ./etc
640 0:10001 ./etc/ansibled.yml
700 10001:10001 ./etc/overlay
700 10001:10001 ./etc/overlay/ansible
400 10001:10001 ./etc/overlay/ansible/.vault-pass
700 10001:10001 ./etc/overlay/ansible/group_vars
700 10001:10001 ./etc/overlay/ansible/group_vars/all
600 10001:10001 ./etc/overlay/ansible/group_vars/all/secret.yml
644 10001:10001 ./etc/overlay/top.txt
700 10001:10001 ./etc/secrets
400 10001:10001 ./etc/secrets/forge_token
400 10001:10001 ./etc/secrets/metrics_token
400 10001:10001 ./etc/secrets/oidc_client_id
400 10001:10001 ./etc/secrets/oidc_client_secret
400 10001:10001 ./etc/secrets/session_key
400 10001:10001 ./etc/secrets/ssh_key
400 10001:10001 ./etc/secrets/ssh_key.pub
400 10001:10001 ./etc/secrets/webhook_secret'
[ "$got" = "$want" ] || { diff <(echo "$want") <(echo "$got"); fail "file layout"; }
pass "file layout, owners and modes"
for s in webhook_secret metrics_token session_key; do
  [ "$(dind "wc -c < /opt/ansibled/etc/secrets/$s" | tr -d ' ')" = 48 ] || fail "$s is not 48 chars"
done
pass "generated secrets are 48 characters"

hashes=$(dind 'sha256sum /opt/ansibled/etc/secrets/*')
started=$(started_at)
play "${DAEMON[@]}" -e @"$HERE/secrets.yml"
assert_unchanged "second deploy (with secrets)"
play "${DAEMON[@]}"
assert_unchanged "deploy without the secret variables"
play "${DAEMON[@]}" --check --diff
assert_unchanged "check mode without the secret variables"
[ "$hashes" = "$(dind 'sha256sum /opt/ansibled/etc/secrets/*')" ] || fail "secrets changed across reruns"
[ "$started" = "$(started_at)" ] || fail "container restarted without a change"
pass "secrets persist across reruns, no restart"

play "${DAEMON[@]}" -e test_overlay_set=b
assert_ok "overlay set b with prune"
[ "$(dind 'cd /opt/ansibled/etc/overlay && find . | sort | tr "\n" " "')" = ". ./ansible ./ansible/.vault-pass " ] || fail "overlay prune"
[ "$started" = "$(started_at)" ] || fail "overlay change restarted the container"
pass "overlay pruned (no restart needed: ansibled copies it per run)"

play "${DAEMON[@]}" -e test_overlay_set=b -e ansibled_forge_token=glpat-rotated-not-real
assert_ok "rotate the forge token"
grep -q 'RUNNING HANDLER.*Restart ansibled' "$WORK/last.log" || fail "restart handler did not run"
[ "$started" != "$(started_at)" ] || fail "container was not restarted"
pass "secret change restarts the container"

play "${DAEMON[@]}" -e ansibled_dir=/opt/fresh
assert_failed_with "fresh host without secrets fails" "ansibled_forge_token is undefined"
dind 'test ! -e /opt/fresh' || fail "fresh host was modified before the secret check"
play "${DAEMON[@]}" -e ansibled_dir=/opt/fresh --check
assert_ok "fresh host without secrets, check mode"
grep -q "WARNING: ansibled_forge_token is undefined" "$WORK/last.log" || fail "no check-mode warning"
pass "check mode only warns about the missing secrets"

CFG_BASE='"site":"x","public_url":"http://a:1","listen":{"webhook":"a:1","web":"a:2"},"repo":{"setup":"s","venv_key_files":["a"]},"ansible":{"inventory":"i","playbooks":["p"]}'
OIDC_OK='"oidc":{"issuer":"https://auth.example.org/","allowed_groups":["sysadmins"]}'
play "${DAEMON[@]}" --check -e "{\"ansibled_config\":{$CFG_BASE,\"forge\":{\"url\":\"http://g\",\"project\":\"p\",\"trusted_group\":\"g\",\"token_file\":\"/x\"},\"web\":{$OIDC_OK}}}"
assert_failed_with "role-managed key in ansibled_config is rejected" "token_file"
play "${DAEMON[@]}" --check -e "{\"ansibled_config\":{$CFG_BASE,\"forge\":{\"url\":\"http://g\",\"project\":\"p\",\"trusted_group\":\"g\"},\"web\":{\"oidc\":{\"issuer\":\"https://auth.example.org/\",\"allowed_groups\":[\"sysadmins\"],\"client_secret_file\":\"/x\"}}}}"
assert_failed_with "role-managed web.oidc key is rejected" "client_secret_file"
play "${DAEMON[@]}" --check -e "{\"ansibled_config\":{$CFG_BASE,\"forge\":{\"url\":\"http://g\",\"project\":\"p\",\"trusted_group\":\"g\"}}}"
assert_failed_with "missing web.oidc is rejected" "missing required arguments: web"
play "${DAEMON[@]}" --check -e "{\"ansibled_config\":{$CFG_BASE,\"forge\":{\"url\":\"http://g\",\"project\":\"p\",\"trusted_group\":\"g\"},\"web\":{\"oidc\":{\"issuer\":\"https://auth.example.org/\",\"allowed_groups\":[\"sysadmins\"],\"scopes\":[\"profile\"]}}}}"
assert_failed_with "web.oidc.scopes without openid is rejected" "must include openid"
play "${DAEMON[@]}" --check -e '{"ansibled_overlay_files":[{"path":"ansible/../../x","content":"do-not-print-me"}]}'
assert_failed_with "overlay path escaping the repo is rejected" "stays inside the repo"
grep -q do-not-print-me "$WORK/last.log" && fail "overlay content was printed"
pass "overlay content not printed on failure"

step "webhook registration (fake GitLab on the controller)"
python3 -I "$HERE/fake_gitlab.py" $FG_PORT $FG_TOKEN & FG_PID=$!
sleep 1
HOOK=(-e test_overlay_set=b -e ansibled_gitlab_hook_token=$FG_TOKEN -e ansibled_gitlab_api_url=http://127.0.0.1:$FG_PORT)
play "${DAEMON[@]}" "${HOOK[@]}" --check
assert_ok "hook check mode"
[ "$(curl -s http://127.0.0.1:$FG_PORT/_log)" = "[]" ] || fail "check mode wrote to GitLab"
play "${DAEMON[@]}" "${HOOK[@]}"
assert_ok "hook created"
play "${DAEMON[@]}" "${HOOK[@]}"
assert_unchanged "hook second run"
play "${DAEMON[@]}" "${HOOK[@]}" --check
assert_unchanged "hook check mode after creation"
curl -s http://127.0.0.1:$FG_PORT/_log | python3 -c '
import json, sys
log = json.load(sys.stdin)
c = log[0]["body"]
assert log[0]["op"] == "create", log
assert c["url"] == "http://127.0.0.1:8470/webhook/gitlab", c
assert c["push_events"] and c["merge_requests_events"] and c["note_events"], c
assert not c["tag_push_events"] and not c["issues_events"], c
print(c["token"])' >"$WORK/hook_token" || fail "hook payload"
[ "$(cat "$WORK/hook_token")" = "$(dind 'cat /opt/ansibled/etc/secrets/webhook_secret')" ] || fail "hook token != webhook secret"
pass "hook has push/MR/note events and the effective webhook secret"
play "${DAEMON[@]}" "${HOOK[@]}" -e ansibled_webhook_secret=rotated-webhook-secret-not-real
assert_ok "rotate the webhook secret"
curl -s http://127.0.0.1:$FG_PORT/_log | grep -q rotated-webhook-secret-not-real || fail "rotated secret not sent to GitLab"
pass "rotated webhook secret is pushed to the hook"
kill $FG_PID; FG_PID=

# -------------------------------------------------------------- integration
step "integration: daemon key on the targets"
play "$HERE/site.yml" -e test_image=$P-image:dev -e test_overlay_set=b -e "{\"test_from\":[\"$DIND_IP\"]}"
assert_ok "site.yml"
for h in $ROCKY_IP $TRIXIE_IP; do ssh_daemon "$h" || fail "ansibled container -> $h"; done
pass "the ansibled container logs in to both targets with its key and can sudo"
play "$HERE/site.yml" -e test_image=$P-image:dev -e test_overlay_set=b -e "{\"test_from\":[\"$DIND_IP\"]}"
assert_unchanged "site.yml again"
play "$HERE/site.yml" -e test_image=$P-image:dev -e test_overlay_set=b -e "{\"test_from\":[\"$DIND_IP\"]}" --check
assert_unchanged "site.yml check mode"

step "all tests passed"
