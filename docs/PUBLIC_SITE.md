# Public website backends

Every relay hostname needs an operator-owned website. This repository deliberately
ships no shared starter: repeated content and assets are an easy probing signature.

Use `public_upstream` for an ordinary website's routing, caching, errors, forms,
APIs, and WebSockets. A stock local Caddy file server can also be the upstream.
`public_dir` remains a convenient in-process static server, with Go HTTP serving
semantics. Neither mode promises to hide the HTTP stack or defeat every classifier.

## Private web application (recommended)

Configure exactly one public source:

```json
{
  "public_upstream": "http://127.0.0.1:3000"
}
```

The URL must contain only `http`, a numeric loopback address, and an explicit
port; it has no trailing slash or path. The application must listen on loopback.
Manage its process, framework, persistence, health checks, and updates separately.

Without an authentic relay credential in request metadata, every path uses the
same public handler, including `/api/v1/session`, `/api/v1/up`, `/api/v1/down`,
and `/api/v1/ws`. Those paths may be genuine application routes. The relay does
not remove random bearer values, cookies, carrier headers, uploads, or WebSocket
upgrades. It preserves the original Host, raw query, and streaming upload trailers.
Normal HTTP proxy rules still apply to hop-by-hop headers and framing.

The application owns its response headers, cookies, CSP, caching, redirects, and
body limits. The relay's carrier body deadline applies only after an authentic
secret is recognized. Caddy and the relay listener retain their general connection
and header limits. Caddy sends all paths through the relay; do not route a separate
set of public paths around it.

Authentic capabilities and signed tokens in request metadata are intercepted even
when expired, malformed, or misplaced. Only the canonical bridge GET or a valid
carrier operation succeeds. Other authentic requests fail locally with an
uncacheable 404; they are never sent to the website. See [HARDENING.md](HARDENING.md)
for the scope of recognition and the first-upgrade limitation for old tokens.

The public application is **trusted code sharing the bridge's origin**. The bridge
never incorporates its HTML or assets, but that alone is not a browser isolation
boundary. A service worker controlling `/` can intercept bridge navigation, and
same-origin scripts or injected third-party scripts carry that origin's authority.
Do not install service workers that cover the bridge or carrier paths, host
untrusted executable content, or treat a third-party analytics script as isolated
from this origin. Prefer a separate origin for features requiring that trust.
The bridge retains its strict nonce CSP, no-store policy, and cookie-free fetches.

A fresh installation can select the application directly:

```bash
sudo ./deploy/install.sh \
  --hostname proxy.example.com \
  --email operator@example.com \
  --site-upstream http://127.0.0.1:3000
```

## Static directory

Use `public_dir` instead of `public_upstream`:

```json
{
  "public_dir": "/srv/tproxy-site",
  "static_routes": "exact"
}
```

The directory requires `index.html`. Include a real `404.html`, favicon, styles,
and whatever other pages the site needs. A missing 404 still falls back to the
index body with status 404 for compatibility. Regular files are loaded recursively
at startup; symlinks and other non-regular entries are skipped. Restart the relay
after changing files. Keep secrets and deployment configuration outside this tree.

Successful GET/HEAD responses use `http.ServeContent`, per-file modification times,
and content-derived ETags. Conditional requests and byte ranges use standard Go
HTTP semantics. No common five-minute cache policy, CSP, Permissions-Policy, or
framing policy is imposed. Set site-specific policies through a public application
or stock static server if required. Error documents keep their error status and
are not converted to successful partial or conditional responses.

`static_routes: "exact"` serves `/` and exact filenames such as `/about.html`.
Link to the actual favicon; `/favicon.ico` does not silently return an SVG.
`static_routes: "legacy"` additionally resolves extensionless paths to `.html`
and aliases `/favicon.ico` to `/favicon.svg`. Old configs without this option
retain legacy routing so existing sites' links keep working. New installer output
and example configs explicitly select `exact`; use `--static-routes legacy` when
installing a site that depends on the old aliases.

```bash
sudo ./deploy/install.sh \
  --hostname proxy.example.com \
  --email operator@example.com \
  --site-dir /path/to/my-site
```

The installer copies the site to `/srv/tproxy-site`, preserving an existing site's
files on a reinstall. Use operator-owned copy and assets rather than a shared
proxy landing page. To update a site, replace the files, validate configuration,
and restart:

```bash
sudo /usr/local/bin/tproxy-server \
  -config /etc/tproxy-server/config.json \
  -profiles-file /etc/tproxy-server/profiles.json -check
sudo systemctl restart tproxy-server
```
