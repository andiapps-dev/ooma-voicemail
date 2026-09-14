# Ooma Voicemail Checker

A small daemon that logs into your **Ooma** account, downloads new
voicemails as MP3s, and notifies you (email, Discord, ntfy, Telegram,
Pushover, ...) via [Apprise](https://github.com/caronc/apprise) when one
arrives — all from a single static binary, no extra services to run.

> Not affiliated with, endorsed by, or sponsored by Ooma, Inc. "Ooma" is a
> trademark of its respective owner, referenced here only to describe
> compatibility. This tool automates your own authenticated access to your
> own account using your own credentials — nothing here bypasses
> authentication or accesses anyone else's data — but it may still fall
> outside Ooma's Terms of Service for automated access. Use at your own
> risk and discretion.

## Why this exists

Ooma's own voicemail-to-email/SMS notifications are [gated behind the
**Premier** subscription tier](https://www.ooma.com/home-phone-service/resources/features/voicemail-notifications/)
— on the **Basic** plan, which is what most residential lines are
actually on, the only way to know a voicemail arrived is to open the My
Ooma app or web portal and check. Even on Premier, [Ooma's own support
forums](https://forums.ooma.com/viewtopic.php?f=5&t=17854) have long-running
threads of notification emails that just don't show up, or an attachment
toggle that silently reverts to "off" after being saved — with no fix
beyond "check your spam folder."

This tool closes that gap without upgrading a plan or depending on
Ooma's own delivery. Since Ooma has no public API for voicemail, it does
the next best thing: log into `my.ooma.com` the same way a browser would,
watch for anything new, and get you the MP3 immediately over whatever
channel you actually use — not just email, since it's built on
[Apprise](https://github.com/caronc/apprise), so Discord, ntfy, a phone
push notification, or any of 100+ other services work exactly the same
way.

## How this works (and why it's fragile)

Ooma has no documented public API for voicemail. This tool logs into
`my.ooma.com` the same way a browser would, then parses the HTML of the
voicemail inbox page (`tr[data-id]` rows) to find new messages, falling
back to the site's internal `/phone/voicemail/get_link` AJAX endpoint to
resolve a download URL when one isn't already in the row.

That means it's inherently coupled to Ooma's current site markup. It was
last verified working in December 2025. **If it stops finding voicemails,
the most likely cause is that Ooma changed their HTML** — open an issue
(or a PR) with a snippet of the new markup and it can likely be fixed
quickly; the parsing logic is isolated in a handful of small functions in
[main.go](main.go) (`extractCaller`, `extractTimestamp`, `extractAudioURL`,
`loginOoma`).

## Features

- Polls on a configurable interval and downloads any voicemail it hasn't
  seen before (tracked in a local JSON state file)
- Notifies via [Apprise](https://github.com/caronc/apprise) — one
  integration point that fans out to email, Discord, Telegram, ntfy,
  Pushover, Matrix, and 100+ other services, with the MP3 attached —
  embedded in-process via [github.com/unraid/apprise-go](https://github.com/unraid/apprise-go),
  a pure-Go reimplementation. No Python, no sidecar service.
- Single static binary / minimal Docker image, no database required

## Requirements

- An Ooma account with a voicemail inbox at `my.ooma.com` (or your
  region's equivalent — see `OOMA_URL` below)
- Go 1.25+ if building from source (only relevant outside Docker)

## Quick start

```bash
git clone https://github.com/andiapps-dev/ooma-voicemail.git
cd ooma-voicemail
cp .env.example .env
# edit .env: at minimum set OOMA_USER, OOMA_PASS, and (optionally) APPRISE_URLS
docker compose up -d
```

Downloaded MP3s and the seen-voicemail state file land in `./data`.

### Without Docker

```bash
go build -o ooma-voicemail .
OOMA_URL=https://my.ooma.com/login OOMA_USER=... OOMA_PASS=... ./ooma-voicemail
```

## Configuration

All configuration is via environment variables (see `.env.example` for a
copyable template) plus two optional command-line flags.

| Variable | Required | Description |
|---|---|---|
| `OOMA_URL` | yes | Your Ooma login page, e.g. `https://my.ooma.com/login`. The host portion is reused for every other request the checker makes. |
| `OOMA_USER` | yes | Ooma account username/email. |
| `OOMA_PASS` | yes | Ooma account password. |
| `APPRISE_URLS` | no | One or more comma-separated [Apprise URLs](https://github.com/caronc/apprise/wiki), e.g. `mailto://user:pass@smtp.example.com,discord://webhook_id/webhook_token`. If unset, voicemails are still downloaded, just not announced anywhere. |
| `CHECK_INTERVAL` | no | How often to poll, as a Go duration (`5m`, `15m`, `1h`, ...). Defaults to `15m`. |

| Flag | Default | Description |
|---|---|---|
| `--state-file` | `voicemails.json` | Where to persist the set of voicemail IDs already seen. |
| `--mp3-dir` | `./voicemails` | Where downloaded MP3s are written, named `<id>.mp3`. |

The Docker image sets both flags to paths under `/data`, so mount a
volume there (see `docker-compose.yml`).

## Scheduling

`ooma-voicemail` is a long-running daemon: it checks once at startup, then
again every `CHECK_INTERVAL`, until it receives `SIGINT`/`SIGTERM`. Run it
with `docker compose up -d`, as a systemd service, or as a long-lived
Kubernetes Deployment — there's no separate cron/scheduler to wire up.

## Local development

```bash
go build ./...
go vet ./...
go test ./...
```

`main_test.go` covers the HTML-parsing helpers (`extractCaller`,
`extractTimestamp`, `extractAudioURL`) against fixture markup, so changes
to Ooma's page structure can be reproduced and fixed without needing a
live account.

## License

MIT — see [LICENSE](LICENSE).
