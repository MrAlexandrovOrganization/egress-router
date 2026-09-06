# Catalog Shadow Client

Opt-in, one-shot, read-only catalog integration. Nothing invokes this CLI from
Compose, deployment, refresh, or subscription-manager. It never activates a
candidate, reloads sing-box, updates subscriptions, or calls catalog mutations.
There is deliberately no shadow Dockerfile or change to the production image.

## Build And Check

From the router root:

```sh
make shadow-check
make -C catalog-client build
```

The executable is `catalog-client/bin/catalog-shadow`, built for the native
platform. Go 1.23+ is required. All runtime Go dependencies and generated protobuf
code belong to this module; there are no sibling paths or module replacements.
Root `make test` covers both Go modules with race/coverage and vet; formatting
checks cover both. CI builds this CLI natively. Root `make config-check` validates
generated synthetic fixtures from both modules with the same pinned sing-box
image. Local unit tests use bufconn and fake validators, not Docker or live data.
The container test is opt-in with `SING_BOX_TEST_IMAGE`; it runs only `check`, with
network disabled. Pre-pull the image when running it independently.

## Explicit Invocation

This is documentation, not an activation step. Run only in an approved shadow
environment with a local read-only base and an operator-owned output directory.
Paths are relative to the invocation directory, not the executable. The default
output assumes invocation from the router root, where `runtime/` already exists.

```sh
CATALOG_ADDR=catalog.example:443 \
CATALOG_TOKEN_FILE=/run/secrets/catalog-read-token \
  catalog-client/bin/catalog-shadow \
  --base=/approved/read-only/base.json \
  --output=runtime/catalog-shadow.json \
  --max-age=1h \
  --sing-box=/approved/bin/sing-box
```

- `CATALOG_ADDR`: required `host:port`, without a scheme. TLS is the default,
  with system trust roots and hostname verification, minimum TLS 1.2.
- `CATALOG_TOKEN` or `CATALOG_TOKEN_FILE`: exactly one nonempty bearer token
  source. Prefer a mounted file. No token command-line flag and no `.env` loading.
- `--allow-insecure`: explicit plaintext opt-in for `localhost` or loopback IPs.
  Other hosts additionally require `--trusted-network`, an operator assertion,
  not an automatic private-IP exemption. Plaintext sends the token unencrypted.
- `--base`: required; no default production file is opened implicitly.
- `--output`: defaults to `runtime/catalog-shadow.json`. Only the basename
  `catalog-shadow.json` is permitted. Base-file identity, output symlinks and
  symlink ancestors are rejected. Active/current/generated/rollback filenames
  are therefore not writable. The directory must already exist and must not be
  concurrently modified by untrusted users. Do not configure a live router to
  consume the reserved shadow filename; filesystem guards cannot discover an
  arbitrary external service's configuration.
- `--max-age`: positive duration, at least one second; sent as whole seconds.
- `--sing-box`: trusted validator executable, defaults to `sing-box` on PATH.
  Use the deployed sing-box version. Execution is `check -c <temporary-file>`,
  bounded to 15 seconds; validator stdout/stderr are discarded.

The request is exclusively `GetSnapshot(profile="server", limit=50, max_age_seconds)`
with a 15-second RPC deadline. The response must have a nonempty ID, the exact
profile, 1..50 nodes, current creation/expiration times, and fresh successful
checks on every node with schema version 1. Future timestamps are rejected; clocks
must be synchronized. Freshness is checked again after config validation.

The authoritative URI is parsed with a frozen copy of the current router parser:
VLESS, Trojan, Hysteria2 and modern Shadowsocks, including supported TLS, Reality,
WS and gRPC options. Production code is unchanged. Unsupported or invalid nodes
fail the entire shadow attempt rather than silently skipping them. Parsed nodes
are deduplicated and prepended to `telegram-auto` and `default-auto` selectors;
other base content is preserved. Catalog metadata never overrides URI fields.

Candidates use a same-directory temporary file, initially mode 0600. Only a
successfully validated, still-fresh candidate is renamed atomically to the
output, mode 0640. Any pre-publication failure preserves the previous output and
removes the temporary file. No last-known-good shadow fallback is auto-activated.
The candidate contains credentials: restrict directory/group access, do not
upload it to CI artifacts, logs, tickets, or source control.

## Contract Ownership

`api/proto/catalog/v1/catalog.proto` is a verbatim vendored copy of the canonical
`subscription-catalog/api/proto/catalog/v1/catalog.proto`, owned by the catalog
repository. Do not edit the owner's schema here. Updates require an explicit
reviewed copy and regeneration, not a build-time sibling read.

```sh
make -C catalog-client generate
```

Generation requires protoc 34.0 and installs pinned `protoc-gen-go` v1.36.6 and
`protoc-gen-go-grpc` v1.5.1 into ignored module-local `.tools/`. Both invocations
use a local `Mcatalog/v1/catalog.proto=.../internal/gen/catalogv1` mapping, keeping
the owner's `go_package` intact. Generated files are committed with the module.
Ordinary build/test never invokes protoc or accesses the canonical checkout.

## Observability And Migration

The CLI emits JSON slog events with service identity, shadow mode, context-aware
call sites, timestamp and severity. Success reports only a 16-hex SHA-256 digest
of the opaque snapshot ID (`version`) and the deduplicated node `count`. Failures
use fixed stage messages; endpoints, tokens, RPC status text, IDs, URIs, config
payloads, and validator diagnostics are never reported. No OTel SDK/exporter is
enabled for this one-shot tool. Trace propagation/export and collector correlation
are deferred until a concrete caller/collector contract exists; context deadlines
alone are not trace propagation.

Staged migration, with no active rollout in this change:

1. Review the vendored contract and synthetic fixture checks in CI.
2. Separately authorize a read-only shadow run using a restricted catalog token,
   approved base copy and separate output. Keep submgr as the only live generator.
3. Compare candidate behavior in an isolated environment without publishing raw
   configs. Observe freshness failures, unsupported nodes, counts and versions.
4. Any activation mechanism, deploy/Compose change, ownership transfer or rollback
   integration requires a separate reviewed change. Never copy shadow output into
   the active path as part of this tool. Stopping shadow runs requires no rollback
   of the live router because it was never changed.
