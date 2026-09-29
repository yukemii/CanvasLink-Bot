# CanvasLink on request-based Cloud Run

Status: a small cloud pilot was authorized on 30 September 2026. The Cloud Run
service is deployed and activated. Public health/authentication checks passed,
Telegram points to the cloud webhook, and authenticated scheduled runs returned
HTTP 204. The Mac poller/proxy are stopped. Real-account tests, public launch,
and Google verification remain pending. Existing Dulie
services are not modified.

## Prepared deployment

- Project `dulie-assistant`, Singapore `asia-southeast1`, service `canvaslink`.
- Separate runtime, build, and scheduler service accounts; no repository merge.
- Existing Cloud SQL instance and restricted `canvaslink_app` / `canvaslink` database.
- 1 vCPU, 512 MiB, request-based billing, 0 minimum / 1 maximum service instances,
  concurrency 8, 300-second request timeout, startup CPU boost disabled.
- Telegram `POST /telegram/webhook`, authenticated by an independent random secret
  in `X-Telegram-Bot-Api-Secret-Token`. Registration limits Telegram to one concurrent
  connection and accepts only messages and callbacks, without discarding updates.
- One Scheduler job each minute, `POST /internal/tick`, authenticated with a Google
  OIDC token. The application checks its signature, expiry, audience, issuer, and
  exact verified service-account email. Merely supplying Scheduler-looking headers
  is insufficient. A public service is required for Telegram and the OAuth callback.
- OAuth callback: `https://canvaslink-4523246116.asia-southeast1.run.app/oauth/callback`.
  This is the deterministic Cloud Run URL. The operator confirmed this callback
  was saved in Google Console.
- `GET /health` reports the running HTTP process; startup verifies the database.
  It is not proof of recent successful reminders or provider connectivity.

## Cost authorization

The user initially requested $0 additional hosting, then authorized a small
metered pilot on 30 September 2026. A **S$5/month alert** now covers resources
labelled `app=canvaslink` in `dulie-assistant`, at 50%, 90%, and 100%. Default
billing-account recipients receive notifications. It does not cover every
unlabelled charge (including some build/network costs) and is not a hard cap.
Preparation below is local only; cloud-changing steps require cost authorization. Cloud Run free allowances are shared by billing account, Scheduler offers
three free jobs per billing account, and builds/image storage/networking can also
cost money. Minimum zero and maximum one are not spending caps, and billing alerts
are not hard caps. No new domain is required.

Official references:
[Cloud Run pricing](https://cloud.google.com/run/pricing),
[Scheduler pricing](https://cloud.google.com/scheduler/pricing),
[request-based CPU allocation](https://docs.cloud.google.com/run/docs/configuring/billing-settings),
[Telegram webhooks](https://core.telegram.org/bots/api#setwebhook).

## Prepare and review (no cloud changes)

```sh
python3 deploy/cloud-run.py
python3 deploy/check-launch.py --env-file .local/cloud-run.env.json
```

This creates owner-only files in ignored `.local/`: a non-secret plan, a random
webhook secret, and runtime environment JSON. The root `.env` remains configured
for local polling. Runtime credentials never enter the Docker or Cloud Build
source context. The managed Cloud SQL Unix socket provides the encrypted remote
connection; `sslmode=disable` refers only to the local socket hop.

Credentials are supplied as Cloud Run environment variables, like Dulie's current
setup. Project/service administrators can read them; limit IAM access accordingly.
Do not publish the generated JSON, `.env`, source credentials JSON, or diagnostic
files. Replace previously chat-shared Telegram/Google credentials before launch.

## Deploy (only after cost authorization)

```sh
python3 deploy/cloud-run.py --deploy --accept-metered-costs
```

The script enables required APIs, creates dedicated identities, grants runtime
Cloud SQL access and build permissions, creates a dedicated Artifact Registry
repository, builds the container, and deploys the service. It adds a narrowly
scoped default-log-sink exclusion for this service's OAuth callback request logs
before deploying. Review other project/organization sinks for the same exclusion;
app logs do not log request URLs. Do not claim all infrastructure logs are sanitized
until inherited/export sinks have been checked.

The deploying identity needs permission to create these resources and to act as
the created service accounts. Errors are written only to `.local/deploy-error.json`
so provider responses cannot expose secrets in terminal output. A failed deployment
can leave created resources; review them before rerunning. The first remote container build and deployment succeeded. The initial health
path `/healthz` was intercepted by Google Frontend; the application now uses
`/health`. Verify that endpoint on the final revision before activation.

The script does **not** set Telegram's webhook or create a Scheduler job at this
stage. Verify health, and confirm that unauthenticated webhook and tick requests
return 401. Review service settings and the built image before activation. Keep the
Google consent audience in Testing and add the operator's Google account for a
private pilot; production verification is a separate step.

## Activate a private cloud pilot

1. Save the exact public callback URI above in the existing CanvasLink Google OAuth
   Web client. Keep the localhost URI too if development testing still needs it.
2. Stop the local `deploy/run-local.py` process; never use polling and webhooks with
   the same token simultaneously. Confirm no other host is polling that token.
3. Run:

   ```sh
   python3 deploy/cloud-run.py --activate --accept-metered-costs --google-redirect-saved
   python3 deploy/check-launch.py --env-file .local/cloud-run.env.json --check-telegram
   ```

   Activation checks health and that private local settings match the deployed
   revision, registers Telegram delivery without dropping updates, then creates
   or updates/resumes the OIDC Scheduler job. If scheduler setup fails after
   webhook registration, Telegram replies may work while reminders do not; resolve
   the failed step before treating the pilot as operational.
4. Confirm the Scheduler job succeeds and `/start`, Canvas connection, Google
   sign-in, event creation/update, and a real reminder work. Verify from another
   device: localhost must no longer be involved. Restart/redeploy once and verify
   saved preferences and acknowledged reminders persist.
5. Complete [Google verification](google-verification.md), finalize retention and
   backups, then remove pre-launch notices and enable the website's live bot CTA.
   Keep the site sample-only until the backend has passed these checks.

## Runtime behavior and limits

No Telegram polling, timer loop, or detached OAuth-notification queue runs in
webhook mode. Work finishes before its HTTP response. One scheduled request checks
existing reminders first, reserves due feeds using persistent timestamps, runs up
to four feeds concurrently, and processes a batch of up to 20 Google jobs. Canvas
checks default to hourly; failed attempts also wait that interval. New completed
onboardings are eligible on the next tick. Google jobs run on scheduled ticks
rather than the polling-mode 15-second timer.

A database lock prevents overlapping scheduler cycles across instances/revisions.
Existing per-user locks serialize calendar writes, feed changes, and reminders.
Maximum instances alone is not relied on for correctness. Reminder checks can be
late after cold starts, failures, long cycles, or downtime; monitor Scheduler
errors and per-user last-sync status. Larger workloads may need partitioned jobs
and a revised capacity/cost plan; this is a small-pilot setup, not unlimited scale.

Telegram update IDs (no message body/user mapping) are kept for seven days and
pruned by scheduled runs. A receipt is committed before dispatch, so retries do
not repeat a task-creation command. If the process dies mid-command, the user may
need to repeat it. This is deliberately at-most-once dispatch, not a claim of
transactional/exactly-once end-to-end delivery. Outbound Telegram reminders still
have the provider's send/ack crash window documented in README.

## Roll back to local polling

Pause `canvaslink-tick`, delete the Telegram webhook with
`drop_pending_updates=false` using a private token-handling tool, then wait for
in-flight cloud requests to drain (up to five minutes with this configuration).
Only then restart `python3 deploy/run-local.py`. Do not reset/delete the database
or change the stable instance ID/encryption key. A paused Scheduler job and stored
container images may still be billable; remove unneeded dedicated resources only
after confirming they are no longer used. Do not remove shared Dulie resources.
