# WEB proxy protocol v1

This is the client-independent wire contract shared by a WEB-capable Telegram app,
the HTTPS bridge page, and `tproxy-server`. All binary integers are unsigned and
big-endian.

A client applies Telegram's normal MTProxy transform first. Its local WEB adapter
maps every resulting TCP connection to a logical stream and multiplexes those
streams through one WebView carrier session. The bridge page converts complete
shared-frame batches into authenticated HTTPS requests; the relay converts each
logical stream back into one TCP connection to its configured stock MTProxy. DATA
payloads are opaque at every layer after the app's MTProxy transform.

## Bridge URL

The user configures a canonical lowercase ASCII/IDNA hostname `H` and an MTProxy
secret. Decode the secret to bytes `S`, retaining the leading `dd` byte when random
padding mode is selected, then derive:

```text
context = UTF-8("tdesktop-web-proxy-bridge-v1\n" + H)
bridge = base64url-no-padding(HMAC-SHA256(key=S, message=context))
URL = https://H/?bridge=bridge
```

Vectors:

| Host | Secret bytes in hex | Bridge capability |
|---|---|---|
| `proxy.example.com` | `000102030405060708090a0b0c0d0e0f` | `MHLEY5PmW1GWqJkSrlmJpvJUiLhBH_QKy6yKg8a0JPk` |
| `proxy.example.com` | `dd000102030405060708090a0b0c0d0e0f` | `IpJrt3e7sKtzPyoXy6w-Zj6GGEvsvclN66JzQEfPYLA` |

Only an exact `GET /` with one canonical 43-character `bridge` parameter selects
the bridge. Every other root query returns the normal public index.

`tdesktop-web-proxy-bridge-v1` is a frozen v1 domain-separation label. Its name is
retained for compatibility and does not restrict the protocol to Telegram Desktop.

## Client-to-bridge boundary

The bridge supports two ways to connect a Telegram app to the same carrier logic.
A normal native implementation uses the injected WebView boundary. The loopback
parent boundary is retained for clients that deliberately use a system browser as
a carrier or fallback. Neither boundary changes the HTTP or shared-frame protocol.

### Injected WebView boundary

The app loads the bridge document as the WebView main frame and appends a
client-only fragment:

```text
https://H/?bridge=bridge#android=webview-nonce
```

`webview-nonce` is 32 random bytes in canonical unpadded base64url form. The
`android` fragment key is a frozen v1 compatibility name used by all current
reference clients; it does not identify the client platform. URL fragments are not
sent in the HTTPS request.

Before navigation, the app exposes a page object named `TelegramWebProxy` only to
the exact `https://H` main frame. The object has a `postMessage(value)` method used
by the page and an `onmessage` callback used by the app. The platform binding must
authenticate the active WebView, main frame, exact origin, current navigation, and
nonce. Wildcard origins and unrestricted JavaScript interfaces are not conforming.

The bridge removes the query and fragment with `history.replaceState`, adapts the
object to its internal port contract, and sends this JSON control value:

```json
{"t":"tproxy-android-init","v":1,"nonce":"webview-nonce"}
```

`tproxy-android-init` is also a frozen v1 compatibility name. The app accepts it
only when the nonce and authenticated WebView context match. Messages from the
bridge to the app are either JSON control values or one complete shared frame.
Messages from the app to the bridge use the same representations. A platform may
use ArrayBuffer, a shared native buffer, or a private base64 envelope across its
WebView IPC boundary; that private encoding is removed before the value reaches the
bridge and is not part of the HTTP carrier or shared-frame format.

The bridge splits an aggregated HTTP downlink body at validated frame boundaries
before sending frames through this direct WebView boundary. Clients should keep
DATA frames at or below the relay's 64 KiB chunk size.

Each platform binds this object through its origin-scoped native WebView API. Some
bindings carry ArrayBuffers directly; others use a private string/base64 or shared
buffer shim. The platform documents linked below describe those implementation
choices without changing this boundary.

### Loopback parent boundary

A client-controlled loopback page may embed the HTTPS bridge page and transfer one
`MessagePort`:

```javascript
iframe.contentWindow.postMessage(
  {t: 'tproxy-init', v: 1},
  'https://proxy.example.com',
  [channel.port2]
);
```

The bridge accepts this once, only from its parent, only with the exact object and
one port, and only when `event.origin` is an explicit
`http://127.0.0.1:<port>` origin. Binary MessagePort messages are complete carrier
batches. Control objects are `{t:'status',state}`, diagnostic
`{t:'traffic',up,down}` byte counts, and `{t:'close'}`. Traffic counts are
nonnegative numbers describing the completed carrier operation; clients may
discard them after validation.

## HTTP carrier

All API requests have `Origin: https://H`, no cookies, and a bearer token. Binary
bodies use exactly `Content-Type: application/octet-stream`.

Session creation exchanges a two-minute bootstrap token atomically and
idempotently:

```text
POST /api/v1/session
Authorization: Bearer bootstrap-token
Body: one HELLO frame

200 OK
X-Session-Token: session-token
X-Down-Cursor: 0
Body: one WELCOME frame
```

After a valid bootstrap is authenticated, temporary session-capacity or
creation-rate exhaustion returns `503 Service Unavailable` with `Retry-After: 1`.
The bootstrap remains unconsumed so the byte-identical creation request can retry.

Uplink requests are serialized. `X-Up-Seq` begins at `1`. The relay accepts the
next sequence or a byte-identical retry of the last committed sequence:

```text
POST /api/v1/up
Authorization: Bearer session-token
X-Up-Seq: 1
Body: one or more complete frames

204 No Content
X-Up-Ack: 1
```

If the next valid batch cannot yet fit the relay's DATA queue budget, the relay
returns `503 Service Unavailable` with `Retry-After: 1`. The sequence remains
uncommitted and no frame from the batch is applied. The bridge retries the same
sequence with the byte-identical body.

One downlink poll may be active. The cursor acknowledges a previously delivered
batch. Repeating the old cursor replays the unacknowledged batch byte-for-byte:

```text
POST /api/v1/down
Authorization: Bearer session-token
X-Down-Cursor: 0
Empty body

200 OK                       204 No Content
X-Down-Cursor: 1             X-Down-Cursor: 0
Body: complete frame batch   Empty body
```

`DELETE /api/v1/session` with the session bearer closes all streams and is
idempotent for a currently authenticated session. Tokens are 32 random bytes in
canonical unpadded base64url form. Missing or invalid credentials receive the
site's ordinary 404 response.

## Shared frames

```text
type:u8 | stream_id:u24 | payload_length:u32 | payload
```

| Value | Name | Direction | Stream | Payload |
|---:|---|---|---:|---|
| `0x01` | `OPEN` | client → relay | nonzero | empty |
| `0x02` | `DATA` | both | nonzero | opaque, nonempty |
| `0x03` | `CLOSE` | both | nonzero | empty |
| `0x04` | `WINDOW` | both | nonzero | nonzero `u32` delta |
| `0x05` | `PING` | relay → client | zero | opaque echo token |
| `0x06` | `PONG` | client → relay | zero | exact echo token |
| `0x10` | `HELLO` | client → relay | zero | byte `01` |
| `0x11` | `WELCOME` | relay → client | zero | empty |
| `0x1f` | `BYE` | relay → client | zero | optional bounded reason |

## Client stream lifecycle

The Telegram app, not the bridge JavaScript, prepares the shared frames:

1. After the client boundary is authenticated, the client sends one
   `HELLO`. It may create streams only after receiving `WELCOME`.
2. Each MTProxy TCP connection opened by the app becomes a new, never-reused
   nonzero stream id and one `OPEN` frame.
3. Bytes already transformed for MTProxy become one or more `DATA` frames for that
   id. The client sends only within the relay-granted window.
4. Relay `DATA` is written to the corresponding local app connection. As the local
   Telegram networking engine drains those bytes, the client returns that amount as
   `WINDOW` credit.
5. EOF or failure on either side produces `CLOSE` for that stream. Other streams
   and the shared carrier continue.
6. Replacing or disabling the WEB proxy closes the carrier session and all of its
   logical streams.

The bridge batches complete client frames into `/up` request bodies and delivers
complete relay frames from `/down`; it never creates MTProxy payloads or assigns
stream ids.

The implementation does not emit PING or BYE in v1. A carrier failure closes the
authenticated HTTP session and the bridge tells the client to replace it.

The maximum payload is 1 MiB. Relay DATA chunks are at most 64 KiB. Each stream
begins with 4 MiB of credit in each direction. Client DATA consumes relay receive
credit; the relay grants WINDOW only after the bytes reach the local MTProxy TCP
socket. Backend reads consume client-granted credit and stop when it reaches zero.
One carrier body may contain at most 4096 complete frames. The default carrier body
target is 2 MiB; deployments may tune it up to the configured HTTP body limit.

Pending limits charge encoded bytes plus a conservative 256-byte cost for every
queued write or frame. Separate item limits remain authoritative even when adjacent
writes or WINDOW updates cannot be coalesced. Moving frames into the one replayable
downlink batch retains their byte and item charges until the next cursor acknowledges
that batch. Downlink DATA admission leaves room for one maximum uplink batch and
reserved byte and item headroom for WINDOW, CLOSE, and session control frames.
WINDOW grants for one stream coalesce while pending even when other streams' controls
are interleaved. Backend reads pause when the downlink DATA partition is full and
resume after a downlink acknowledgement releases capacity.

An `OPEN` creates exactly one connection to the profile's configured numeric
loopback backend. The client cannot select a destination. Stream IDs cannot be
reused during a session. Up to 4096 recently closed IDs remain as tombstones so
well-formed late DATA, WINDOW, or CLOSE frames from a close race can be ignored.
If an otherwise valid `OPEN` exceeds a per-session, profile, process-wide,
dial-in-flight, or stream-creation-rate limit, the relay returns `CLOSE` for that
stream id. It does not close the authenticated session or its other streams.

## Implementation references

- Telegram Desktop frame codec: `../tproxy/Telegram/SourceFiles/mtproto/web_proxy/web_proxy_frame.h`
- Telegram Desktop carriers: `../tproxy/Telegram/SourceFiles/mtproto/web_proxy/web_proxy_webview.cpp`
  and `../tproxy/Telegram/SourceFiles/mtproto/web_proxy/web_proxy_transport.cpp`
- Android client notes: `ANDROID.md`
- iOS client notes: `IOS.md`
- Server codec: `internal/frame/frame.go`
- Server carrier: `internal/server/server.go`
