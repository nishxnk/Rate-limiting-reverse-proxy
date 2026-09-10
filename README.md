# Rate-Limiting Reverse Proxy

A small proxy you put **in front of your web app or API**. It counts requests
per client, blocks anyone who sends too many (HTTP `429`), and forwards the rest
to your app. It comes with a live dashboard.

Think of it as a self-hosted "traffic guard" — like a tiny Cloudflare you run
yourself.

## What it does

- **Rate limiting** — e.g. "100 requests per second per client". Extra requests
  get `429 Too Many Requests` instead of hitting your app.
- **Per client** — limits are tracked per IP, or per API key (`X-API-Key`
  header).
- **Keeps working if Redis dies** — it uses Redis to share limits across many
  instances, but if Redis is down it automatically falls back to in-memory
  limiting and keeps serving.
- **Live dashboard** — watch traffic, allowed vs blocked, change the limit, and
  run a stress test — all in the browser.

## How it works

```
frontend ──► [ rate limiter ] ──► server      (allowed)
                   │
                   └──────────────► 429        (too many requests)
```

Every response also carries `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and
`X-RateLimit-Reset` headers so clients know where they stand.

---

## Run it locally

You only need **Go** installed. Redis is optional — the repo ships a tiny
built-in one, so nothing else to install.

Open **two terminals**.

**Terminal 1 — the built-in Redis** (leave it running):

```bash
go run ./cmd/devredis
```

**Terminal 2 — the proxy:**

```bash
# macOS / Linux
REDIS_ADDR=127.0.0.1:6379 UPSTREAM_URL=mock://internal RATE_LIMIT_RPS=10 \
  go run ./cmd/proxy
```

```powershell
# Windows PowerShell
$env:REDIS_ADDR="127.0.0.1:6379"; $env:UPSTREAM_URL="mock://internal"; $env:RATE_LIMIT_RPS="10"
go run ./cmd/proxy
```

Now:

- your traffic → <http://localhost:8080/>
- dashboard → <http://localhost:8080/_rl/dashboard>

`mock://internal` is a fake app built into the tool, so you can try it with no
real app. To point it at your own app instead, change one value:
`UPSTREAM_URL=http://localhost:3000`. Don't want Redis at all? Use
`REDIS_ADDR=disabled`.

> **Seeing the block (429):** a plain `curl` loop is too slow to trip the limit.
> Use the dashboard's **Run stress test** button (it fires many requests at
> once), or send a burst with any load tool.

---

## Use it in front of your own app

1. Set `UPSTREAM_URL` to your app's address (e.g. `http://localhost:3000`).
2. Run the proxy (as above, or with Docker: `docker compose up`).
3. Send your app's traffic to the proxy instead of directly to the app — on a
   real server this means pointing your domain's DNS at the proxy.

That's it — from then on every request is rate-limited automatically. Full
production setup (Docker, a public server, DNS, HTTPS) is in
**[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)**.

---

## Dashboard

Open `…/_rl/dashboard`. You can:

- see **live traffic** — allowed vs blocked, per second
- **change the limit** (requests + window) instantly, no restart
- see the **list of clients** and how much quota each has left
- **run a stress test** — fire 50 requests and watch the 200 vs 429 split

---

## Configuration

Set these as environment variables (all have sensible defaults):

| Variable | Default | Meaning |
| --- | --- | --- |
| `PORT` | `8080` | Port the proxy listens on |
| `UPSTREAM_URL` | `mock://internal` | Your app's address (or the built-in mock) |
| `RATE_LIMIT_RPS` | `10` | Requests allowed per window |
| `WINDOW_SIZE_SECONDS` | `1` | Length of the window in seconds |
| `REDIS_ADDR` | `localhost:6379` | Redis address, or `disabled` |
| `ADMIN_ADDR` | *(empty)* | Put the dashboard on its own port (recommended in production) |
| `ADMIN_TOKEN` | *(empty)* | Require this token for the dashboard + control API |

There are a few more (timeouts, circuit breaker, admin path) — see
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) and `.env.example`.

> **Note:** the dashboard is open by default (fine locally). Before exposing it,
> set `ADMIN_TOKEN` (browser prompts for it; scripts send `Authorization: Bearer
> <token>`) and/or put it on a private port with `ADMIN_ADDR=127.0.0.1:9090`.

---

## Run the tests

```bash
go test ./...
```

## Project layout

```
cmd/proxy/       the proxy (main program)
cmd/devredis/    a built-in Redis for local use
internal/limiter/  rate limiting (Redis + in-memory fallback)
internal/proxy/    request forwarding
internal/api/      dashboard API + live stream
internal/dashboard/  the dashboard web page
config/          settings
docs/DEPLOYMENT.md   production deployment guide
```
