# CanvasLink

CanvasLink is a standalone Telegram bot for Canvas deadlines, personalized reminders, daily/weekly agendas, and optional Google Calendar sync.

It was extracted from the [Dulie](https://github.com/markadodo/dulie) scheduling assistant codebase into its own independent project. CanvasLink is now fully self-contained with its own Google OAuth flow, database tables, and no runtime dependencies on Dulie.

## Landing page

The student-facing landing page lives in [`docs/`](docs/README.md). It includes an
interactive sync-mode and reminder/agenda previews, light/dark themes, responsive
layouts, and social sharing metadata.
Preview it with `python3 -m http.server 4173 --directory docs`, then open
http://localhost:4173. See the [site guide](docs/README.md) for GitHub Pages
deployment and live Telegram button configuration.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        CanvasLink Bot                           │
│                                                                 │
│  ┌──────────┐   ┌──────────────┐   ┌────────────────────────┐  │
│  │ Telegram  │──▶│  Sync Worker │──▶│  Google Calendar API   │  │
│  │  Bot API  │   │  (periodic)  │   │  (event lifecycle)     │  │
│  └────┬─────┘   └──────┬───────┘   └────────────────────────┘  │
│       │                │                                        │
│       ▼                ▼                                        │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                    Store (PostgreSQL)                     │   │
│  │  canvaslink_feeds | canvaslink_course_type_settings      │   │
│  │  canvaslink_synced_events | canvaslink_pending_actions   │   │
│  │  canvaslink_oauth_states | canvaslink_google_tokens      │   │
│  │  canvaslink_telegram_accounts                            │   │
│  └──────────────────────────────────────────────────────────┘   │
│       │                                                         │
│       ▼                                                         │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │              OAuth HTTP Server (built-in)                 │   │
│  │  - /oauth/callback — handles Google OAuth redirect       │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

## Features

- **Personalized Reminders**: Enabled one day before by default, with custom offsets, course/type overrides, quiet hours, and snoozing
- **Telegram Planner**: Today, next-seven-days, and paginated upcoming views with course filters, completion/undo, personal tasks, and personal targets
- **Scheduled Agendas**: Optional daily and weekly summaries at user-selected local times
- **Passive Connection Alerts**: Repeated-failure and recovery notices, plus before/after deadline-change messages
- **Dedicated Calendar**: Default CanvasLink calendar, writable-calendar selection, course colors, and optional course title prefixes

- **Canvas iCal Feed Parsing**: Fetches and parses Canvas LMS iCal feeds to detect courses, assignments, quizzes, and exams
- **Guided Onboarding**: Connect a Canvas feed, optionally link Google Calendar, and configure each module
- **Per-Course Settings**: Configure sync mode per course and assignment type (Auto, Active, Ignore)
- **Google Calendar Sync**: Idempotently creates, updates, and safely removes tracked Canvas assignments
- **Durable Calendar Jobs**: PostgreSQL-backed retries prevent duplicate or lost Google Calendar operations
- **Encrypted Secrets**: Canvas feed URLs and OAuth credentials are encrypted at rest
- **Timezone and All-Day Support**: User-specific display timezones and native Google all-day events
- **Self-Contained Google OAuth**: Full OAuth 2.0 flow with PKCE — Telegram presents the authorization URL as a clean inline button, while callback handling, token exchange, refresh, and storage are handled internally
- **Private Telegram Interface**: Inline keyboard UI is restricted to direct messages for account safety
- **Standalone Deployment**: CanvasLink runs independently without requiring the Dulie backend

## Tables

All CanvasLink tables use the `canvaslink_` prefix and are auto-created on startup:

| Table | Purpose |
|-------|---------|
| `canvaslink_telegram_accounts` | Telegram user → chat ID mapping |
| `canvaslink_feeds` | User iCal feed URLs |
| `canvaslink_course_type_settings` | Per-course, per-type sync mode settings |
| `canvaslink_synced_events` | Tracks which Canvas events have been synced to Google Calendar |
| `canvaslink_pending_actions` | Pending user actions (add to calendar, ignore) |
| `canvaslink_oauth_states` | Temporary OAuth state tokens (PKCE) |
| `canvaslink_google_tokens` | Google OAuth tokens (access + refresh) |
| `canvaslink_calendar_jobs` | Durable Google Calendar operations and retry state |
| `canvaslink_destructive_confirmations` | Short-lived, one-time disconnect/reset confirmations |

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `CANVASLINK_TELEGRAM_BOT_TOKEN` | Yes | Telegram Bot API token |
| `CANVASLINK_DATABASE_URL` | Yes | PostgreSQL connection string; use verified TLS for remote/production databases |
| `CANVASLINK_ENCRYPTION_KEY` | Yes | Base64-encoded 32-byte key used to encrypt stored secrets |
| `CANVASLINK_ENCRYPTION_KEY_ID` | No | Identifier embedded in new ciphertext (default: `primary`) |
| `CANVASLINK_INSTANCE_ID` | No | Stable deployment namespace for event IDs and ownership markers (default: `canvaslink`) |
| `CANVASLINK_PREVIOUS_ENCRYPTION_KEYS` | No | Old `key_id:base64_key` pairs retained temporarily during a coordinated key rotation |
| `CANVASLINK_GOOGLE_CLIENT_ID` | No* | Google OAuth client ID |
| `CANVASLINK_GOOGLE_CLIENT_SECRET` | No* | Google OAuth client secret |
| `CANVASLINK_OAUTH_REDIRECT_URL` | No | OAuth callback URL (default: `http://localhost:9090/oauth/callback`) |
| `CANVASLINK_OAUTH_LISTEN_ADDR` | No | OAuth callback listener address (default: `127.0.0.1:9090`); use `:9090` only when a private container network requires it |
| `CANVASLINK_SYNC_INTERVAL` | No | Global sync interval, minimum `1m` (default: `1h`) |
| `CANVASLINK_DEFAULT_TIMEZONE` | No | IANA timezone for new users (default: `Asia/Singapore`) |
| `CANVASLINK_REMOVAL_MISSES` | No | Successful feeds an event must be absent from before removal (default: `3`) |
| `CANVASLINK_REMOVAL_GRACE_PERIOD` | No | Minimum absence time before removal (default: `6h`) |
| `CANVASLINK_CALENDAR_JOB_INTERVAL` | No | Durable calendar-job retry interval (default: `15s`) |
| `CANVASLINK_ALLOW_INSECURE_FEEDS` | No | Development override permitting HTTP feeds (default: `false`) |
| `CANVASLINK_ALLOW_PRIVATE_FEEDS` | No | Development override permitting private-network feeds (default: `false`) |
| `CANVASLINK_COURSE_REGEX` | No | Custom course-code detection regex |

\* Set both Google variables to enable Google Calendar. Without them, Telegram review notifications still work.

## Quick Start

### Prerequisites

- Go 1.25+
- PostgreSQL database
- Telegram Bot Token (from [@BotFather](https://t.me/botfather))
- Google Cloud project with Calendar API enabled (optional, for calendar sync)

### Google Cloud Console Setup

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Enable the **Google Calendar API**
3. Create OAuth 2.0 credentials (Web application type)
4. Add the redirect URI: `http://localhost:9090/oauth/callback` (or your production URL)
5. Copy the Client ID and Client Secret

### Setup

1. Clone the repository:
   ```bash
   git clone https://github.com/markadodo/canvaslink.git
   cd canvaslink
   ```

2. Copy and fill in environment variables:
   ```bash
   cp .env.example .env
   openssl rand -base64 32
   # Edit .env with your values
   ```

   Put the generated value in `CANVASLINK_ENCRYPTION_KEY`. Keep this key in a
   secrets manager or secure backup; losing it makes encrypted feeds and tokens
   unrecoverable.

3. Run (tables are auto-created on startup):
   ```bash
   go run main.go
   ```

### Build

```bash
go build -o canvaslink .
```

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/start` | Start or resume guided onboarding |
| `/help` | Full command guide, defaults, and feature explanations |
| `/connect_google` | Link Google Calendar using an inline authorization button |
| `/settings` | Configure sync modes, reminders, agendas, and calendar preferences |
| `/today` | Show today's outstanding work |
| `/week` | Show the next seven days |
| `/upcoming` | Browse upcoming work, filter by course, and open task actions |
| `/completed` | Browse completed items and undo completion |
| `/add` | Create a personal task in Telegram |
| `/reminders` | Set reminder offsets, course/type overrides, and quiet hours |
| `/agenda` | Enable or configure daily/weekly summaries |
| `/timezone` | Show or change the timezone used in Telegram messages |
| `/disconnect_canvas` | Remove the feed and course settings; keep Google events, personal tasks, and notification preferences |
| `/disconnect_google` | Remove the Google authorization while retaining calendar events |
| `/reset` | Verified wipe of CanvasLink-owned events, followed by deletion of local data |
| `/cancel` | Cancel pending planner text input |

## Sync Modes

| Mode | Behavior |
|------|----------|
| **Auto** | Silently syncs to Google Calendar without user confirmation |
| **Active** | Sends a Telegram confirmation card for each new item |
| **Ignore** | Skips the item entirely |

Changing a Canvas event updates its tracked Google event. If a professor cancels
an upcoming event explicitly, CanvasLink safely removes the corresponding tracked
Google event. For an item merely missing from a feed, deletion requires multiple
successful checks and a grace period. The configured minimum is three checks
over six hours by default; losing the complete set of one or two upcoming
events requires at least six checks over 24 hours, while a majority loss of
three or more requires at least twelve checks over seven days. Reappearance
cancels pending deletion immediately. Past calendar history and events not
created by CanvasLink are never automatically removed.

Disconnecting Canvas intentionally removes the feed and course settings, but
keeps both existing Google events and the ownership records needed for a later
verified wipe. Disconnecting Google removes the stored authorization (and makes
a best-effort provider revocation) without changing calendar events. Reconnect
the same Google account if those retained events should be wiped later.

The explicit **Wipe / Reset** action first requires a short-lived confirmation.
For every deletion, CanvasLink checks the deterministic event ID, private
deployment marker, hashed Canvas source UID, absence of attendees, and the
event's current revision. Local tracking is removed only after the verified
deletions finish. A mismatch stops the wipe; the unverified event is untouched
and local tracking is retained so the operation can be reviewed or retried.

Events created by older CanvasLink versions without the private ownership
markers are treated as unverified. CanvasLink refuses to delete them
automatically; remove such an event manually in Google Calendar, then retry the
wipe if needed.

## Production Security

- Use an HTTPS `CANVASLINK_OAUTH_REDIRECT_URL` in production. Plain HTTP is
  accepted only for localhost or loopback development callbacks.
- On a single host, set `CANVASLINK_OAUTH_LISTEN_ADDR=127.0.0.1:9090` and place
  a TLS reverse proxy in front of it. In a container, `:9090` may be necessary;
  keep that port on a private container network and expose only the proxy.
- Do not record OAuth callback query strings in proxy, CDN, load-balancer, or
  application access logs. They contain a short-lived authorization code and
  state value. Redact or omit the query component for `/oauth/callback`.
- `sslmode=disable` is suitable only for a trusted local PostgreSQL connection.
  For a remote production database, use certificate and hostname verification
  such as `sslmode=verify-full` with the appropriate trusted CA configuration.
- Give each independent deployment a unique, stable
  `CANVASLINK_INSTANCE_ID`. Changing it later prevents CanvasLink from
  recognizing events created under the old namespace.

### Encryption key rotation

Secret-field migration runs during database initialization. Rotate keys as a
coordinated deployment:

1. Back up the database and every active encryption key.
2. Stop or drain all CanvasLink instances and workers that use the old primary
   key. Do not mix old and new writers during migration.
3. Generate a new key and key ID. Configure them as
   `CANVASLINK_ENCRYPTION_KEY` and `CANVASLINK_ENCRYPTION_KEY_ID`, and put the
   old `key_id:key` pair in `CANVASLINK_PREVIOUS_ENCRYPTION_KEYS`.
4. Start one updated instance and let startup/schema initialization complete,
   then start the remaining instances with the identical key configuration.
5. Remove the previous key only after all sensitive rows have been migrated,
   all old processes are gone, and a verified backup of the old key exists.

Losing a still-needed key makes the corresponding encrypted feed URLs or OAuth
tokens unrecoverable.

## Verification

```bash
go test ./...
go vet ./...
go build ./...
```

Set `CANVASLINK_TEST_DATABASE_URL` to a PostgreSQL test database to run the
live lifecycle tests. Each test uses and removes its own randomly named schema;
the included CI workflow runs these tests automatically.

## Dependencies

- [go-telegram-bot-api/v5](https://github.com/go-telegram-bot-api/telegram-bot-api) — Telegram Bot API
- [pgx/v5](https://github.com/jackc/pgx) — PostgreSQL driver
- [godotenv](https://github.com/joho/godotenv) — Environment file loader
- [golang.org/x/oauth2](https://pkg.go.dev/golang.org/x/oauth2) — Google OAuth2
- [google.golang.org/api/calendar/v3](https://pkg.go.dev/google.golang.org/api/calendar/v3) — Google Calendar API

## License

[MIT](LICENSE)

## Student planner and reminders

The bot now includes a Telegram planner alongside Google Calendar syncing:

| Command | What it does |
|---|---|
| `/today` | Today's outstanding items in the student's timezone |
| `/week` | Outstanding items in the next seven local calendar days |
| `/upcoming` | Paginated list with course filters and item actions |
| `/completed` | Completed items, with an undo action |
| `/add` | Create a personal task with a due date |
| `/reminders` | Reminder defaults, course/type overrides, and quiet hours |
| `/agenda` | Configure scheduled daily and weekly summaries |
| `/cancel` | Cancel an in-progress text entry |

All these features are also accessible from `/settings`.

### Reminder behavior

- **Enabled by default, one day before.** Reminders are independent of Google
  Calendar approval and apply to non-ignored feed items and personal tasks.
- Choose presets (one hour, one day, one week), `off`, or up to five custom
  offsets such as `1h, 1d, 1w`. Supported units are minutes, hours, days, and weeks,
  with a maximum offset of 30 days.
- Override the default per course, then per assignment type. Explicit **Off**
  overrides inherited reminders; **Use inherited default** removes an override.
- Quiet hours default to **22:00–08:00** in the user's timezone. Reminders and
  agendas wait until quiet hours end. Connection health alerts bypass quiet hours.
  Equal start/end hours disable quiet hours.
- Timed reminders use elapsed offsets. All-day offsets count back from 09:00 on
  the local due date; whole-day offsets preserve that wall time across DST changes.
- The scheduler runs every minute. After downtime or late discovery, only the
  most recently elapsed offset is caught up, and only before the target/deadline.
  It does not send a burst for every missed offset.
- **Done** is local to CanvasLink, stops reminders, and can be undone. It does not
  submit anything to Canvas or delete a Google event. **Snooze 1h** schedules one
  follow-up and then resumes future offsets.
- A personal target replaces the reminder reference time while leaving the
  official deadline unchanged. If a Canvas change moves the official deadline
  before the target, reminders fall back to the official deadline.
- Missing, cancelled, ambiguously duplicated, and ignored items do not receive
  reminders. Missing items remain stored; this does not bypass the existing
  safety checks for deleting Google events.

### Agendas, tasks, and onboarding

Scheduled agendas are **off by default**. Students can enable daily summaries
(default 08:00) and weekly summaries (default Sunday 18:00), and configure their
own times. `/today` and `/week` always work. Digests omit done/ignored items and
include the last successful Canvas check where available.

Personal tasks use `YYYY-MM-DD HH:MM | title` during `/add`, interpreted in the
student's timezone. Tasks support completion, snoozing, editing their due date,
setting/clearing a personal target, and deletion. Personal tasks live in Telegram;
they are not automatically synced to Google Calendar.

Onboarding accepts valid empty feeds, previews upcoming work when available, and
lets students apply one default to all courses or customize individual types.
New courses inherit the selected default. Setup explicitly explains that every
course/type and reminder setting can be changed later. A successful first feed
check queues a summary; its counts describe sync preferences, not confirmation
that all pending Google operations have completed.

### Passive connection health

Canvas produces one warning after three consecutive failed reads, followed by a
recovery notice when checks succeed. Google is checked during feed sync even
when no assignments changed. Invalid authorization produces a reconnect prompt;
other repeated connection failures use the same three-check threshold. These
checks are periodic, not instantaneous. An intentionally disconnected account
does not keep receiving pending connection warnings. A bot/server outage itself
requires separate external uptime monitoring.

### Dedicated Google Calendar

New events default to a separate **CanvasLink** calendar, created lazily on the
first calendar sync (or using **Use dedicated CanvasLink calendar** in settings).
Students can choose another writable calendar, toggle a course prefix in event
titles, and choose per-course event colors. Appearance changes apply on the next
create/update. Destination changes affect newly tracked events; existing events
and durable retries remain pinned to their recorded calendar.

The Google authorization flow now requests these scopes:

- `https://www.googleapis.com/auth/calendar.events`
- `https://www.googleapis.com/auth/calendar.calendars`
- `https://www.googleapis.com/auth/calendar.calendarlist.readonly`

Existing test accounts should reconnect with `/connect_google` to grant the
additional calendar creation/list permissions. Configure these permissions in
your Google OAuth consent setup before rolling the bot out. Existing credentials
and environment-variable names are unchanged.

### Persistence and delivery guarantees

Startup creates the planner preferences, tasks, deliveries, connection-health,
and expiring-input tables automatically. Notifications use a durable outbox
with unique logical keys; user locks serialize scheduling, settings, reset, and
send attempts. Transient send failures retry after five minutes, and deliveries
are rechecked against current task state/settings before sending. Old expired
outbox records are pruned after 30 days.

Telegram's send API does not provide an idempotency key. If Telegram accepts a
message but the response or subsequent database acknowledgement is lost, a retry
can repeat it. Normal scheduler ticks and process restarts after acknowledgement
do not duplicate deliveries. Google event writes retain their existing
idempotency and ownership protections.

Disconnecting Canvas removes feed-backed planner items while retaining personal
tasks and preferences. A full reset removes all planner data, including pending
reminders and text-entry state, after the existing verified Google wipe flow.

### Before releasing the planner

Automated tests cover PostgreSQL persistence, reminder delivery, and mocked
Google/Telegram requests. A real account smoke test is still required before
promoting this release to `main`:

1. Run the development branch with a separate test bot token and test database.
   Stop it before switching that token back to another instance; do not run two
   Telegram polling instances with the same token.
2. Configure the Google consent permissions listed above and reconnect a test
   account. Confirm that a dedicated CanvasLink calendar is created, a test event
   is added, and a changed deadline updates that same event.
3. Check `/start`, `/help`, course settings, and `/upcoming` with a test Canvas
   feed. Use a near-future personal task and a one-minute offset to verify an
   actual reminder; check Done/Undo, quiet hours, and a scheduled agenda too.
4. Restart the test instance and confirm preferences survive and acknowledged
   notifications are not repeated. Disconnect/reconnect Google and confirm the
   recovery path works.
5. Deploy the tested backend before publishing the matching landing-page claims.
   Pushing `docs/` to `main` publishes the site through GitHub Pages; it does not
   deploy the Go bot. Preserve a database backup before upgrading an existing
   instance.
