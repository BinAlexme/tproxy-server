# Public probing hardening

The security invariant is that a request without an authentic relay credential
uses the same public handler on every path. Public applications may naturally
return different responses for different routes. The relay must not strip bodies,
authorization, cookies, or WebSocket headers merely because a route resembles a
carrier endpoint.

## Assessment of the proposal

- **Correct:** the previous `sanitizedPublicFallback` was a reserved-path oracle.
  It is removed. Public requests preserve method, Host, raw query, headers, body,
  and upload trailers under normal HTTP reverse-proxy semantics.
- **Correct:** token provenance must survive expiration and restart. Tokens now
  contain a random nonce and a kind-separated 128-bit HMAC tag, under an independent
  persistent key. The encoded length remains 43 characters. A valid MAC never
  substitutes for a live bootstrap/session lookup or protocol validation.
- **Correct:** genuine secrets in malformed metadata must stay local. Recognition
  runs before canonical routing, checks all URL/header values (including duplicate
  fields and percent escapes), and recognizes either token kind on any path.
  Authentic failures are local 404s with `Cache-Control: no-store`.
- **Correct:** carrier body deadlines belong after authentication. Public traffic
  retains ordinary gateway/listener timeouts; authenticated body, batch, replay,
  stream, and WebSocket limits remain in place.
- **Correct:** public static responses should not share a relay-specific policy
  bundle. They now use per-file timestamps, ETags, and standard `ServeContent`
  conditional/range behavior. New configurations use exact paths; old configs
  retain aliases to avoid breaking deployed sites. A custom 404 is recommended,
  not made mandatory for existing deployments.
- **Correction:** `header_down -Via` does not remove Caddy 2.11.4's own banner.
  The pinned [Caddy source](https://github.com/caddyserver/caddy/blob/v2.11.4/modules/caddyhttp/reverseproxy/reverseproxy.go)
  adds it to the downstream writer, then applies `header_down` to the upstream
  response. The supplied Caddyfile instead uses the outer `header -Via` operation,
  whose deletion is deferred. A real-binary test verifies the result. Stock
  Caddy branding on redirects and locally generated errors remains coherent;
  no nginx or CDN headers are forged. The shared error-policy bundle is removed.
- **Correction:** a same-origin public app is trusted, not isolated by the bridge
  having its own CSP. Existing service workers can intercept navigation before
  that CSP is received. See the [browser service-worker model](https://developer.mozilla.org/en-US/docs/Web/API/Service_Worker_API/Using_Service_Workers)
  and the trust requirements in [PUBLIC_SITE.md](PUBLIC_SITE.md).
- **Limit:** literal byte-for-byte equivalence for every malformed HTTP message
  is not attainable through an additional HTTP proxy. Hop-by-hop fields, framing,
  parser limits, HTTP version conversion, failure layers, and timing remain
  observable. Real Caddy tests also found an occasional extra empty gzip block
  when the extra proxy flushes headers sooner; decoded content and headers match.
  Buffering the website to force identical compressed bytes would harm streaming
  and upload behavior. This change removes path-dependent relay decisions; it does
  not conceal Caddy, Go, the IP address, or site ownership.

The metadata scan does not consume request bodies or late trailers, recursively
decode arbitrary encodings, or search encrypted data for credentials. Carrier
credentials must remain in their specified Authorization, subprotocol, and bridge
query locations. The broader metadata scan contains common malformed placements;
it is not a general data-loss-prevention filter.

## Client compatibility and the first token migration

Capability derivation, bridge URL, IPC messages, binary frames, headers, paths,
and all four carrier modes are unchanged. Existing apps consume opaque tokens and
need no code update. As before, a server restart discards sessions; the app reloads
the bridge and recreates streams. Signed tokens from a prior run remain recognizable
as internal even though their state is gone. Back up the signing key; replacing it
loses this provenance. Profile rotation does not rotate the independent token key.

There is an unavoidable first-upgrade exception: old random tokens have no MAC,
and the old process did not persist their identities. After its restart, they
cannot be distinguished from random public input. Claiming otherwise, or guessing
provenance solely from 43-character syntax, would preserve the probing oracle.

For an existing deployment without the default signing key, `deploy/update-relay.sh`
automatically enables a temporary drain phase in the dedicated
`/etc/systemd/system/tproxy-server.service.d/token-migration.conf` drop-in before
provisioning the key and restarting. For a custom or pre-provisioned key on a
legacy deployment, configure this environment setting yourself before the update:

```ini
[Service]
Environment=TPROXY_LEGACY_TOKEN_DRAIN=1
```

For a manually created drop-in, run `sudo systemctl daemon-reload`, then the
updater. The new server issues signed
tokens but rejects legacy-shaped bearer and carrier WebSocket credentials locally
unless they authorize a live operation. Old clients fail and reload normally.
The environment variable is ignored by an older binary if rollback is needed;
no unsupported JSON field is added to the old configuration.

Keep this phase until existing clients have reloaded the bridge, including dormant
browser pages. There is no time interval that proves an arbitrarily suspended
old page is gone. For the automatically created drop-in, finish with:

```bash
sudo rm /etc/systemd/system/tproxy-server.service.d/token-migration.conf
sudo systemctl daemon-reload
sudo systemctl restart tproxy-server
```

For a manual drop-in, remove only its drain setting instead. This restart enables
complete public pass-through. Drain mode deliberately
retains a credential-shape probing signal; the startup log reports whether it is
enabled. Fresh installations and later signed-token upgrades need no drain phase.

The installer and updater create `/etc/tproxy-server/token.key` only if absent,
using 32 bytes from the OS random source, owned by `tproxy`, mode `0400`. The default
path is next to config.json, so existing configs need no edit. Startup fails for a
missing, incorrectly sized, or group/other-accessible key. An optional custom
`token_key_file` is resolved relative to the config; provision that path separately
with `sudo bash deploy/ensure-token-key.sh /path/to/token.key`. Never regenerate
the key on each startup, derive it from the public bridge capability, or store it
under `public_dir`.

## Caddy migration

The relay updater does not overwrite operator Caddy configuration. Merge these
changes into the installed site block:

```caddyfile
header {
    -Via
    Strict-Transport-Security "max-age=31536000; includeSubDomains"
}
```

Remove the common CSP, Permissions-Policy, Referrer-Policy, X-Frame-Options, and
X-Content-Type-Options from `handle_errors`. Keep its no-store response and HSTS.
Retain operator-specific website policies in the public application, not around
the bridge response. Back up the current file, validate with the installed Caddy
binary and the same `TPROXY_HOSTNAME`/`ACME_EMAIL` environment as the service, then
restart Caddy. The supplied unit has its admin API disabled and no `ExecReload`;
`caddy reload` is not a valid deployment step for that unit. Restore the backup
if validation or restart fails. This repository update does not deploy remotely.

## Verification

```bash
go test ./...
TPROXY_CADDY_BIN=/path/to/caddy-2.11.4 go test -race ./...
go vet ./...
bash -n deploy/install.sh deploy/update-relay.sh deploy/ensure-token-key.sh
```

The opt-in Caddy suite adapts the actual deployment Caddyfile, uses ephemeral
loopback listeners and test certificates, and compares Caddy → application with
Caddy → relay → application over HTTP/1.1 and HTTP/2. It checks application-observed
metadata and bodies, chunked uploads and trailers, random/duplicate bearer values,
malformed queries, cookies, conditional/range headers, response policies/trailers,
gzip decoded-content parity and gzip/zstd negotiation (not compressed block
identity), HTTP/1.0, unusual Host/SNI combinations, malformed upgrades,
backend-down behavior, public WebSockets on transport paths, Via removal, and all four authenticated
carrier round trips. Unit tests cover secret containment, expiry/restart/tampering,
static conditionals/ranges, body deadlines, legacy draining, replay, backpressure,
and capacity limits. These tests are not a complete TLS/parser fingerprint audit.
