# Deployment guide (self-hosted)

This guide covers running the rate limiter the way it is meant to be used in
production: **you deploy it in front of your own application**, on your own
infrastructure. Once it is in the request path, every request is protected — you
do nothing per request, and no third party (not even the author of this tool) is
involved.

```
            before                                after
   users ───────────────► app        users ──► [ rate limiter ] ──► app
                                                     │
                                              redis (shared limit,
                                              fleet-wide policy)
```

You put the proxy in the path once — with DNS, a load balancer, or an ingress —
and it stays there. This is the same model as nginx, Kong, or a self-hosted API
gateway.

> **Scope, honestly.** This is an *application-layer* rate limiter. It stops
> request floods and enforces per-client limits (the thing you can demo on the
> dashboard). It is **not** a full anti-DDoS service: a volumetric network-level
> flood (L3/L4, tens of Gbps) has to be absorbed upstream of any single host —
> that is what a CDN/scrubbing provider is for. Use this for fair-use limiting
> and application-flood protection, behind whatever network protection your host
> already provides.

---

## 1. The one rule that keeps it safe

The dashboard and control API have **no authentication** — anyone who can reach
them can change the rate limit or clear counters. So the golden rule is:

> **Never expose the admin plane to the internet.**

The binary makes this easy: set `ADMIN_ADDR` and the control plane binds to its
**own address**, while the public port serves *only* proxied traffic.

```
PORT=8080            → public: proxied traffic only (dashboard NOT here)
ADMIN_ADDR=127.0.0.1:9090 → dashboard + API, reachable from the host only
```

Verified behaviour with this split:

| Request | Public `:8080` | Admin `:9090` |
| --- | --- | --- |
| `/` , `/login` , `/api/...` (your app) | forwarded to your app | — |
| `/dashboard` , `/api/stats` | **404** (not exposed) | served |
| `/healthz` | 404 | served |

Reach the dashboard from your laptop over an SSH tunnel:

```bash
ssh -L 9090:127.0.0.1:9090 you@your-server
# then open http://localhost:9090/dashboard
```

---

## 2. Fastest path — Docker Compose

A ready-made production compose file ships as `docker-compose.deploy.yml`. It
runs Redis + the proxy, binds the admin plane to loopback, and locks the target.

```bash
# point it at your app and start it
export UPSTREAM_URL=http://your-app:3000     # or https://your-internal-host
docker compose -f docker-compose.deploy.yml up --build -d

# public traffic:      http://<server>:8080/
# dashboard (host only): http://127.0.0.1:9090/dashboard
```

If your app also runs in Docker, put it in the same compose file / network and
use its service name as `UPSTREAM_URL` (e.g. `http://app:3000`).

> Docker is not installed on the machine this project was built on, so the
> compose commands here are not machine-verified. The compose file is valid YAML
> and the container envs mirror the flags below, which **are** verified by
> running the binary directly.

---

## 3. Standalone binary (systemd)

No Docker required. Build once, run under systemd.

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /usr/local/bin/ratelimit-proxy ./cmd/proxy
```

`/etc/systemd/system/ratelimit-proxy.service`:

```ini
[Unit]
Description=Rate limiting reverse proxy
After=network.target redis.service
Wants=redis.service

[Service]
ExecStart=/usr/local/bin/ratelimit-proxy
Restart=always
RestartSec=2
User=ratelimit
# --- configuration ---
Environment=PORT=8080
Environment=ADMIN_ADDR=127.0.0.1:9090
Environment=REDIS_ADDR=127.0.0.1:6379
Environment=UPSTREAM_URL=http://127.0.0.1:3000
Environment=RATE_LIMIT_RPS=100
Environment=WINDOW_SIZE_SECONDS=1
Environment=UPSTREAM_EDITABLE=false
Environment=LOG_FORMAT=json
# hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd -r -s /usr/sbin/nologin ratelimit
sudo systemctl daemon-reload
sudo systemctl enable --now ratelimit-proxy
```

No Redis? Set `REDIS_ADDR=disabled` — the in-memory limiter carries the whole
load, but each instance then limits independently (see §6).

---

## 4. TLS

The proxy speaks plain HTTP. Terminate TLS in front of it — the standard shape,
and it lets you keep using your existing certificate tooling.

**Caddy** (automatic Let's Encrypt certificates), `Caddyfile`:

```
your-domain.com {
    reverse_proxy 127.0.0.1:8080
}
```

**nginx**:

```nginx
server {
    listen 443 ssl;
    server_name your-domain.com;
    ssl_certificate     /etc/letsencrypt/live/your-domain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/your-domain.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Host $host;
    }
}
```

When a TLS terminator you control sits in front and sets `X-Forwarded-For`, turn
on `TRUST_PROXY_HEADERS=true` so the limiter keys on the real client IP instead
of the terminator's address. **Only** with a trusted hop in front — otherwise
clients spoof the header and dodge their limit.

---

## 5. Pointing your domain at it (DNS), step by step

The proxy only protects traffic that flows through it, so you make it the front
door once, with a DNS record. DNS is **not** set inside this app — it lives at
whoever manages your domain (Cloudflare, GoDaddy, Namecheap, your registrar). No
tool can change it for you; you point it once, by hand.

### What you need first

- A **domain** you control (e.g. `oxmaint.es`).
- A **server with a public, static IP** running the proxy. A home laptop behind
  a router will not work — it has no reachable public IP. Use a small VPS
  (Hetzner, DigitalOcean, AWS Lightsail, …).
- Ports **80** and **443** open on that server's firewall / security group.

### The steps

1. **Get the server's public IP** — e.g. `203.0.113.50`:
   ```bash
   curl -s https://ifconfig.me      # run this on the server
   ```
2. **Open your DNS provider's dashboard** — the place your domain's nameservers
   point to. For most people that is Cloudflare or the registrar you bought the
   domain from.
3. **Add an `A` record**:

   | Field | Value |
   | --- | --- |
   | Type | `A` (use `AAAA` for an IPv6 address) |
   | Name | `@` for the root `oxmaint.es`, or a subdomain like `app` / `dms` |
   | Value / Points to | your server IP, `203.0.113.50` |
   | TTL | Auto (or 300s) |

4. **Save.** Propagation takes minutes (sometimes up to an hour). Check it:
   ```bash
   nslookup dms.oxmaint.es      # should return your server IP
   ```

### On Cloudflare specifically (your site is behind Cloudflare)

DNS → **Add record** → Type `A`, Name, IPv4 = server IP. The **orange cloud**
toggle decides the shape:

- **Orange (Proxied):** visitors → Cloudflare → your server (the proxy) → app.
  Cloudflare's own protection sits in front; your proxy adds per-client limits.
- **Grey (DNS only):** visitors → your server directly. Cloudflare is just
  resolving the name.

Either works. If Cloudflare already does rate limiting for you, this proxy is
still useful for limits Cloudflare's plan does not cover, or for a self-hosted
dashboard you fully control.

### ⚠️ The one trap: don't create a loop

Once the domain points at the proxy, the proxy's `UPSTREAM_URL` must point at
your app's **real origin**, never back at the public domain:

```
WRONG:  dms.oxmaint.es → proxy → https://dms.oxmaint.es   (infinite loop)
RIGHT:  dms.oxmaint.es → proxy → http://10.0.0.5:3000      (the app's origin)
```

Give the origin its own name or use its internal IP, and set that as
`UPSTREAM_URL`. Common patterns: a private IP (`http://10.0.0.5:3000`), a
same-host port (`http://127.0.0.1:3000`), or an unproxied `origin.` subdomain
you keep grey-clouded.

### Other ways to put it in the path

- **Load balancer / ingress**: add the proxy as the upstream for your app's route.
- **Kubernetes**: run the proxy as a sidecar or a Service in front of your app's
  Service; point the Ingress at the proxy.

After that one change it is permanent: every visitor is rate-limited, with
nothing to do per request.

### You do NOT need DNS to test locally

Locally everything is `localhost`, so there is no DNS step at all. To make it
*feel* like a real domain, add a line to your hosts file
(`C:\Windows\System32\drivers\etc\hosts` on Windows, `/etc/hosts` on
Linux/macOS), which is a local-only override — it changes nothing on the
internet:

```
127.0.0.1   myapp.local
```

Then open `http://myapp.local:8080/`. That is the local stand-in for a DNS
record pointing at the proxy.

---

## 6. More than one instance (a fleet)

Run several proxy instances behind your load balancer and give them the **same
Redis**. Then the limit is *global*: 100 req/s means 100 across the whole fleet,
not 100 per instance. A policy change from any dashboard, and a target change
(when editing is enabled), propagate to all of them through Redis.

```
        ┌── proxy A ──┐
users ──┼── proxy B ──┼── your app
        └── proxy C ──┘
              │
            redis   (one global limit, shared policy)
```

If Redis becomes unreachable, each instance fails over to its in-memory limiter
and keeps serving — approximate across the fleet, but never unmetered. The
dashboard's backend indicator and the `degraded` flag show when this happens.

---

## 7. Production checklist

- [ ] `ADMIN_ADDR` set to a loopback/internal address — dashboard **not** on the public port
- [ ] `UPSTREAM_EDITABLE=false` — target is fixed to `UPSTREAM_URL`
- [ ] TLS terminated in front (Caddy / nginx / cloud LB)
- [ ] `TRUST_PROXY_HEADERS=true` **only** if a trusted proxy sets `X-Forwarded-For`
- [ ] `RATE_LIMIT_RPS` / `WINDOW_SIZE_SECONDS` sized for your real traffic
- [ ] Redis reachable and, if you rely on persistence of counters, configured with a volume
- [ ] `LOG_FORMAT=json` for your log pipeline
- [ ] Health checks point at `{ADMIN_ADDR}/healthz`
- [ ] The host itself sits behind whatever network-level DDoS protection your provider offers

---

## 8. Verifying a deployment

```bash
# through the proxy: your app should answer, with rate-limit headers added
curl -i http://<server>:8080/

# the limit should bite under a burst (adjust the count to your RPS)
for i in $(seq 1 200); do
  curl -s -o /dev/null -w "%{http_code} " http://<server>:8080/
done; echo
# ... 200 ... then 429 once the window fills

# admin plane only from the host
curl -s http://127.0.0.1:9090/healthz
```

`scripts/test_load.sh` automates the burst-and-threshold check. It is written
for the shared-listener layout (admin under `ADMIN_PREFIX` on the same host);
against a split deployment, run the burst loop above by hand and read the policy
from the admin address directly (`curl http://127.0.0.1:9090/api/policy`).
