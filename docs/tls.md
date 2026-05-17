# TLS Setup

aimebu supports direct HTTPS with caller-supplied certificate files. This is
the narrow TLS option used by native clients and cross-host deployments that
cannot rely on plaintext loopback.

## Direct HTTPS

Set both server-side env vars before starting the server:

```bash
export AIMEBU_TLS_CERT=/path/to/fullchain.pem
export AIMEBU_TLS_KEY=/path/to/privkey.pem
export AIMEBU_TLS_PORT=9996   # optional; this is the default
aimebu server serve
```

When both variables are set, the server validates that both paths are
readable files and then serves HTTPS on `AIMEBU_TLS_PORT` while keeping plain
HTTP on `AIMEBU_PORT`. If neither TLS variable is set, aimebu keeps the
existing plain HTTP-only behavior. Setting only one variable, pointing either
variable at a missing/unreadable path, or setting an invalid `AIMEBU_TLS_PORT`,
fails startup.

`aimebu server start` passes the TLS variables through to the daemon child and
reports both listener URLs when TLS is enabled.

## Development Certificates

For local or LAN development, [mkcert](https://github.com/FiloSottile/mkcert)
is the simplest way to create a trusted certificate:

```bash
mkcert -install
mkcert -cert-file aimebu.pem -key-file aimebu-key.pem aimebu.local 127.0.0.1 ::1

export AIMEBU_TLS_CERT="$PWD/aimebu.pem"
export AIMEBU_TLS_KEY="$PWD/aimebu-key.pem"
export AIMEBU_TLS_PORT=9996
export AIMEBU_BIND=0.0.0.0
export AIMEBU_ALLOW=127.0.0.0/8,::1/128,192.168.1.0/24
aimebu server serve
```

Clients should then use an HTTPS URL:

```bash
export AIMEBU_URL=https://aimebu.local:9996
```

Existing local clients can keep using `http://localhost:9997` because the HTTP
listener remains active on `AIMEBU_PORT`.

For quick tests against an untrusted self-signed certificate, clients can set:

```bash
export AIMEBU_INSECURE_SKIP_VERIFY=1
```

This disables TLS certificate verification for aimebu client requests and
prints a warning. Use it only for development; it makes active network
attackers indistinguishable from the intended server.

## Reverse Proxy

For production-like setups, a reverse proxy can own certificate acquisition
and renewal while aimebu stays on loopback HTTP.

Caddy:

```caddyfile
aimebu.example.com {
	reverse_proxy 127.0.0.1:9997
}
```

Caddy's `reverse_proxy` directive handles WebSocket upgrades automatically.

nginx:

```nginx
server {
    listen 443 ssl;
    server_name aimebu.example.com;

    ssl_certificate /etc/letsencrypt/live/aimebu.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/aimebu.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:9997;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
    }
}
```

Keep `AIMEBU_BIND=127.0.0.1` when the proxy runs on the same host. If the
proxy connects from another host or container, widen both `AIMEBU_BIND` and
`AIMEBU_ALLOW` explicitly.

## Future ACME Hook

aimebu does not currently request or renew Let's Encrypt certificates itself.
If native certificate management is added later, it should be a separate
provisioning mode from the current caller-supplied cert/key path so operators
can keep using reverse proxies or externally managed certificates unchanged.
