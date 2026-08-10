# WEB proxy protocol v1

This is the wire contract shared by Telegram Desktop, the browser bridge, and
`tproxy-server`. All binary integers are unsigned and big-endian.

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

## MessageChannel boundary

The local tdesktop page embeds the HTTPS bridge page and transfers one port:

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
batches. Control objects are status updates and `{t:'close'}`.

## Android WebView boundary

An Android client may load the same bridge document as the WebView main frame and
append a client-only fragment:

```text
https://H/?bridge=bridge#android=android-nonce
```

`android-nonce` is 32 random bytes in canonical unpadded base64url form. It is not
sent in the HTTPS request. Before navigation, the app injects a WebMessage listener
named `TelegramWebProxy` for exactly the `https://H` origin. Both
`WEB_MESSAGE_LISTENER` and `WEB_MESSAGE_ARRAY_BUFFER` must be supported by the
installed Android System WebView. A wildcard origin or `addJavascriptInterface` is
not part of this protocol.

The bridge removes the query and fragment with `history.replaceState`, adapts the
injected listener to its internal port contract, and sends this JSON string:

```json
{"t":"tproxy-android-init","v":1,"nonce":"android-nonce"}
```

The app accepts it only from the main frame, only from the exact configured HTTPS
origin, and only when the nonce matches the value it generated. Messages from the
bridge to the app are either JSON-encoded control strings or an ArrayBuffer
containing exactly one complete shared frame. The bridge splits aggregated HTTP
downlink batches at validated frame boundaries before crossing the WebView IPC
boundary. Messages from the app to the bridge use the same two representations;
the app should keep DATA frames at or below the relay's 64 KiB chunk size.

The Android proof of concept points tgnet's existing MTProxy connection at a
numeric loopback listener. Each accepted local TCP connection becomes one logical
WEB stream, so bytes crossing this WebView boundary have already received the
normal MTProxy transformation. The public relay remains unable to choose a client
destination or decrypt the stream.

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

The implementation does not emit PING or BYE in v1. A carrier failure closes the
authenticated HTTP session and the bridge tells tdesktop to replace it.

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

## Compatibility sources

- Desktop frame constants: `../tproxy/Telegram/SourceFiles/mtproto/web_proxy/web_proxy_frame.h`
- Desktop browser parent: `../tproxy/Telegram/SourceFiles/mtproto/web_proxy/web_proxy_transport.cpp`
- Server codec: `internal/frame/frame.go`
- Server carrier: `internal/server/server.go`
