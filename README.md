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
│  │  Bot API  │   │  (periodic)  │   │  (event creation)      │  │
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
│  │  - /oauth/status — checks connection status              │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

## Features

- **Canvas iCal Feed Parsing**: Fetches and parses Canvas LMS iCal feeds to detect courses, assignments, quizzes, and exams
- **Per-Course Settings**: Configure sync mode per course and assignment type (Auto, Active, Ignore)
- **Google Calendar Sync**: Creates Google Calendar events for Canvas assignments
- **Self-Contained Google OAuth**: Full OAuth 2.0 flow with PKCE — auth URL generation, callback handling, token exchange, token refresh, and token storage are all handled internally
- **Telegram Bot Interface**: Full inline keyboard UI for managing settings and pending actions
- **No External Dependencies**: CanvasLink runs independently without requiring the Dulie backend

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

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `CANVASLINK_TELEGRAM_BOT_TOKEN` | Yes | Telegram Bot API token |
| `CANVASLINK_DATABASE_URL` | Yes | PostgreSQL connection string |
| `CANVASLINK_GOOGLE_CLIENT_ID` | Yes | Google OAuth client ID |
| `CANVASLINK_GOOGLE_CLIENT_SECRET` | Yes | Google OAuth client secret |
| `CANVASLINK_OAUTH_REDIRECT_URL` | No | OAuth callback URL (default: `http://localhost:9090/oauth/callback`) |
| `CANVASLINK_OAUTH_LISTEN_ADDR` | No | OAuth HTTP server address (default: `:9090`) |
| `CANVASLINK_SYNC_INTERVAL` | No | Sync interval (default: `1h`) |

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
   # Edit .env with your values
   ```

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
| `/start` | Welcome message |
| `/connect_canvas <ical_url>` | Connect your Canvas iCal feed |
| `/connect_google` | Link Google Calendar (opens OAuth URL) |
| `/settings` | Open settings UI to configure sync modes |

## Sync Modes

| Mode | Behavior |
|------|----------|
| **Auto** | Automatically syncs to Google Calendar without user confirmation |
| **Active** | Sends a Telegram confirmation card for each new item |
| **Ignore** | Skips the item entirely |

## Dependencies

- [go-telegram-bot-api/v5](https://github.com/go-telegram-bot-api/telegram-bot-api) — Telegram Bot API
- [pgx/v5](https://github.com/jackc/pgx) — PostgreSQL driver
- [godotenv](https://github.com/joho/godotenv) — Environment file loader
- [golang.org/x/oauth2](https://pkg.go.dev/golang.org/x/oauth2) — Google OAuth2
- [google.golang.org/api/calendar/v3](https://pkg.go.dev/google.golang.org/api/calendar/v3) — Google Calendar API

## License

MIT
