# HTTPS

V2 can serve HTTPS with a certificate and key you supply. HTTP stays on
`AIMEBU2_PORT` (default `9897`); HTTPS uses `AIMEBU2_TLS_PORT` (default `9896`).
Both listeners use the same `AIMEBU2_BIND` address and `AIMEBU2_ALLOW`
allowlist. The embedded supervisor continues to use HTTP on loopback.

```bash
export AIMEBU2_TLS_CERT=/path/to/cert.pem
export AIMEBU2_TLS_KEY=/path/to/key.pem
export AIMEBU2_TLS_PORT=9896
aimebu server
```

Set both certificate variables or neither. Startup fails if only one is set,
either file is missing or unreadable, the pair cannot be loaded, the TLS port
is invalid, or it equals the HTTP port. With neither set, the server uses HTTP
only. Sessions set the `Secure` cookie attribute on HTTPS requests, and on
plain-HTTP requests addressed to the host of an `https` `AIMEBU2_PUBLIC_URL`.
Open the
web UI in a secure context (HTTPS, or `localhost`) to enable its browser
notification permission button.

## Local certificate with mkcert

```bash
mkcert -install
mkcert -cert-file aimebu.pem -key-file aimebu-key.pem localhost 127.0.0.1 ::1
export AIMEBU2_TLS_CERT="$PWD/aimebu.pem"
export AIMEBU2_TLS_KEY="$PWD/aimebu-key.pem"
aimebu server
```

Open `https://localhost:9896/`. Add the host name or IP used by remote
clients to the certificate, and widen both `AIMEBU2_BIND` and `AIMEBU2_ALLOW`
when remote peers need access.

For a standalone supervisor using a self-signed certificate that its host
does not trust, set `AIMEBU2_INSECURE_SKIP_VERIFY=1` before pairing or running
it. The same setting applies to its WebSocket, approval checks, and attachment
downloads. It disables certificate verification;
use a trusted certificate for ordinary remote deployments.

```bash
export AIMEBU2_INSECURE_SKIP_VERIFY=1
export AIMEBU2_URL=https://server.example:9896
aimebu supervisor
```

## Reverse proxy

If a proxy provides HTTPS, keep aimebu on HTTP loopback and forward to it.
For example, Caddy handles both HTTP requests and WebSocket upgrades:

```caddyfile
aimebu.example.com {
	reverse_proxy 127.0.0.1:9897
}
```

Set the public HTTPS origin so invitation and password-reset links and the
machine pairing command use the proxy address instead of the loopback
listener. Include a port when it is not the default HTTPS port:

```bash
export AIMEBU2_PUBLIC_URL=https://aimebu.example.com
```

`AIMEBU2_PUBLIC_URL` must be an HTTP or HTTPS origin without credentials,
path, query, or fragment. When unset, direct TLS deployments generate HTTPS
links using `AIMEBU2_TLS_PORT`; HTTP-only loopback deployments generate HTTP
links. Proxy headers are not used to choose the link scheme.

The public origin also governs browser sessions behind the proxy. The event
WebSocket accepts an `Origin` equal to `AIMEBU2_PUBLIC_URL` (default ports
may be omitted) as well as the listener's own origin; any other origin is
still rejected. With an `https` public URL, session cookies get the `Secure`
attribute when the forwarded `Host` is the public host, even though the proxy
forwards plain HTTP. Caddy forwards the original `Host` by default; with nginx
add `proxy_set_header Host $host;`, or cookies arrive without `Secure`.
Direct plain-HTTP requests to another host, such as a LAN IP, never get
`Secure`, because browsers would drop the cookie and sign-in would fail.

The browser reaches the proxy over HTTPS, so browser notifications work there
too. Restrict the proxy's inbound access separately; aimebu's allowlist sees
the proxy's direct source IP and does not trust `X-Forwarded-For`.

Behind a proxy every request arrives from the proxy's address, so login,
invitation and pairing throttles would otherwise treat all clients as one IP:
one client's failed guesses could hold everyone's sign-in in backoff. Name
the proxy in `AIMEBU2_TRUSTED_PROXIES` (comma-separated IPs or CIDRs, default
empty) to tell clients apart. While `AIMEBU2_PUBLIC_URL` is set and this
list is empty, `aimebu server` logs a startup warning and `aimebu doctor`
warns:

```bash
export AIMEBU2_TRUSTED_PROXIES=127.0.0.1
```

Only when the direct peer is in that list does aimebu read
`X-Forwarded-For`, and then it uses the right-most entry that is not itself a
trusted proxy; entries further left are client-supplied and ignored. An
unparseable hop falls back to the peer address. This affects throttling only:
`AIMEBU2_ALLOW` still checks the direct peer and never reads forwarded
headers. (Separately, the first-run setup page shows the full path of the setup
token file only to a loopback connection that carries no forwarding header: a
proxy on this machine connects from loopback too, so a request with
`X-Forwarded-For`, `Forwarded`, `X-Real-Ip`, `X-Forwarded-Proto/Host` or `Via` is
treated as remote. A proxy that relays none of these cannot be told from a local
browser, so keep the setup page off the internet until the owner exists.) List only proxies that overwrite or append to `X-Forwarded-For`
(Caddy and nginx with `$proxy_add_x_forwarded_for` do). Trusting a
loopback address such as `AIMEBU2_TRUSTED_PROXIES=127.0.0.1` lets any local
process that connects over loopback pick its own throttle IP through
`X-Forwarded-For`, so it can escape the per-IP backoff; the per-login and
per-token throttles still apply to it.
