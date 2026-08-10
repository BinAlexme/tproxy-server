# tproxy-server

`tproxy-server` lets the WEB proxy type in the accompanying Telegram Desktop build
use a real browser's ordinary HTTPS connection. The public hostname remains a real
website. A value derived from the hostname and MTProxy secret selects a one-shot
bridge page.

The production layout is:

```text
Internet :80/:443 -> Caddy -> static website
                         \-> 127.0.0.1:8080 tproxy-server
                                             \-> 127.0.0.1:2398 official MTProxy
```

Only Caddy listens on external interfaces. The relay, its admin endpoints, the
official MTProxy client port, and MTProxy statistics remain local. The relay never
receives a client-selected backend address and never decrypts the MTProxy stream.

## What you need

- a dedicated lowercase hostname such as `proxy.example.com` that you control;
- an **x86_64** Linux server with a public IPv4 address, SSH access, systemd, and
  either Ubuntu 22.04+ or Debian 12+;
- root or passwordless `sudo` on that server;
- public inbound TCP 80 and 443; and
- one random 16-byte secret.

The automated installer is intended for a clean server on which Caddy may own ports
80/443. It backs up an existing `/etc/caddy/Caddyfile`, but it then replaces the
active Caddy configuration. If the server already hosts other sites, use the manual
integration section instead.

## 1. Choose the hostname and secret

At your DNS provider, add an `A` record:

```text
proxy.example.com -> YOUR_SERVER_PUBLIC_IPV4
```

Add an `AAAA` record only when the server really has working public IPv6. Do not put
a CDN or HTTP proxy in front of this first deployment. Wait until the record resolves
from outside your network:

```bash
dig +short A proxy.example.com
dig +short AAAA proxy.example.com
```

Generate the client-facing secret on your own computer:

```bash
openssl rand -hex 16
```

That produces 32 lowercase hexadecimal characters. To enable MTProxy random-padding
mode, prefix those characters with `dd`. Keep the exact resulting 32- or
34-character value: it is entered in Telegram Desktop and passed to the installer.
The installer removes `dd` only for the stock MTProxy backend.

## 2. Prepare the real public site

The repository includes a small, self-contained site named “Quiet Systems” under
`web/public`. It has an index, about page, privacy page, custom 404, stylesheet,
favicon, and robots file, with no third-party resources. It is usable as a starter,
but a hostname you publish should contain content that is genuinely yours.

Edit those files before uploading, or edit `/srv/tproxy-site` after installation.
Do not add third-party analytics to the bridge page; the bridge page is generated
inside the relay and never loads public-site assets.

## 3. Configure the hosting firewall

In the hosting provider's network rules or firewall, allow:

| Port | Source | Purpose |
|---:|---|---|
| TCP 22 | your administrator IP if possible | SSH |
| TCP 80 | anywhere | ACME validation and HTTPS redirect |
| TCP 443 | anywhere | website and browser transport |

Do not allow TCP 2398, 8080, 8081, or 8888. The installer adds a local nftables rule
that drops external traffic to 2398 and 8888, but the provider firewall is the
second required boundary.

If the host itself runs UFW or another firewall, allow 80/443 there as well. Preserve
your working SSH rule before changing anything remotely:

```bash
sudo ufw status
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
```

## 4. Upload and install over SSH

From the parent directory on your computer, upload this working tree. `rsync` is
convenient even before the repository has a remote:

```bash
rsync -az --delete --exclude .git \
  tproxy-server/ YOUR_SSH_USER@YOUR_SERVER_PUBLIC_IP:/tmp/tproxy-server/
```

Then connect and run the installer, substituting the hostname and contact email:

```bash
ssh YOUR_SSH_USER@YOUR_SERVER_PUBLIC_IP
cd /tmp/tproxy-server
sudo ./deploy/install.sh \
  --hostname proxy.example.com \
  --email you@example.com
```

The installer prompts without echo for the secret. Paste the exact 32- or
34-character value there. This keeps it out of the shell history and process list.
For unattended provisioning, `--secret` is available, but it places the value in
the invoking process list and should be used only in automation where process
arguments are controlled.

The installer:

1. installs nftables and build prerequisites, a checksum-verified official Caddy
   binary with a dedicated systemd service, and a verified Go toolchain when the
   host does not already have Go 1.20 or newer;
2. verifies the archive for pinned MTProxy commit
   `f36d8af769ffaeac36978d38c2c0f6d1104c2137`, builds it unchanged as the
   unprivileged `mtproxy` user, and installs the result as root;
3. downloads the official MTProxy secret and routing configuration over HTTPS;
4. runs all Go tests and installs `/usr/local/bin/tproxy-server`;
5. installs the public site without overwriting an existing `/srv/tproxy-site`;
6. creates mode-restricted configuration and a systemd credential for the WEB
   secret;
7. installs the backend firewall, relay, MTProxy, refresh timer, and Caddy units; and
8. asks Caddy to obtain and renew the hostname's public certificate.

The script accepts lowercase hexadecimal secrets. The server configuration itself
also accepts canonical base64url secrets if you later manage profiles manually.

## 5. Verify the deployment

On the server:

```bash
systemctl --no-pager --full status \
  caddy tproxy-firewall mtproxy tproxy-server
curl --fail http://127.0.0.1:8081/healthz
curl --fail http://127.0.0.1:8081/readyz
curl --silent http://127.0.0.1:8081/metrics
ss -lntp
sudo nft list table inet tproxy_backend
```

Expected listeners are public Caddy on 80/443 and loopback relay listeners on
8080/8081. Official MTProxy listens on 2398 because its upstream command has no bind
address option; nftables must drop that port on every non-loopback interface.

From your own computer, verify the site and certificate:

```bash
curl --fail --show-error --location https://proxy.example.com/
curl --fail --show-error https://proxy.example.com/about
openssl s_client -connect proxy.example.com:443 \
  -servername proxy.example.com </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates
```

Also confirm the local-only ports are unreachable externally. These commands should
time out or fail:

```bash
nc -vz -w 3 YOUR_SERVER_PUBLIC_IP 2398
nc -vz -w 3 YOUR_SERVER_PUBLIC_IP 8888
nc -vz -w 3 YOUR_SERVER_PUBLIC_IP 8080
nc -vz -w 3 YOUR_SERVER_PUBLIC_IP 8081
```

Check that missing, wrong, duplicated, or augmented bridge queries all display the
same normal home page:

```bash
curl --fail 'https://proxy.example.com/?bridge=wrong'
curl --fail 'https://proxy.example.com/?bridge=wrong&x=1'
```

Do not paste the real derived bridge URL into logs or test commands. Telegram
Desktop derives it in memory.

## 6. Connect Telegram Desktop

In the accompanying tdesktop build:

1. open **Settings → Advanced → Connection type → Use custom proxy**;
2. add a **WEB** proxy;
3. enter only `proxy.example.com` as the hostname;
4. enter the exact client-facing secret, including `dd` when you selected it;
5. enable the proxy; and
6. choose **Open browser** and keep that tab open.

The address field must not contain `https://`, `:443`, a slash, or a query. Telegram
Desktop opens a loopback page; that page embeds the HTTPS bridge. The
external DNS and TLS connection belongs to the real browser.

For an internationalized domain, convert the name to its lowercase ASCII IDNA
A-label form before putting it in the server configuration; tdesktop stores the
same canonical form internally.

Once the server is live, execute the complete matrix in
`../tproxy/docs/web-proxy-test-plan.md` before depending on it where direct MTProto
works unreliably.

### Repeating this on several hosting accounts

Give every server its own hostname, for example `north.example.com` and
`south.example.com`, and repeat the DNS, firewall, upload, and install steps on each
host. One relay process intentionally serves one public hostname. Independent
secrets make rotation and deployment management clearer; reusing a base secret is
technically possible because the bridge capability also includes the hostname, but
it couples all of those deployments to one credential.

## Manual integration on an existing Caddy server

Build and install the relay without running `deploy/install.sh`:

```bash
go test ./...
go build -trimpath -o tproxy-server ./cmd/tproxy-server
sudo install -m 0755 tproxy-server /usr/local/bin/tproxy-server
```

Create `/srv/tproxy-site`, `/etc/tproxy-server/config.json`, and a mode-`0400`
profiles file from `config.example.json` and `profiles.example.json`. When not using
systemd `LoadCredential`, point `profiles_file` directly at the mode-restricted file.
Both relay listeners and every backend address must be numeric loopback addresses.

Build the official backend with `deploy/install-mtproxy.sh`, install the supplied
systemd units, and add these routes before your existing static-site route:

```caddyfile
@tproxy_relay path / /api/v1/*
handle @tproxy_relay {
  reverse_proxy 127.0.0.1:8080 {
    transport http {
      response_header_timeout 40s
    }
  }
}
```

The relay must receive the original `Host`. The supplied direct-to-origin Caddy
layout relies on Caddy's default sanitizing of forwarded client addresses; if you
change trusted-proxy handling, the relay must still receive exactly one IP address.
Do not apply your public site's
`X-Frame-Options`, COOP, COEP, or framing CSP to the proxied root response; the
bridge supplies a distinct CSP permitting only the numeric loopback parent.
Do not enable access logging of raw URIs, authorization headers, or bodies.

## Multiple secrets on one hostname

Add profiles to `/etc/tproxy-server/profiles.json`. Every profile has a unique name,
client secret, and numeric loopback backend:

```json
{
  "profiles": [
    {"name":"alpha","secret":"0123456789abcdef0123456789abcdef","backend":"127.0.0.1:2398"},
    {"name":"beta","secret":"ddfedcba9876543210fedcba9876543210","backend":"127.0.0.1:2399","limits":{"max_streams_per_session":32,"max_pending_per_session":8388608}}
  ]
}
```

Run one official MTProxy process and listener per profile when profiles need
separate quotas or routing. Extend `firewall.nft` to include every added backend
port. A single MTProxy may receive repeated `-S` arguments only when all profiles
intentionally share the same policy and routing scope.

Restarting `tproxy-server` invalidates active browser sessions:

```bash
sudo systemctl restart tproxy-server
```

Users then choose **Open browser** again. Existing TCP streams are intentionally not
resumed across a relay restart.

## Operations and updates

Useful diagnostics contain event classes and counts, not secrets or request URLs:

```bash
journalctl -u tproxy-server -u mtproxy -u caddy --since '30 minutes ago'
systemctl list-timers refresh-mtproxy-config.timer
curl --silent http://127.0.0.1:8888/stats
```

Go profiling routes are disabled by default. Set `"enable_pprof": true` only for a
bounded diagnostic window on a host where the loopback admin listener is trusted,
then disable it and restart the relay.

The supplied service units use `ProtectProc=invisible` and `ProcSubset=pid`, so the
public Caddy and relay users cannot inspect other services' process metadata. Stock
MTProxy accepts its secret through `-S`, which leaves the value in that process's
argument memory. Root and unrestricted host administrators can still inspect it;
do not give untrusted users login or unconfined service access to this host.

The refresh timer replaces `proxy-multi.conf` daily and restarts MTProxy so the new
routing data is active. Existing backend streams reconnect through the still-live
relay session. The public website remains available when either backend process is
down; `/readyz` reports the backend outage.

For a relay-code update, upload the new repository and run from its root:

```bash
sudo ./deploy/update-relay.sh
```

The updater finds the installed Go toolchain, runs all Go tests, builds and validates
a candidate against the installed configuration, keeps the previous binary, installs
the candidate atomically, and restarts only `tproxy-server`. It waits for health and,
when the old deployment was ready, backend readiness; a failure automatically rolls
back to the previous binary. Existing browser sessions are invalidated and users must
choose **Open browser** again.

This script intentionally does not replace configuration, systemd units, Caddy,
MTProxy, firewall rules, or public-site files. Running the complete automated
installer again preserves an existing site directory but replaces the single-profile
config and active Caddyfile, so use it deliberately.

## Troubleshooting

- **Caddy cannot obtain a certificate:** confirm the `A`/`AAAA` records point
  directly to this host and that both 80 and 443 reach Caddy. Remove a broken AAAA
  record rather than leaving IPv6 half-configured.
- **`/readyz` returns 503:** inspect `systemctl status mtproxy`, then confirm a local
  TCP connection to `127.0.0.1:2398` and the downloaded files under `/etc/mtproxy`.
- **The browser shows the public site instead of connecting:** hostname and secret
  must match the server profile exactly; `dd` changes the bridge capability.
- **Telegram Desktop remains “Waiting for browser”:** use **Open browser** from the
  same running desktop instance and ensure local firewall or network software
  permits its loopback WebSocket.
- **The public site works but the bridge fails:** inspect only sanitized service
  status and metrics. Never log the browser address bar or authorization headers.
- **Configuration check fails on permissions:** the profiles file must have no group
  or other permission bits. Use `chmod 0400` and ensure the service receives it via
  `LoadCredential`.

The complete architecture and implementation milestones remain in `PLAN.md`; the
normative wire format is in `PROTOCOL.md`.
