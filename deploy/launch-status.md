# CanvasLink launch status

## Completed

- Cloud SQL database `canvaslink`, restricted login `canvaslink_app`, and the original 14 tables
  on the existing `dulie-assistant:asia-southeast1:dulie-db` instance.
- Private `.env` credentials, distinct application encryption key, and verified
  Telegram token for `@CanvasLink_bot` (no webhook configured).
- Google project `canvaslink-498509` found; Calendar API already enabled.
- Local launcher that builds the bot, starts a loopback-only encrypted Cloud SQL
  proxy, starts the bot, and cleans up both processes on exit.
- Container build recipe with a non-root, shell-free runtime and embedded timezone
  data. Build contexts exclude `.env`, Git history, and website content.
- Pre-launch homepage, privacy, and terms published at
  https://site.dulie.app/canvaslink/ through Dulie’s GitHub Pages deployment
  (commit `dcbb036`); all three URLs verified with HTTP 200.
- Scope minimized from `calendar.calendars` to `calendar.app.created` for creating
  the dedicated calendar; existing-calendar event and calendar-list access retained.

## Webhook implementation prepared

- Authenticated Telegram and OIDC Scheduler endpoints; no timer/polling loops in
  webhook mode, synchronous Google OAuth completion.
- PostgreSQL update receipts, scheduler overlap lock, and persistent feed cadence.
- Unit and PostgreSQL integration/race tests passed, including retry and overlap
  behavior. Additive schema creates a receipts table and feed-attempt timestamp.
- Private Cloud Run environment and deployment plan generated in ignored `.local/`.
  No deployment/activation has been performed.

## Run locally without adding a hosting service

```sh
python3 deploy/check-launch.py --local --check-telegram
python3 deploy/run-local.py
```

Run from a terminal on this computer. Keep it awake and connected. Ctrl+C stops
the bot and proxy; the launcher exits if either process fails. It prevents a
second launcher on this computer, but it is not a distributed leader lock. Never
run the same Telegram token on another host at the same time.

This is a development/pilot setup, not an always-on deployment. It does not create
a new hosting bill; existing Cloud SQL usage/traffic continues under its existing
billing. Avoid relying on this laptop for important deadline reminders.

For a full production check, omit `--local`. Missing Google credentials and a
production HTTPS callback must be resolved for a Calendar-enabled launch. The
checker only reports configuration presence and safe read-only Telegram results;
it never prints credential values, sends messages, or starts polling.

## Remaining inputs and public-launch work

1. Google Web client credentials are saved privately and local configuration checks
   pass. Live Google account consent and calendar-write tests are still pending.
2. Complete live Canvas-feed, reminder, restart, and Google account tests using
   accounts/feed data the operator is authorized to use.
3. Webhook mode and authenticated scheduled checks are implemented. The
   [Cloud Run setup](cloud-run.md) prepares a separate scale-to-zero service in
   Dulie's project, sharing only the existing database instance. The user authorized a small metered pilot after reviewing the original $0
   constraint. A S$5/month labelled-resource alert is configured (not a hard cap);
   build/deployment verification is in progress.
4. Set up the stable public HTTPS OAuth callback, with query-string logging omitted.
5. Review backups: the shared instance reports automated backups disabled. Do not
   change Dulie's shared retention/cost settings without an agreed plan. Finalize
   actual log, backup, and deletion practices before publishing final policies.
6. Verify domain ownership in Google Search Console and complete Google review.
7. Only after backend verification, remove the website's pre-launch notice and
   switch `docs/config.js` to the live bot link. Sync website changes to the Dulie
   website repo. GitHub Pages publishes the site, never the Go bot.

## Container preparation

```sh
docker build -t canvaslink:local .
```

Supply secrets at runtime, never as build arguments. The image listens on port
9090, which must stay private behind the deployment's HTTPS endpoint. Its database
URL must target the chosen host's Cloud SQL connector/proxy, not this laptop.

The Linux static binary has been cross-compiled. A Docker engine is not installed
on this computer, so the image itself has not been built or run here. Do not
promote it without a container smoke test.

Cloud Run can now use request-based billing with zero minimum instances through
webhook mode. Database locks protect overlap, and scheduled requests execute
background work before responding. See the [runbook](cloud-run.md) for preparation,
activation, limitations, and rollback. Container build/cloud smoke tests remain
pending until metered deployment is authorized.
