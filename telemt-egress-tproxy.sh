#!/bin/sh
set -eu

NFT_TABLE=telemt_tproxy
TELEMT_SOURCE_IP=${TELEMT_SOURCE_IP:-192.168.128.2}
TELEMT_UID=${TELEMT_UID:-65532}
HOST_IFACE=${HOST_IFACE:-eth0}

fail() {
    echo "$*" >&2
    exit 1
}

validate() {
    case "$TELEMT_SOURCE_IP" in
        ''|*[!0-9.]*|.*|*.|*..*) fail 'TELEMT_SOURCE_IP must be a dotted-decimal IPv4 address' ;;
    esac
    IFS=. read -r a b c d <<EOF
$TELEMT_SOURCE_IP
EOF
    for octet in "$a" "$b" "$c" "$d"; do
        case "$octet" in
            ''|*[!0-9]*|0[0-9]*) fail 'Invalid TELEMT_SOURCE_IP octet' ;;
        esac
        if [ "${#octet}" -gt 3 ] || [ "$octet" -gt 255 ]; then
            fail 'Invalid TELEMT_SOURCE_IP octet'
        fi
    done
    case "$TELEMT_UID" in
        ''|*[!0-9]*|0[0-9]*) fail 'TELEMT_UID must be a decimal numeric UID' ;;
    esac
    if [ "${#TELEMT_UID}" -gt 10 ] || [ "$TELEMT_UID" -gt 4294967294 ]; then
        fail 'TELEMT_UID is out of range'
    fi
    case "$HOST_IFACE" in
        ''|.|..|*[!a-zA-Z0-9_.:-]*) fail 'Invalid HOST_IFACE' ;;
    esac
    [ "${#HOST_IFACE}" -le 15 ] || fail 'HOST_IFACE exceeds Linux interface name length'
}

ready() {
    command -v ss >/dev/null 2>&1 || fail 'ss (iproute2) is required to check sing-box readiness'
    attempts=30
    while [ "$attempts" -gt 0 ]; do
        listeners=$(ss -H -4 -ltn 'sport = :12345') || fail 'Cannot query TCP listeners with ss'
        # Both local OUTPUT and container PREROUTING redirects need a wildcard bind.
        while read -r _ _ _ address _; do
            case "$address" in
                '0.0.0.0:12345'|'*:12345') return 0 ;;
            esac
        done <<EOF
$listeners
EOF
        attempts=$((attempts - 1))
        [ "$attempts" -eq 0 ] || sleep 1
    done
    fail 'sing-box redirect listener 0.0.0.0:12345 not ready after 30 checks'
}

start() {
    validate
    ready
    # add is non-exclusive; all three operations commit atomically via nft -f.
    /usr/sbin/nft -f - <<EOF
add table ip $NFT_TABLE
delete table ip $NFT_TABLE
table ip $NFT_TABLE {
    chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        ip saddr $TELEMT_SOURCE_IP ip daddr { 91.105.192.0/23, 91.108.4.0/22, 91.108.8.0/22, 91.108.12.0/22, 91.108.16.0/22, 91.108.20.0/22, 91.108.56.0/22, 149.154.160.0/20, 185.76.151.0/24 } tcp dport 443 counter redirect to :12345
    }
    chain output {
        type nat hook output priority dstnat; policy accept;
        meta skuid $TELEMT_UID ip daddr { 91.105.192.0/23, 91.108.4.0/22, 91.108.8.0/22, 91.108.12.0/22, 91.108.16.0/22, 91.108.20.0/22, 91.108.56.0/22, 149.154.160.0/20, 185.76.151.0/24 } tcp dport 443 counter redirect to :12345
    }
    chain input {
        type filter hook input priority -10; policy accept;
        iifname "$HOST_IFACE" tcp dport 12345 drop
    }
}
EOF
}

stop() {
    /usr/sbin/nft -f - <<EOF
add table ip $NFT_TABLE
delete table ip $NFT_TABLE
EOF
}

case "${1:-}" in
    start|restart) start ;;
    ready) validate; ready ;;
    stop) stop ;;
    *) echo "usage: $0 {start|stop|restart|ready}" >&2; exit 2 ;;
esac
