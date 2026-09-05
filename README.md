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

For host-networked telemt, set the host source address before enabling the unit:

```bash
sudo install -d -m 0755 /etc/default
printf '%s\n' 'TELEMT_SOURCE_IP=10.130.0.5' 'HOST_IFACE=eth0' | sudo tee /etc/default/telemt-egress-tproxy >/dev/null
sudo systemctl enable --now telemt-egress-tproxy.service
```

The interception is limited to telemt's container IP, Telegram CIDRs, and TCP port 443.

The host-specific defaults assume telemt has Docker IP `192.168.128.2` and the
host interface is `eth0`. Override `TELEMT_SOURCE_IP` and `HOST_IFACE` through
`/etc/default/telemt-egress-tproxy` when deploying on a different layout.

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

## CI/CD

Pull requests run `make check` and build the subscription-manager image. A push
to `main` deploys only after both checks pass. The deploy job connects over SSH
and expects these GitHub Actions secrets:

```text
VM_HOST
VM_USER
VM_SSH_KEY
VM_PROJECT_PATH=/home/maxim/projects/infra/network/egress-router
```

The VM keeps its ignored `config.json`, `subscription-manager/subscriptions.json`
and `runtime/` files. Deployment updates tracked files with `git pull --ff-only`,
then runs `make deploy` and `make status`.
