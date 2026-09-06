#!/usr/bin/env bash
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
action=${1:-deploy}
case $action in init|deploy|refresh|add|rollback) ;; *) exit 2 ;; esac
for command in flock sudo docker curl jq; do command -v "$command" >/dev/null; done
if [[ ${DEPLOY_LOCKED:-0} != 1 ]]; then
    exec 9>.deploy.lock
    flock -x 9
    # CI may have changed the checkout while this process waited for the lock.
    DEPLOY_LOCKED=1 exec bash scripts/deploy.sh "$@"
fi
export EGRESS_UID=${EGRESS_UID:-1000} EGRESS_GID=${EGRESS_GID:-1000}
sudo bash scripts/init.sh "$EGRESS_UID" "$EGRESS_GID"
[[ $action != init ]] || exit 0
compose=(docker compose -p egress-router -f docker-compose.yaml)
api=http://127.0.0.1:19091
generation_failed() {
    printf 'Generation failed; active config unchanged\n' >&2
    # Health contains only fixed diagnostics and counters, never provider URLs.
    curl --silent --noproxy '*' --max-time 5 "$api/health" |
        jq -c '{ok,error,nodes,skipped_nodes,last_attempt,last_success}' >&2 || true
    exit 1
}
smoke_url=${SMOKE_URL:-https://cp.cloudflare.com/generate_204}
proxy=${SMOKE_PROXY:-http://127.0.0.1:10880}
[[ $smoke_url == https://* && $proxy == http://127.0.0.1:* ]] || {
    printf 'Smoke requires HTTPS and a localhost mixed proxy\n' >&2; exit 2;
}
snapshot() {
    sudo install -m 0640 -o "$EGRESS_UID" -g "$EGRESS_GID" "$1" "$2.new" &&
    sudo mv -f "$2.new" "$2"
}
router_ready() {
    local id status
    id=$("${compose[@]}" ps -q egress-router)
    [[ -n $id ]] || return 1
    for ((i=0; i<30; i++)); do
        status=$(docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$id") || return 1
        if [[ $status == 'running healthy' ]] && curl --silent --fail --output /dev/null \
            --connect-timeout 5 --max-time 10 --noproxy '' --proxy "$proxy" "$smoke_url"; then
            return 0
        fi
        sleep 2
    done
    return 1
}
recreate() { "${compose[@]}" up -d --no-deps --force-recreate egress-router; }
if [[ $action == rollback ]]; then
    sudo test -f runtime/previous.json || { printf 'No previous snapshot\n' >&2; exit 1; }
    recovery=$(sudo mktemp runtime/.rollback.XXXXXX)
    snapshot runtime/active.json "$recovery"
    candidate=runtime/previous.json
else
    if [[ $action == deploy ]]; then
        "${compose[@]}" up -d --build --no-deps subscription-manager
    fi
    # Health can be 503 before the first build or after failure. Wait for HTTP
    # readiness, not health, so a corrective add/refresh can recover the manager.
    ready=false
    for ((i=0; i<60; i++)); do
        code=$(curl --silent --noproxy '*' --output /dev/null --write-out '%{http_code}' --max-time 2 "$api/health") || code=000
        if [[ $code == 200 || $code == 503 ]]; then ready=true; break; fi
        sleep 2
    done
    $ready || { printf 'Manager readiness timed out\n' >&2; exit 1; }
    if [[ $action == add ]]; then
        [[ -n ${NAME:-} && -n ${URL:-} ]] || { printf 'Usage: make add NAME=name URL=https://...\n' >&2; exit 2; }
        jq -n '{name:env.NAME,url:env.URL}' | curl --silent --noproxy '*' --fail --output /dev/null \
            --max-time 300 -X POST -H 'Content-Type: application/json' --data-binary @- "$api/subscriptions" || {
            generation_failed
        }
    else
        curl --silent --noproxy '*' --fail --output /dev/null --max-time 300 -X POST "$api/refresh" || {
            generation_failed
        }
    fi
    snapshot runtime/active.json runtime/previous.json
    recovery=runtime/previous.json
    candidate=runtime/config.json
fi
restore() {
    trap - EXIT INT TERM
    printf 'Activation failed; restoring previous active snapshot\n' >&2
    if ! snapshot "$recovery" runtime/active.json || ! recreate || ! router_ready; then
        printf 'Rollback verification failed; operator intervention required\n' >&2
    elif [[ $action == rollback ]]; then
        sudo rm -f "$recovery"
    fi
    exit 1
}
trap restore EXIT
trap 'exit 1' INT TERM
snapshot "$candidate" runtime/active.json
recreate
router_ready
if [[ $action == rollback ]]; then
    snapshot "$recovery" runtime/previous.json
fi
trap - EXIT INT TERM
if [[ $action == rollback ]]; then
    sudo rm -f "$recovery"
fi
printf 'Active config passed router health and HTTPS proxy smoke test\n'
