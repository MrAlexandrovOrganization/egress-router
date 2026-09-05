#!/bin/sh
set -eu

NFT_TABLE=telemt_tproxy
TELEMT_SOURCE_IP=${TELEMT_SOURCE_IP:-192.168.128.2}
TELEMT_UID=${TELEMT_UID:-65532}
HOST_IFACE=${HOST_IFACE:-eth0}

start() {
    /usr/sbin/nft delete table ip "$NFT_TABLE" 2>/dev/null || true
    /usr/sbin/nft -f - <<EOF
table ip telemt_tproxy {
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
    /usr/sbin/nft delete table ip "$NFT_TABLE" 2>/dev/null || true
}

case "${1:-}" in
    start) start ;;
    stop) stop ;;
    restart) stop; start ;;
    *) echo "usage: $0 {start|stop|restart}" >&2; exit 2 ;;
esac
