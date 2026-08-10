# Telegram WEB Proxy server plan

This repository implements the hosted half of the WEB proxy transport already
implemented in `../tproxy`. The first deployable version is deliberately narrow:

- the public hostname serves an HTTPS website;
- an exact query value derived from the hostname and MTProxy secret selects the
  bridge document;
- the bridge uses ordinary same-origin HTTPS `fetch` requests;
- the Go relay multiplexes logical streams onto a stock official MTProxy running on
  the same host;
- Caddy listens on ports 80/443; the relay and MTProxy backend use local listeners.

The project adds a browser-based proxy type so Telegram Desktop can stay connected
on networks where MTProto works unreliably. It does not implement a custom MTProto
server. Telegram Desktop performs the existing MTProxy transformation before bytes
reach the browser, and this server forwards those bytes to an unmodified MTProxy.

## 1. Reviewed architecture

```text
                                      one Linux server
                            +----------------------------------+
Telegram Desktop            |                                  |
  MTProto + MTProxy bytes    |  Caddy :443                      |
          |                  |  +----------------------------+  |
          v                  |  | public static website      |  |
local parent page            |  | bridge response            |  |
  http://127.0.0.1:<port>    |  | reverse proxy /api/v1/*    |  |
          | MessageChannel   |  +-------------+--------------+  |
          v                  |                |                 |
real browser iframe -------- HTTPS ----------+                 |
  https://site.example/      |                v                 |
  ?bridge=<capability>       |  Go relay 127.0.0.1:8080        |
                            |    session + frame mux            |
                            |                | TCP per stream    |
                            |                v                   |
                            |  official MTProxy :2398            |
                            |                |                   |
                            +----------------+-------------------+
                                             v
                                       Telegram DCs
```

External network ownership is intentional:

- Telegram Desktop opens only its loopback connection.
- The real browser owns the external TLS connection and DNS lookup.
- Caddy owns the public certificate and HTTP/1.1 + HTTP/2 behavior.
- The Go relay listens on loopback and never terminates public TLS itself.
- The official MTProxy is reached only through a local TCP connection.

## 2. Corrections to the initial plan

The initial plan had the right mux and browser-side concept, but it mixed the first
deployable server with several future products. This revision fixes the following:

1. The public root serves the site unless the exact bridge query selects the bridge
   page. Production uses the root query rather than a separate `/bridge` route.
2. A domain-separated HMAC of the hostname and MTProxy secret derives the bridge
   query value. Browser inputs contain that derived value rather than the MTProxy
   secret.
3. HTTPS long-poll is the only v1 carrier. WebSocket, streaming fetch, H3,
   Telegram Web integration, calls, and cross-process session resume are deferred.
4. Caddy terminates TLS and serves the public site. The Go process is an internal
   application server.
5. The official MTProxy `-H` and `-p` arguments are ports, not bind addresses.
   `-H 127.0.0.1:2398` from the old draft is invalid. V1 uses `-H 2398` and limits
   the port to local traffic with host and provider firewall rules.
6. M1 includes reliability, windows, memory limits, and resource limits. They cannot be
   postponed until after bulk traffic because the current client already relies on
   them for correctness.
7. The HTTP request semantics, retry rules, ownership, shutdown behavior, and
   browser bootstrap are specified below rather than left to implementation.

## 3. Public website and bridge

### 3.1 Public root behavior

The hostname given to users is also the TLS SNI hostname. Its root behavior is:

- `GET /` without the exact bridge capability returns the static home page.
- Missing, incorrect, duplicated, malformed, or extra query parameters also return
  the same home page with status `200`.
- Static pages and assets include at least `/about`, `/privacy`, `/favicon.ico`,
  `/robots.txt`, CSS, and a normal custom `404` page.
- Public HTML and assets are separate from the dynamically generated bridge page.
- The site uses content owned by the operator.
- Public responses are cacheable where appropriate and use a normal site CSP.
- HTTPS on the configured hostname does not redirect to another hostname; otherwise
  the parent's exact `postMessage` target origin would no longer match.

The static site can initially be small and lets the same hostname host regular web
content alongside the browser transport.

### 3.2 Derived bridge URL

Users configure exactly two values in Telegram Desktop:

```text
Hostname: site.example
Secret:   <existing MTProxy secret>
```

Telegram Desktop fixes the scheme and port to HTTPS/443 and constructs:

```text
https://site.example/?bridge=<derived-capability>
```

The raw MTProxy secret is never used as the query value. Both sides derive the
capability from canonical inputs:

```text
S = decoded WEB secret bytes, including a leading dd mode byte when present
H = lowercase ASCII/IDNA A-label hostname, without a trailing dot
context = UTF-8("tdesktop-web-proxy-bridge-v1\n" + H)
capability = base64url-no-padding(HMAC-SHA256(key=S, message=context))
```

Normative test vectors:

| Hostname | Decoded secret hex | Capability |
|---|---|---|
| `proxy.example.com` | `000102030405060708090a0b0c0d0e0f` | `MHLEY5PmW1GWqJkSrlmJpvJUiLhBH_QKy6yKg8a0JPk` |
| `proxy.example.com` | `dd000102030405060708090a0b0c0d0e0f` | `IpJrt3e7sKtzPyoXy6w-Zj6GGEvsvclN66JzQEfPYLA` |

Hex and base64url spellings that decode to identical bytes produce the same
capability. Plain and `dd` credentials intentionally produce different capabilities.
The derived value is stable for canonical hostname and secret inputs, distinct for
different inputs, and lets the server select a profile without putting the MTProxy
secret in the query.

At startup the relay loads one or more WEB profiles from configuration,
canonicalizes each profile secret, derives its fixed 32-byte capability for the
configured public hostname, and rejects collisions. A profile maps the matching
request to a fixed local MTProxy listener and optional limits. The bridge response is
selected only for an exact `GET /` containing exactly one 43-character `bridge`
parameter. The server decodes it as canonical unpadded base64url, requires exactly 32
bytes, and performs a constant-time scan across a configured, bounded profile list.

This permits one normal hostname to support several independently rotatable MTProxy
secrets or policies without adding another field to Telegram Desktop. Rotating the
MTProxy secret also rotates the derived bridge capability.

The query is part of the HTTPS request and is used only to select the profile and
bridge response. The limits in section 10 apply to every resulting session.

On a valid request, the relay selects the matched profile, returns a dynamic,
`no-store` bridge document, and mints a 256-bit bootstrap token bound to that profile.
The token is embedded only in that response, expires after two minutes, and is
idempotently consumable for one session creation. Retrying the same creation request
after an ambiguous network failure returns the same session result during the
bootstrap lifetime.

The bridge document immediately removes the query from its visible iframe URL with
`history.replaceState`. Reloading then receives the ordinary public site. This is
intentional and matches the desktop client's one-shot browser-tab lifecycle.

### 3.3 Telegram Desktop contract

The current tdesktop implementation follows this contract:

1. accept only a canonical DNS hostname and an existing MTProxy secret for WEB;
2. reject a scheme, port, path, query, fragment, user info, IP address, single-label
   name, invalid IDNA, or hostname longer than DNS limits;
3. store the lowercase ASCII/IDNA A-label hostname and effective port `443`;
4. construct `relayOrigin = https://<hostname>`;
5. derive the exact `bridge` query using section 3.2 and construct the bridge URL in
   memory, without persisting or displaying the derived capability;
6. use the root query URL as `bridgeUrl` instead of appending `/bridge`;
7. show only the hostname in proxy rows and sanitized logs;
8. when the authenticated local WebSocket closes, send `{t:'close'}` to the iframe;
   the bridge closes the relay session and then its MessagePort so a replaced tab
   cannot keep an orphan relay session.

The MessageChannel target remains the exact HTTPS origin. The browser page removes
the derived capability from its visible URL and application logs omit it. A separate
`/bridge` compatibility route may exist in local development builds, but it is not
part of the production service configuration.

## 4. TLS and reverse-proxy layout

Caddy is the only public listener:

```text
TCP 80   Caddy ACME redirect/challenge
TCP 443  Caddy HTTPS site and reverse proxy
TCP 8080 Go relay, 127.0.0.1 only
TCP 2398 official MTProxy client port, limited to loopback traffic
TCP 8888 official MTProxy statistics port
```

Caddy responsibilities:

- obtain and renew a normal certificate for the public hostname;
- negotiate HTTP/1.1 and HTTP/2 normally;
- apply standard response headers;
- reverse proxy exact path `/` to Go so Go can choose public index versus bridge;
- reverse proxy `/api/v1/*` to Go;
- serve all other public pages/assets from the static directory;
- serve the static index through Caddy's error handler if the root Go upstream is
  unavailable, so the public root remains available during relay restarts;
- preserve streaming/long-poll responses and use a timeout longer than the relay's
  25-second poll hold;
- never enable access logging that records raw URIs, query strings, Authorization
  headers, or request bodies.

The bridge-specific response must override public-site framing policy:

```text
Content-Security-Policy:
  default-src 'none';
  script-src 'nonce-<per-response nonce>';
  connect-src 'self';
  frame-ancestors http://127.0.0.1:*;
  base-uri 'none';
  form-action 'none'
Cache-Control: no-store
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
Permissions-Policy: camera=(), microphone=(), geolocation=()
```

Do not send `X-Frame-Options`, COOP, COEP, or another header that prevents the
loopback page from embedding the bridge. The public site may use stricter framing
headers because it is a different response.

Do not put a CDN in front of v1. Long polls, in-memory session affinity, request
buffering, provider terms, and CDN behavior are separate design work. DNS should
point directly to this server for the first field tests.

## 5. Browser bridge contract

The bridge is a small inline script in the dynamic HTML response. Keeping it inline
keeps the page and carrier implementation versioned together without another asset.

The desktop parent sends the existing initialization message:

```javascript
iframe.contentWindow.postMessage(
  { t: 'tproxy-init', v: 1 },
  'https://site.example',
  [channel.port2]);
```

The bridge accepts initialization once and validates all of the following:

- `event.source === window.parent`;
- `event.data` is exactly the supported init shape;
- exactly one `MessagePort` is transferred;
- `event.origin` parses as `http://127.0.0.1:<explicit-port>`;
- the port is not already initialized.

After initialization:

- parent-to-iframe `ArrayBuffer` messages are complete carrier payloads from
  tdesktop;
- iframe-to-parent `ArrayBuffer` messages are complete relay response batches;
- `{t:'status', state:'connecting|connected|reconnecting|failed'}` reports state;
- iframe-to-parent `{t:'close'}` says the relay session cannot resume;
- parent-to-iframe `{t:'close'}` says the local tdesktop transport was replaced or
  stopped and the bridge must close its relay session.

The bridge does not parse the shared frame protocol. Its first upstream binary
message is sent verbatim as the session-creation body; the relay validates that it is
the v1 `HELLO`. Later binary messages are serialized and batched without inspecting
their contents.

Bridge behavior:

1. Wait for the MessageChannel init and first upstream buffer.
2. `POST /api/v1/session` with the bootstrap token and first buffer.
3. Forward the returned `WELCOME` body to the parent.
4. Run one serialized uplink queue and one serialized long-poll loop.
5. Retry ambiguous HTTP failures with the same sequence/cursor and byte-identical
   body, using bounded exponential backoff with jitter.
6. Report `reconnecting` during transient carrier loss.
7. Report `failed`, then `close`, for expired sessions, protocol failures, or limits
   that cannot be recovered within the same relay session.
8. On parent close or page shutdown, stop polling and make a best-effort authenticated
   session-close request. Server idle cleanup remains the fallback.

There is no browser WebSocket in v1. The browser makes ordinary same-origin HTTPS
POST requests only.

## 6. HTTP carrier protocol

All API requests require:

- exact public `Host` as forwarded by the local Caddy instance;
- `Origin: https://site.example`;
- `Content-Type: application/octet-stream` when a binary body is present;
- no cookies and no CORS response headers;
- request bodies limited before allocation.

Invalid or missing bearer credentials return the site's `404` response.

### 6.1 Create session

```text
POST /api/v1/session
Authorization: Bearer <bootstrap-token>
Body: exactly one HELLO frame
```

V1 `HELLO` is session frame `0x10`, stream zero, payload exactly `01`. The bootstrap
token is atomically exchanged for a relay session. A successful response is:

```text
200 OK
X-Session-Token: <256-bit base64url token>
X-Down-Cursor: 0
Cache-Control: no-store
Body: exactly one WELCOME frame
```

V1 `WELCOME` is session frame `0x11`, stream zero, with an empty payload. The HTTP
session token is intentionally outside the shared frame bytes because the iframe,
not tdesktop, owns the HTTP carrier.

The bootstrap token is an idempotency key for this one operation. Repeating the same
valid request while its cached result remains alive returns the same session token
and WELCOME. A different body or expired bootstrap is rejected.

### 6.2 Uplink

```text
POST /api/v1/up
Authorization: Bearer <session-token>
X-Up-Seq: <decimal sequence, starts at 1>
Body: one or more complete client-to-relay frames
```

Rules:

- the bridge sends at most one uplink request at a time;
- it coalesces only complete MessagePort buffers, targeting at most 2 MiB per
  request and never exceeding the 2 MiB request cap;
- the server accepts exactly the next sequence or an exact duplicate of the most
  recently committed sequence;
- the server commits the sequence only after all frames pass validation and are
  accepted by bounded stream queues;
- if a valid next batch cannot temporarily fit the DATA queue partition, the server
  applies none of it, leaves the sequence uncommitted, and returns retryable `503`
  with `Retry-After: 1`;
- a duplicate is acknowledged without applying its frames again;
- gaps, reordered sequences, or a duplicate with different bytes terminate the
  session as a protocol error.

A successful response is `204 No Content` with `X-Up-Ack` equal to the committed
sequence. V1 does not piggyback downlink bytes on uplink responses.

### 6.3 Downlink long-poll

```text
POST /api/v1/down
Authorization: Bearer <session-token>
X-Down-Cursor: <last fully delivered cursor, starts at 0>
Empty body
```

Only one down request may be outstanding per session. The relay:

1. treats the supplied cursor as acknowledgement of the previous batch;
2. returns any previously issued but unacknowledged batch byte-for-byte;
3. otherwise waits for frames, a batch limit, session close, or 25 seconds;
4. assigns the new nonzero cursor only when returning a nonempty batch.

Responses:

- `200` + binary body + new `X-Down-Cursor` for a frame batch;
- `204` + unchanged cursor on the 25-second empty timeout;
- terminal error/close when the session can no longer continue.

The bridge advances its cursor only after it has successfully transferred the entire
body to the MessagePort. A lost response is therefore requested again. Batch order is
strict, batches are never combined across cursors, and unacknowledged data remains
bounded by the session windows and queue caps.

### 6.4 Close session

```text
DELETE /api/v1/session
Authorization: Bearer <session-token>
Empty body
```

Close is idempotent. It cancels the active poll, closes every backend connection,
discards queued frames, invalidates the token, and returns `204 No Content`. The
bridge uses `fetch(..., {keepalive:true})` on parent close/page shutdown where the
browser permits it. If that best-effort request is lost, the ten-minute idle grace
still reclaims the session.

## 7. Shared frame protocol and MTProxy mapping

All integers are big-endian:

```text
type:u8 | stream_id:u24 | length:u32 | payload:length
```

V1 values matching the tdesktop implementation:

| Value | Name | Direction | Stream | Payload |
|---:|---|---|---:|---|
| `0x01` | `OPEN` | client to relay | >0 | empty |
| `0x02` | `DATA` | both | >0 | opaque bytes |
| `0x03` | `CLOSE` | both | >0 | empty in v1 |
| `0x04` | `WINDOW` | both | >0 | nonzero big-endian `u32` delta |
| `0x05` | `PING` | relay to client | 0 | opaque echo token |
| `0x06` | `PONG` | client to relay | 0 | exact token |
| `0x10` | `HELLO` | client to relay | 0 | one byte `01` |
| `0x11` | `WELCOME` | relay to client | 0 | empty |
| `0x1f` | `BYE` | relay to client | 0 | optional bounded reason |

`AUTH_CHAL` and `AUTH_RESP` are reserved but unsupported because the current desktop
client rejects them in v1.

Limits and validation:

- maximum frame payload: 1 MiB;
- relay-generated DATA chunk: at most 64 KiB;
- WELCOME is the first relay-to-client frame and has an empty payload;
- session frames must use stream zero and stream frames must use nonzero ids;
- stream ids cannot be reused within a live session;
- each side retains a bounded set of 4096 recently closed stream ids and discards
  well-formed late DATA, WINDOW, or CLOSE for those ids, covering cross-direction
  close races without affecting other streams;
- unknown live stream ids, unknown types, trailing partial frames in an HTTP body,
  arithmetic overflow, zero WINDOW, DATA beyond credit, or frames in the wrong
  direction are fatal protocol errors;
- an invalid frame never reaches the MTProxy backend.

### 7.1 Stream lifecycle

For each valid `OPEN`, the relay dials only its configured backend address,
`127.0.0.1:2398`. The client cannot supply or influence an address or port.
`OPEN` has no separate acknowledgement: tdesktop may send DATA immediately after it,
including in the same uplink batch. The session owner creates the stream before
dialing, queues at most the initial receive window while the backend dial is pending,
and emits `CLOSE` if the dial fails or times out.

```text
OPEN s=17      -> dial one backend TCP connection
DATA s=17      -> bounded write to that connection
backend read   -> DATA s=17 to downlink queue
CLOSE s=17     -> close backend connection
backend EOF    -> enqueue CLOSE s=17
dial failure   -> enqueue CLOSE s=17; keep the relay session alive
```

Do not use an unrestricted `io.Copy`: flow-control credit must gate reads and writes.

### 7.2 Flow control

Both directions begin with an implicit 4 MiB per-stream receive window.

- Client DATA consumes the relay receive window. The relay sends WINDOW only after
  those bytes have actually drained to the MTProxy socket.
- Backend reads consume relay send credit. The relay stops reading that backend
  socket when credit reaches zero and resumes only after a client WINDOW.
- Window addition uses checked/saturating arithmetic capped at `uint32` maximum.
- DATA is never read into an unbounded intermediate queue.

This mirrors tdesktop: it grants downlink credit only as its MTProto engine drains
bytes, and it obeys server-granted uplink credit.

### 7.3 Carrier performance envelope

Uplink and downlink run concurrently, but v1 deliberately keeps one sequenced HTTP
request active in each direction so retries are unambiguous. With the 2 MiB default
batch, the RTT-only upper bound for a continuously busy direction is `2 MiB / RTT`:

| Browser-to-relay RTT | RTT-only bound |
|---:|---:|
| 50 ms | 40 MiB/s |
| 100 ms | 20 MiB/s |
| 200 ms | 10 MiB/s |
| 500 ms | 4 MiB/s |

Transfer time, the relay-to-MTProxy path, and browser scheduling lower the measured
rate. The 4 MiB stream window is intentionally larger than one carrier batch, so a
single stream can continue while returned WINDOW credit crosses the opposite
carrier. Caddy compression is limited to static-site routes; encrypted carrier
bodies are never sent through zstd/gzip. On a controlled link with at least
100 Mbit/s capacity, the acceptance target is 20 Mbit/s at 500 ms RTT and 40 Mbit/s
at 200 ms RTT, in both a foreground and a normally hidden supported browser tab.

Telegram Desktop's built-in HTTP transport also uses HTTP request/response bodies
and an HTTP wait request, but it may keep several POSTs active. WEB therefore has
more RTT sensitivity in v1. That serialization, fixed batching, and the current
copy count are implementation choices rather than requirements of using a browser.
An ordered bounded pipeline or a compatible streaming carrier can narrow the gap.
The unavoidable differences are one browser process, one extra relay path and TLS
leg, browser/MessageChannel crossings, and shared-carrier head-of-line exposure.
Accordingly, ordinary messaging and moderate media should be comparable to the
built-in HTTP transport on a well-placed relay, while direct transport remains the
latency and peak-throughput reference.

## 8. Session and concurrency model

Each relay session serializes its maps, sequence/cursor state, windows, tombstones,
and byte accounting under one short-held session mutex. HTTP handlers and backend
goroutines perform parsing, dialing, and socket I/O outside that critical section;
they enter it only to validate or commit a state transition. The race-enabled test
suite covers the resulting handler/backend interleavings.

Per session state includes:

- a hashed session bearer token;
- last committed uplink sequence and duplicate digest;
- current downlink cursor and at most one unacknowledged batch;
- logical stream map and window counters;
- bounded pending control/data queues;
- active long-poll handle;
- last carrier activity and reconnect deadline;
- cancellation context closing all backend sockets and goroutines.

The bridge also bounds its pre-session, queued, and in-flight uplink data at 32 MiB
and 16384 retained buffers. It counts an in-flight request until acknowledgement, so
a slow upload cannot hide one full batch from the queue limit.

The browser may lose connectivity while the tab remains alive. The same session
survives carrier gaps for ten minutes, allowing retries after ordinary network loss
or a five-minute system sleep. When that grace expires, or when the relay process
restarts, live MTProxy TCP streams cannot be reconstructed safely. The bridge emits
`close`; the user uses `Open browser` in tdesktop to obtain a fresh local capability,
bridge page, and relay session.

Shutdown order:

1. Go stops admitting new carrier requests and gives active handlers a short drain
   deadline.
2. Long polls are cancelled, all sessions are invalidated, and backend sockets
   close.
3. The bridge treats the unavailable carrier as terminal and tells tdesktop to
   replace the browser session.
4. The process exits; systemd may restart it.

V1 does not emit BYE during process shutdown because the HTTP carrier is already
draining. BYE remains reserved for a future graceful carrier-level drain.

## 9. Official MTProxy backend

Build commit `f36d8af769ffaeac36978d38c2c0f6d1104c2137` of the official
[`TelegramMessenger/MTProxy`](https://github.com/TelegramMessenger/MTProxy) source
from the checksum-pinned archive and run it unchanged. Build it as the unprivileged
`mtproxy` user, then use root only to install the verified result. Its Makefile and timing/crypto implementation require
an x86_64 host; the automated installer fails before changing the server on another
architecture. A single-profile deployment uses:

```bash
/opt/MTProxy/objs/bin/mtproto-proxy \
  -u nobody \
  -p 8888 \
  -H 2398 \
  -S <32-hex-character-secret> \
  --aes-pwd /etc/mtproxy/proxy-secret \
  /etc/mtproxy/proxy-multi.conf \
  -M 1
```

Important details:

- `-H` is the client port and takes `2398`, not `127.0.0.1:2398`.
- `-p` is the statistics port; upstream documents it as loopback-accessible.
- Because official MTProxy does not document a host argument for `-H`, use both the
  provider network rules and local nftables/ufw rules: accept 2398 through `lo` and
  drop it on external interfaces.
- Configure only 80/443 for external connections. Verify from a second machine that
  2398 and 8888 are unreachable.
- The relay startup configuration accepts only a loopback backend address.
- Download `proxy-secret` and `proxy-multi.conf` from Telegram over HTTPS, check
  nonempty/expected formats, write atomically, and refresh `proxy-multi.conf` daily.
- A client may use the `dd` random-padding prefix. MTProxy still receives the base
  16-byte secret through `-S`; `dd` is a client-side mode prefix.
- Do not use an `ee` TLS-emulation secret. The browser-to-Caddy connection already
  uses TLS, and the current tdesktop WEB implementation rejects `ee`.

Before relay integration, prove the backend independently. Temporarily allow port
2398 only from the operator's test IP (or reach it through an SSH tunnel), configure
a normal MTProxy client with the base secret, connect, then remove the temporary
firewall rule. Also verify `127.0.0.1:8888/stats` and a local TCP dial to 2398.

The public site must remain healthy when MTProxy is down. The relay readiness check
reports backend failure only on a loopback-only admin endpoint; an `OPEN` receives
`CLOSE` rather than taking down Caddy or the static site.

### 9.1 Multiple secrets and profiles

Each derived capability selects one server profile before any streams open. For the
first deployment there is one profile and one local MTProxy listener.

If several secrets share exactly the same routing and quotas, one
official MTProxy process may accept them using repeated `-S` arguments. If a secret
must select a genuinely different usage, quota, operator tag, or backend, run one
official MTProxy process/listener per isolated profile:

```text
profile alpha -> 127.0.0.1:2398 -> MTProxy -H 2398 -S <alpha-base-secret>
profile beta  -> 127.0.0.1:2399 -> MTProxy -H 2399 -S <beta-base-secret>
```

This separation matters because the relay intentionally does not decrypt or inspect
the MTProxy handshake. If two secrets are accepted by one backend, that backend does
not tell the relay which secret was used, so separate per-profile accounting cannot
be maintained. A dedicated backend listener preserves the selected profile without
teaching the Go relay MTProxy internals.

## 10. Resource and operational limits

V1 is intended for controlled deployment but must be bounded before public testing.
Initial defaults are configurable and covered by tests:

| Resource | Default |
|---|---:|
| HTTP header bytes | 16 KiB |
| Binary request body | 2 MiB |
| Frame payload | 1 MiB |
| Relay DATA chunk | 64 KiB |
| Carrier batch target | 2 MiB |
| Initial stream window | 4 MiB |
| Logical streams per session | 128 |
| Recently closed stream ids per session | 4096 |
| Pending bytes per session | 32 MiB |
| Pending bytes process-wide | 512 MiB |
| Pending items per session | 16384 |
| Pending items process-wide | 262144 |
| Queue allocation charge | 256 bytes per item |
| Sessions per source IP | 4 |
| Live sessions process-wide | 128 |
| New sessions per source IP | 10/minute, burst 4 |
| Unused bootstraps per source IP | 8 |
| Bootstrap entries process-wide | 512 |
| New bootstraps per source IP | 30/minute, burst 8 |
| Backend dial timeout | 5 seconds |
| Long-poll hold | 25 seconds |
| Carrier reconnect grace | 10 minutes |
| Bootstrap lifetime | 2 minutes |

Additional requirements:

- derive bridge capabilities exactly as section 3.2, precompute their fixed 32-byte
  values, cap the profile count, compare every entry in constant time, and never log
  raw query input;
- store only hashes of bootstrap/session tokens in maps where practical;
- obtain client IP from Caddy's loopback connection and its single normalized
  forwarding header;
- use a read-header deadline, strict body-size caps, carrier request cancellation,
  idle deadlines, and graceful-shutdown deadlines; do not apply the short header
  deadline to a complete 2 MiB carrier upload;
- reject multiple concurrent uplinks or downlinks for one session;
- reserve room for one uplink batch plus byte and item headroom for relay-to-client
  control frames, coalesce pending WINDOW grants per stream, and pause backend DATA
  reads before they consume either reserve;
- cap goroutines and open backend connections through the session/stream limits;
- disable CORS and validate same-origin POST requests;
- never log bridge URLs, queries, bearer tokens, frame payloads, MTProxy bytes, or
  MTProxy secrets;
- avoid analytics or third-party resources on the bridge response;
- serve health, readiness, and metrics on a separate loopback admin listener, with
  pprof absent unless `enable_pprof` is explicitly set;
- restrict `/proc` visibility in every long-running service unit; the unchanged
  upstream `-S` interface still places the backend secret in MTProxy's argument
  memory, so host administrators must not add unconfined untrusted login users;
- log only sanitized event names, anonymous session ids, counts, byte totals,
  durations, error classes, and limit hits.

The derived bridge capability maps a request to its configured profile without
placing the MTProxy secret in the URL. The stock MTProxy secret remains the backend's
protocol credential, and the relay applies the configured rate, stream, and memory
limits to every session.

## 11. Repository layout

```text
tproxy-server/
├── PLAN.md
├── PROTOCOL.md
├── README.md
├── go.mod
├── cmd/
│   └── tproxy-server/main.go
├── internal/
│   ├── config/                 # strict config, secret-file loading
│   ├── frame/                  # codec and direction validation
│   ├── bridge/                 # dynamic HTML + inline JS
│   ├── session/                # seq/cursor, streams, flow control, budgets
│   └── server/                 # carrier, public site, admin handlers
├── web/
│   └── public/                 # static site and assets
├── deploy/
│   ├── Caddyfile
│   ├── caddy.service
│   ├── install.sh
│   ├── update-relay.sh
│   ├── tproxy-server.service
│   ├── mtproxy.service
│   ├── tproxy-firewall.service
│   ├── install-mtproxy.sh
│   ├── refresh-mtproxy-config.sh
│   ├── refresh-mtproxy-config.service
│   ├── refresh-mtproxy-config.timer
│   └── firewall.nft
└── *_test.go                   # colocated unit and fake-backend integration tests
```

Use Go's standard library where possible. A small maintained WebSocket dependency is
not needed in v1. The browser script is plain JavaScript with no package manager or
build step. Static files may be embedded for development, while production may read
the configured site directory so content can change without rebuilding the relay.

Configuration fields:

```text
public_hostname     site.example
listen              127.0.0.1:8080
admin_listen        127.0.0.1:8081
public_dir          /srv/tproxy-site
profiles_file       /run/credentials/tproxy-server.service/profiles.json
enable_pprof        false
limits              explicit values from section 10
timeouts            explicit values from sections 6, 8, and 10
```

Each profile contains a client-facing WEB secret, fixed loopback backend
address, and optional limit overrides. The relay derives capabilities at startup; no
bridge capability is stored in configuration.

Startup rejects an invalid public DNS hostname, non-loopback listen/admin or
profile backend address, invalid/duplicate profile secret or capability, missing
static site, or inconsistent limits.

## 12. Implementation sequence

### M0 — contracts and backend

1. Extract the frame and MessageChannel contract into `PROTOCOL.md` and cross-check
   it against `../tproxy/docs/web-proxy-plan.md` and the current C++ constants.
2. Verify the tdesktop hostname normalization, capability derivation vectors,
   bridge URL, and parent-close behavior from section 3.3 against the
   extracted contract.
3. Install official MTProxy, firewall 2398/8888, and complete the direct backend
   proof from section 9.

Exit: the stock backend works independently, the public ports are correct, and the
desktop client constructs the production bridge URL from its configured inputs.

### M1 — relay core with a fake backend

1. Scaffold config, frame codec, limits, and session owner loop.
2. Implement OPEN/DATA/CLOSE/WINDOW against the deterministic fake TCP backend.
3. Implement strict seq/cursor reliability and fault injection for lost/duplicated/
   reordered HTTP responses.
4. Add all memory, stream, session, and timeout limits before browser work.

Exit: integration tests move multiple concurrent byte streams without duplication,
loss, reordering, or unbounded memory.

### M2 — public site, bridge page, and browser carrier

1. Add the operator-owned static site and Caddy configuration.
2. Implement root capability selection, public-site response, dynamic bridge CSP,
   bootstrap issuance, and sanitized logs.
3. Implement MessageChannel validation and the serialized HTTPS carrier.
4. Test wrong/missing query behavior, token idempotency, browser refresh, carrier
   retry, parent/tab replacement, network loss, and ten-minute session grace.

Exit: normal root requests return the site, while a matching bridge request
establishes a reliable relay session using browser HTTPS.

### M3 — official backend end to end

1. Point the relay at `127.0.0.1:2398`.
2. Run Telegram Desktop through WEB for login, messages, updates, media, multiple
   accounts, browser loss, sleep/wake, and network loss.
3. Execute `../tproxy/docs/web-proxy-test-plan.md` against the deployed hostname.
4. Capture sanitized ownership evidence showing Telegram Desktop connects only to
   loopback, the browser owns external 443, and the relay owns backend 2398.

Exit: the full server-ready desktop test plan passes on a normal network and on a
network where direct MTProto is unreliable.

### M4 — deployment and operations

1. Add systemd credentials, service isolation, restart policy, graceful shutdown,
   config refresh timer, backup/rollback instructions, and firewall verification.
2. Add loopback metrics, resource alerts, soak tests, and upgrade procedures.
3. Run a 24–72 hour soak with media traffic and injected carrier/backend failures.

Exit: bounded resource use, recoverable operations, and documented deployment.

## 13. Test inventory and acceptance criteria

Unit tests:

- every frame type and invalid direction/stream/length combination;
- partial parser input, concatenated frames, 24-bit ids, checked window arithmetic;
- hostname canonicalization and both normative capability derivation vectors;
- query capability profile matching and ordinary-site fallback;
- bootstrap idempotency, expiry, token hashing, and constant-time comparison paths;
- uplink duplicate/gap/body-digest handling;
- downlink cursor replay and acknowledgement;
- session and stream limit accounting;
- shutdown and cancellation without lingering goroutines.

Integration tests with the fake backend:

- concurrent streams with one slow stream and one active stream;
- bidirectional multi-megabyte deterministic payload hashes;
- lost response after server commit, duplicate POST, dropped poll, delayed poll;
- backend connect refusal, EOF, half-close, slow read/write, and process restart;
- simultaneous client close/backend EOF with delayed DATA, WINDOW, and CLOSE;
- browser-equivalent HTTP/1.1 and HTTP/2 behavior through Caddy;
- invalid auth/origin/content type/body size/header size and slow header delivery;
- memory stays inside configured budgets under high-fragmentation authenticated input.

Hosted acceptance:

- public `/`, wrong bridge queries, assets, and 404s form a coherent normal site;
- only 80/443 are externally reachable;
- logs omit request addresses, payloads, tokens, and secrets;
- the bridge response is embeddable only by numeric loopback origins;
- compare WEB with direct Telegram and ordinary MTProxy on a network where MTProto
  is unreliable;
- the current tdesktop test plan passes, including media, concurrency, lifecycle,
  loopback validation, browser matrix, and unreliable-MTProto network checks.

## 14. Explicit non-goals for v1

- a separate production `/bridge` route or API access without session credentials;
- running official MTProxy as a separate public service;
- use of the raw MTProxy secret by browser JavaScript;
- browser WebSocket, streaming fetch, HTTP/3, WebTransport, or CDN fronting;
- Telegram Web A/K integration;
- calls or UDP relay;
- client-selected backend destinations;
- cross-tab, cross-process, or relay-restart stream resume;
- `AUTH_CHAL` / `AUTH_RESP` relay authentication;
- multiple public domains in one relay process.

Those can be reconsidered after the narrow long-poll deployment works in the target
network. They must not delay or complicate the first server.
