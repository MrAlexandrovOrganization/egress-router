# egress-router

sing-box egress router for telemt and local Docker services.

## First deployment

Clone this repository:

```text
network/
  egress-router/
```

Create the machine-specific base config and initialize subscription state:

```bash
cp config.json.example config.json
mkdir -p runtime
cp subscription-manager/subscriptions.json.example subscription-manager/subscriptions.json
docker compose up -d --build
```

Then add subscriptions from the machine itself:

```bash
make -C subscription-manager add NAME=provider URL=https://example.invalid/subscription
```

Install the optional telemt interception on a host where telemt has Docker IP `192.168.128.2`:

```bash
sudo install -m 0755 telemt-egress-tproxy.sh /usr/local/sbin/telemt-egress-tproxy
sudo install -m 0644 telemt-egress-tproxy.service /etc/systemd/system/telemt-egress-tproxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now telemt-egress-tproxy.service
```

The interception is limited to telemt's container IP, Telegram CIDRs, and TCP port 443.

The host-specific values are intentional: the nftables rule assumes telemt has
Docker IP `192.168.128.2`, and the host interface is `eth0`. Change these values
in `telemt-egress-tproxy.sh` when deploying on a different Docker/network layout.

After changing subscriptions, use `make refresh`. It refreshes the state, validates
the generated config, and recreates `egress-router` so the new nodes are applied.
The background refresh only updates the generated file; it does not restart the
router automatically.

The subscription-manager API is intentionally localhost-only and has no authentication.
Do not publish port `19091`; protect access through host permissions if other local
services are not trusted.

## Rollback

```bash
sudo systemctl disable --now telemt-egress-tproxy.service
docker compose down
```

`config.json` and generated `runtime/config.json` are intentionally not tracked.
