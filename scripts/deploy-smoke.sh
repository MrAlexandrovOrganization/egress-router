#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Only public examples and deployment scripts enter this isolated container.
docker run --rm -i --network none --read-only --tmpfs /work:rw,nosuid,nodev \
    -v "$PWD/scripts/init.sh:/input/init.sh:ro" \
    -v "$PWD/scripts/deploy.sh:/input/deploy.sh:ro" \
    -v "$PWD/config.json.example:/input/config.json.example:ro" \
    -v "$PWD/subscription-manager/subscriptions.json.example:/input/subscriptions.json.example:ro" \
    -w /work debian:bookworm-slim bash -s <<'TEST'
set -euo pipefail
mkdir subscription-manager
cp /input/config.json.example .
cp /input/subscriptions.json.example subscription-manager/
bash /input/init.sh 1234 2345
test "$(stat -c '%u:%g:%a' state)" = 1234:2345:700
test "$(stat -c '%u:%g:%a' runtime)" = 1234:2345:700
test "$(stat -c '%u:%g:%a' state/subscriptions.json)" = 1234:2345:600
test "$(stat -c '%u:%g:%a' runtime/active.json)" = 1234:2345:640
cmp config.json runtime/active.json
cmp config.json runtime/config.json
rm state/subscriptions.json
printf '{"subscriptions":[],"fixture":"legacy"}\n' > subscription-manager/subscriptions.json
bash /input/init.sh 1234 2345
cmp subscription-manager/subscriptions.json state/subscriptions.json
cp state/subscriptions.json expected.json
printf '{"subscriptions":[]}\n' > subscription-manager/subscriptions.json
bash /input/init.sh 3456 4567
cmp expected.json state/subscriptions.json
test "$(stat -c '%u:%g:%a' state/subscriptions.json)" = 3456:4567:600
rm runtime/active.json
printf '{"fixture":"legacy-generated"}\n' > runtime/config.json
bash /input/init.sh 3456 4567
cmp runtime/config.json runtime/active.json
if cmp -s config.json runtime/active.json; then exit 1; fi
printf '{"fixture":"new-background-output"}\n' > runtime/config.json
cp runtime/active.json expected-active.json
bash /input/init.sh 3456 4567
cmp expected-active.json runtime/active.json
rm runtime/active.json
mkdir runtime/active.json
if bash /input/init.sh 3456 4567; then exit 1; fi
printf 'Isolated initialization smoke checks passed\n'
rmdir runtime/active.json

mkdir scripts fixtures
cp /input/init.sh /input/deploy.sh scripts/
printf '{"fixture":"original"}\n' > fixtures/original.json
printf '{"fixture":"candidate"}\n' > fixtures/candidate.json
printf '{"fixture":"bad"}\n' > fixtures/bad.json

# Exported functions survive the locked re-exec. No Docker socket, networking,
# host sudo or real manager/router is available inside this container.
sudo() {
    if flock -n .deploy.lock true; then
        printf 'Deployment did not retain its lock\n' >&2
        return 1
    fi
    "$@"
}
docker() {
    case "$*" in
        'compose -p egress-router -f docker-compose.yaml up -d --build --no-deps subscription-manager')
            printf 'manager\n' >> calls ;;
        'compose -p egress-router -f docker-compose.yaml up -d --no-deps --force-recreate egress-router')
            test "$(stat -c '%a' runtime/active.json)" = 640
            printf 'recreate\n' >> calls ;;
        'compose -p egress-router -f docker-compose.yaml ps -q egress-router')
            printf 'fake-router\n' ;;
        'inspect '*) printf 'running healthy\n' ;;
        *) printf 'Unexpected Docker invocation\n' >&2; return 1 ;;
    esac
}
curl() {
    case "${!#}" in
        http://127.0.0.1:19091/health) printf '503' ;;
        http://127.0.0.1:19091/refresh)
            printf 'generate\n' >> calls
            [[ ${GENERATION_FAIL:-0} != 1 ]] || return 22
            cp "${GENERATED_FIXTURE:-fixtures/candidate.json}" runtime/config.json ;;
        https://cp.cloudflare.com/generate_204)
            [[ " $* " == *' --proxy http://127.0.0.1:10880 '* ]] || return 1
            printf 'smoke\n' >> calls
            ! cmp -s fixtures/bad.json runtime/active.json ;;
        *) printf 'Unexpected curl invocation\n' >&2; return 1 ;;
    esac
}
sleep() { :; }
jq() { return 1; } # Not needed by these scenarios; fail on unexpected use.
export -f sudo docker curl sleep jq
export EGRESS_UID=3456 EGRESS_GID=4567

cp fixtures/original.json runtime/active.json
: > calls
bash scripts/deploy.sh deploy
cmp fixtures/candidate.json runtime/active.json
cmp fixtures/original.json runtime/previous.json
test "$(grep -c '^recreate$' calls)" = 1
test "$(grep -c '^manager$' calls)" = 1
test "$(grep -c '^generate$' calls)" = 1

# Successful rollback swaps history, and a second rollback can undo it.
bash scripts/deploy.sh rollback
cmp fixtures/original.json runtime/active.json
cmp fixtures/candidate.json runtime/previous.json
bash scripts/deploy.sh rollback
cmp fixtures/candidate.json runtime/active.json
cmp fixtures/original.json runtime/previous.json

cp fixtures/original.json runtime/active.json
: > calls
if GENERATED_FIXTURE=fixtures/bad.json bash scripts/deploy.sh refresh; then exit 1; fi
cmp fixtures/original.json runtime/active.json
cmp fixtures/original.json runtime/previous.json
test "$(grep -c '^recreate$' calls)" = 2

cp fixtures/bad.json runtime/previous.json
: > calls
if bash scripts/deploy.sh rollback; then exit 1; fi
cmp fixtures/original.json runtime/active.json
cmp fixtures/bad.json runtime/previous.json
test "$(grep -c '^recreate$' calls)" = 2
test "$(find runtime -name '.rollback.*' | wc -l)" = 0

: > calls
if GENERATION_FAIL=1 bash scripts/deploy.sh refresh; then exit 1; fi
cmp fixtures/original.json runtime/active.json
cmp fixtures/bad.json runtime/previous.json
test "$(grep -c '^recreate$' calls)" = 0
test "$(grep -c '^generate$' calls)" = 1
printf 'Isolated deployment orchestration smoke checks passed\n'
TEST
