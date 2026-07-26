# CanvasLink

CanvasLink is a standalone Telegram bot that syncs Canvas LMS iCal feeds to Google Calendar.

It was extracted from the [Dulie](https://github.com/markadodo/dulie) scheduling assistant codebase into its own independent project. CanvasLink is now fully self-contained with its own Google OAuth flow, database tables, and no runtime dependencies on Dulie.

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
- Google Cloud project with Calendar API enabled

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
| `/connect_google` | Link Google Calendar using an inline authorization button |
| `/settings` | Open settings UI to configure sync modes |
| `/timezone` | Show or change the timezone used in Telegram messages |
| `/disconnect_canvas` | Remove the Canvas feed and settings while retaining calendar events |
| `/disconnect_google` | Remove the Google authorization while retaining calendar events |
| `/reset` | Verified wipe of CanvasLink-owned events, followed by deletion of local data |

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
