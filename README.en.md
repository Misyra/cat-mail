# CatMail

**One sending API in front of all your email providers.** CatMail is a multi-provider failover gateway for transactional email — verification codes, order notifications, system alerts. It exposes a single unified HTTP API (plus an optional SMTP ingress) and routes every message across Resend, Brevo, Mailjet, SMTP2GO, Mailgun, Elastic Email, Mailtrap, Alibaba Cloud DirectMail, or any generic SMTP server, switching channels automatically when one fails or runs out of quota.

🌐 简体中文文档见 [README.md](README.md)

![License: AGPL v3](https://img.shields.io/badge/License-AGPL%20v3-blue.svg) ![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white) ![React 19](https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=black)

## Screenshots

| Dashboard | Channels | Send logs |
|---|---|---|
| ![Dashboard](docs/screenshots/dashboard.png) | ![Channels](docs/screenshots/channels.png) | ![Send logs](docs/screenshots/logs.png) |

Admin console (zh-CN UI, English UI available in-app).

## Why CatMail

- **No provider lock-in.** Every provider ships its own API shape, SDKs, and error quirks. Adding or switching one means rewriting sending code. With CatMail you write against one API once — providers become swappable configuration.
- **Quota exhaustion shouldn't mean downtime.** Free tiers are generous but small. The day your signup flow outgrows 100 emails per day, verification emails start failing. CatMail tracks per-channel daily and monthly usage and rotates to the next channel before a quota wall becomes an outage.
- **Failover you don't have to write.** Hand-rolled retry loops, error classification, and key-health checks belong in the gateway, not in your app. CatMail ships failover, cooldown, and auto-disable out of the box — and never re-sends a message a provider has already accepted.
- **Observability in one place.** Delivery logs are otherwise scattered across N provider dashboards. CatMail centralizes send logs, delivery events, suppression, per-channel health, and Prometheus metrics in a single console.

## Features

- **Unified sending API** — `POST /v1/send`, one Resend-style request body for every provider, Bearer API-key auth.
- **Resend SDK compatible** — `POST /emails` is wire-identical to `/v1/send`; point the Resend SDK's `baseUrl` at CatMail and existing code works unchanged.
- **10 platform adapters** — Resend, Brevo, Mailjet, SMTP2GO, Mailgun, Elastic Email, Mailtrap, Alibaba Cloud DirectMail, generic SMTP, and a mock provider with injectable failures for local failover drills.
- **Quota- and priority-aware rotation** — channels ordered by priority (or round-robin); quota-exhausted channels are skipped automatically, with per-channel daily/monthly usage tracked and quota reserved atomically per send.
- **Automatic failover** — each send tries up to 3 candidate channels by default; errors are classified per provider, and an accepted message is never re-sent elsewhere.
- **Health auto-disable and cooldown recovery** — 401/403 auto-disables a channel; 429 (and Brevo's out-of-credits response) triggers a cooldown with automatic recovery; consecutive failures trip a disable threshold. Per-channel 7-day health scores included.
- **Idempotency keys** — optional `Idempotency-Key` header replays the first response byte-for-byte on retry; in-flight duplicates get `409`.
- **Optional SMTP ingress** — legacy apps that only speak SMTP can use CatMail as a plain SMTP server, with open-relay protection and identical semantics to the HTTP API (STARTTLS / implicit TLS supported).
- **Delivery events and suppression list** — per-channel inbound webhook ingests delivered/bounced/complaint events (signature-verified for Resend and Mailgun); hard bounces and complaints suppress recipients across all entry points.
- **Built-in admin console and observability** — bilingual (zh-CN / English) embedded React UI with dashboards, channel management, send logs with body preview and CSV export, templates, notification center plus signed webhooks, Prometheus `/metrics`, and embedded OpenAPI docs at `/docs`.
- **Ops-ready single binary** — one Go binary with the frontend embedded; SQLite (pure Go, no CGO) or PostgreSQL storage, SQLite backups, TOTP 2FA, audit logging, AES-256-GCM credential encryption at rest, and self-hosted auto-update with mandatory SHA-256 verification.

## Quickstart

### Docker Compose (recommended)

From the repository root — set `CATMAIL_ADMIN_USER` / `CATMAIL_ADMIN_PASS` in `docker-compose.yml` first:

```bash
docker compose up -d --build

docker compose ps      # status and health checks
docker compose logs -f
```

Data persists in the host `./data` directory (mounted to `/data` in the container). On Linux, make it writable by the container user before the first start: `sudo chown -R 1000:1000 ./data`.

### Build from source

Requires Go 1.27+ and Node.js 20.19+. The frontend is embedded via `go:embed`, so build it first:

```bash
# 1. Build the frontend (outputs to backend/internal/web/dist)
cd frontend
npm install
npm run build

# 2. Compile the single binary
cd ../backend
go build -o catmail ./cmd/catmail

# 3. Run
./catmail
```

Open `http://localhost:8080` for the admin console. The default account is **`admin` / `admin123`** (created on first start only) — **change it immediately** under System Settings. Sanity check: `curl http://localhost:8080/api/health` returns `{"ok":true}`. First start also seeds a deletable mock channel so you can exercise the full send-and-rotate flow without any provider account.

## Send your first email

Create an API key in the console (keys start with `cm-` and are shown once), then:

```bash
curl -X POST http://localhost:8080/v1/send \
  -H "Authorization: Bearer cm-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "from": "CatMail <noreply@mail.example.com>",
    "to": ["user@example.com"],
    "subject": "Welcome aboard",
    "text": "Your verification code is 123456."
  }'
```

A successful send returns `200` with the internal `id`, the `channel` that handled it, the `provider`, and the provider's `provider_message_id`. Using the Resend SDK instead? Set its `baseUrl` to your CatMail address and calls land on `POST /emails` — same auth, same bodies, same errors.

## How failover works

Candidate channels are all enabled channels that match the sender domain (when `allowed_domains` is configured) and still have quota left, ordered by your strategy — priority-first by default, round-robin as an alternative. Each send attempts up to `max_attempts` (default 3) channels: an auth failure (401/403) auto-disables the channel, a 429 — including Brevo's out-of-credits response — puts it into a cooldown that recovers automatically, and network errors or 5xx count toward a consecutive-failure threshold that disables the channel. Quota is reserved atomically before each attempt and returned on failure, so concurrent sends never oversell a channel. The iron rule: once a provider accepts a message, CatMail never re-sends it on another channel.

## Documentation

Detailed documentation is currently written in Chinese:

| Document | Contents |
|---|---|
| [docs/API.md](docs/API.md) | Full sending API guide: request fields, error codes and retry semantics, templates, idempotency, webhooks, signature verification |
| [docs/platforms.md](docs/platforms.md) | Per-platform onboarding: credentials, domain verification, free tiers, failure behavior |
| [SPEC.md](SPEC.md) | Complete system specification: management API contract, admin console, security model |
| [README.md](README.md) | Full documentation: environment variables, DNS (SPF/DKIM) guidance, SMTP ingress, operations |

A running instance also serves the embedded OpenAPI 3.1 docs at `/docs`.

## Compliance and fair use

- **Register exactly one account per platform.** Do not create multiple accounts on the same provider to stack free quotas — doing so violates most providers' terms of service (ToS).
- Automatic rotation is designed for **fault tolerance and quota-aware scheduling**: when one account's quota is exhausted or a provider has an outage, traffic shifts to your own accounts on other providers. It is not a tool for circumventing any provider's restrictions.
- **Free-tier policies may change at any time.** Quotas shown in this document and in the admin console are scheduling fallback defaults only — always defer to each provider's official terms, and treat the provider's own error responses as authoritative once a quota is exceeded.
- Use this project for **lawful transactional email only** (verification codes, order notifications, system alerts, and the like). Do not use it for spam or bulk marketing; providers enforce separate compliance requirements for those.
- Safeguard your admin password and API keys, and change the default password immediately after deployment.

## License

[AGPL-3.0](LICENSE) © 2026 Misyra
