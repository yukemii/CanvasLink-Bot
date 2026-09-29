# CanvasLink launch status — 30 September 2026

## Running as a private cloud pilot

- Cloud Run service `canvaslink` in project `dulie-assistant`, Singapore.
  Active revision: `canvaslink-00002-9vw`.
  Public service: https://canvaslink-4523246116.asia-southeast1.run.app
- Request-based billing, 1 vCPU / 512 MiB, zero minimum and one maximum service
  instances, concurrency 8, startup CPU boost disabled. No always-on CPU required.
- `@CanvasLink_bot` now uses the authenticated cloud webhook. The local polling
  bot and Cloud SQL proxy are stopped; this Mac does not need to remain awake.
- `canvaslink-tick` is enabled every minute with an OIDC service-account identity.
  Authenticated runs returned HTTP 204, including empty cycles of 25–47 ms.
  Those empty-cycle timings are not a cost guarantee for real workloads.
- Canvas feeds default to hourly checks; reminders and queued Google work run on
  scheduled ticks. Unauthenticated requests cannot invoke the protected endpoints.
- Separate database `canvaslink`, restricted login `canvaslink_app`, 15 tables,
  and a distinct encryption key on the existing `dulie-db` Cloud SQL instance.
  Dulie's databases, runtime services, and existing credentials were not changed.
- Google credentials are configured; the operator confirmed the public callback
  was saved in the CanvasLink Web OAuth client. Calendar scopes are minimized.
  Successful real Google consent/calendar writes have **not** yet been verified.

## Cost and retention

The user authorized a small metered pilot after the initial $0 requirement.
A **S$5 monthly alert** covers resources labelled `app=canvaslink`, with thresholds
at 50%, 90%, and 100%, sent to default billing-account recipients. It is **not a
hard spending cap** and excludes unlabelled charges such as some build costs.
Free allowances are shared with other workloads on the billing account.

The default application-log bucket retains logs for 30 days. A service-specific
exclusion prevents OAuth callback request URLs from being stored in the default
sink; the project currently has only `_Default` and `_Required` sinks. The shared
Cloud SQL instance reports automated backups disabled; this was not changed.

## Validation completed

- Full Go vet, race tests, and PostgreSQL integration suite; isolated test schemas.
- Retry/duplicate Telegram updates, scheduler overlap, persistent feed cadence,
  and acknowledged reminder delivery across worker restarts.
- Remote container build and actual Cloud Run startup/HTTP smoke checks.
  `/health` returns 200; missing/invalid endpoint authentication returns 401;
  malformed authenticated Telegram JSON is rejected; OAuth callback is reachable.
  `/healthz` is intercepted by Google Frontend, so it is intentionally not used.
- Google Cloud Scheduler OIDC authentication and actual successful requests.
- Telegram `getMe` and webhook configuration verified without sending artificial
  user messages. Live account-level testing is still needed.
- Development code commit `9198d11`, CI successful:
  https://github.com/yukemii/CanvasLink-Bot/actions/runs/36599880226
- Website checked in both themes at mobile/desktop widths; build, lint, and
  formatting checks pass. Website remains a sample demo with private-pilot notices.

## What still needs the operator

1. In Telegram, run `/start`, connect an authorized Canvas feed, and connect Google.
   Confirm event creation/update and a near-future reminder, then test restart
   persistence with real account data. Add the Google account under Audience →
   Test users if the Google app remains in Testing and access is denied.
2. Replace credentials previously shared in chat before inviting public users.
   Save replacements locally and redeploy/re-register the webhook as appropriate.
3. Review backups and final retention; complete domain ownership verification,
   Google consent-screen fields, scope justification, and demonstration video.
   See [Google verification](google-verification.md). Hosting is not Google approval.
4. Only after these checks, remove pre-launch notices, enable the live website CTA,
   and promote the tested release. Do not announce a verified public launch yet.

## Operations

See [Cloud Run deployment and rollback](cloud-run.md). Check current cloud setup:

```sh
python3 deploy/check-launch.py --env-file .local/cloud-run.env.json --check-telegram
```

Private settings and the independent webhook secret are in owner-only, ignored
`.local/` files. The root `.env` remains a local-polling configuration for rollback.
`deploy/run-local.py` now refuses to start while Telegram has an active webhook.
Do not restart it unless the cloud scheduler is paused, the webhook is removed
without dropping updates, and in-flight requests have drained.
