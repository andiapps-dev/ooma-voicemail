# Changelog

All notable changes to this project are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project uses [Semantic Versioning](https://semver.org/) — tags look
like `v0.1.0`, and each one gets its own section below.

## [Unreleased]

## [0.1.1] - 2026-09-14

### Changed

- Docker build now uses Go 1.27 and Alpine 3.24 (previously 1.25/3.20),
  and `goquery` (the HTML parser the Ooma scraping logic depends on) is
  updated from 1.11.0 to 1.13.0.
- CI now actually builds the Dockerfile on every push/PR, not just
  `go vet`/`build`/`test` — a base-image bump previously could pass CI
  without the image itself ever being built.

## [0.1.0] - 2026-09-14

### Added

- Initial release: logs into an Ooma account, downloads new voicemails
  as MP3s, and tracks what's already been seen in a local JSON state
  file.
- Notifications via [Apprise](https://github.com/caronc/apprise) —
  email, Discord, ntfy, Telegram, Pushover, and 100+ other services —
  embedded in-process via
  [github.com/unraid/apprise-go](https://github.com/unraid/apprise-go),
  with the MP3 attached. No Python, no sidecar service required.
- Runs as a long-lived daemon on a configurable `CHECK_INTERVAL`, no
  external cron/scheduler needed.
- Single static binary / minimal Docker image (`docker compose up -d`
  to run).
