# subscription-manager

Fetches HTTPS v2ray-style subscriptions and generates a sing-box runtime config.
The implementation is a single static Go binary with no runtime dependencies
other than `sing-box` for config validation.

## Local state

Create the ignored state file before starting the compose stack:

```bash
cp subscriptions.json.example subscriptions.json
```

Add a subscription without putting its URL in Git:

```bash
make add NAME=provider URL=https://example.invalid/subscription
```

The manager refreshes periodically and validates the generated config with `sing-box check`.

Run local tests with:

```bash
go test ./...
```
