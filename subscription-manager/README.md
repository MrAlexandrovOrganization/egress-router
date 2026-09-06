# subscription-manager

Fetches HTTPS v2ray-style subscriptions and generates a sing-box runtime config.
The static Go binary uses a sing-box validator pinned to the same image digest
as the router. For first deployment, follow the [root README](../README.md);
do not initialize an independent state file or start Compose directly here.

## Operations

```bash
make add NAME=provider URL=https://example.invalid/subscription
make refresh
make list
go test ./...
```

`add` and `refresh` delegate to the root deployment script under the same host
`flock` as deployment. Add parameters reach `jq` via environment, never shell
interpolation, and requests/responses are not printed. Real URLs are credentials:
prefer setting `NAME` and `URL` through a trusted environment mechanism and running
`make add`, since command-line values can appear in shell history/process lists.
`list` shows names only. Do not trace commands or print subscription/config files.

State defaults to `/data/state/subscriptions.json` in the writable directory bind
`../state`. Root `make init` sets private permissions for `EGRESS_UID/EGRESS_GID`
(default 1000:1000) and migrates the legacy local `subscriptions.json` only if the
new file is absent. The legacy file is neither logged nor removed.

Every refresh validates the complete result. ANY invalid node, including unsupported
VMess, fails the whole refresh with HTTP 400 and preserves the generated config.
`GET /health` is 503 before the first successful build and after a failed attempt;
the Alpine container probes it using localhost `wget`.

Periodic refresh writes `/data/runtime/config.json` but does not apply it. The
router mounts a separate `/data/runtime/active.json` host snapshot. Manual
operations require a successful POST before snapshotting and recreating the router,
then check container health and HTTPS through the localhost mixed proxy. Failed
activation restores the previous active snapshot; `make -C .. rollback` also
reapplies it explicitly. Explicit rollback preserves current active config for
recovery on failure and swaps it into the previous snapshot on success, allowing
another rollback to undo the operation. Legacy migration seeds missing active
config from existing generated config, which may differ from a running container's
older bind-mounted config. Config rollback does not undo subscription state or
manager/image changes. Direct is a urltest candidate, not a fallback-only route.

The API is unauthenticated and localhost-only. The manager never receives Docker
socket access; host scripts own activation. See the root README for timeout,
concurrency, smoke-test, and rollback limitations.
