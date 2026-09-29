# CanvasLink launch status

## Completed

- Cloud SQL database `canvaslink`, restricted login `canvaslink_app`, and 14 tables
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

1. Download CanvasLink's **Web application** OAuth client JSON and provide its
   local file path. The [Google setup guide](google-verification.md) gives the
   exact test callback, consent links, scope reasons, and demonstration checklist.
2. Complete live Canvas-feed, reminder, restart, and Google account tests using
   accounts/feed data the operator is authorized to use.
3. Choose an always-on host within the user's **$0 additional hosting** constraint.
   No new paid service has been created. Existing Dulie services run on Cloud Run,
   not a shared VM available for an additional process. CanvasLink's background
   polling needs continuous CPU, so ordinary request-based scale-to-zero hosting
   is not sufficient. A separate always-on Cloud Run service is not guaranteed free.
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

Cloud Run would additionally need instance-based billing, minimum one instance,
and safe handling of overlapping revisions/instances. Maximum-instance settings
alone are not a distributed polling lock. No Cloud Run deployment command is
provided until those lifecycle requirements and hosting cost are resolved.
