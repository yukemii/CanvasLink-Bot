# CanvasLink Google OAuth preparation

Status: Calendar API is enabled in the existing **CanvasLink** project
`canvaslink-498509`. The private Cloud Run pilot has Google credentials configured,
and the operator confirmed the public callback was saved. Real Google consent and
calendar writes, branding/consent settings, and verification remain to be checked.
Hosting alone does not mean Google has approved the app.

## Console values

Open [Google Auth Platform](https://console.cloud.google.com/auth/overview?project=canvaslink-498509)
with the account that manages this project. Keep Dulie's project and OAuth client
unchanged.

| Field                               | CanvasLink value                           |
| ----------------------------------- | ------------------------------------------ |
| App name                            | CanvasLink                                 |
| Operator                            | Ke Mi, Singapore                           |
| Support/privacy contact             | dulie.business@gmail.com                   |
| Homepage                            | https://site.dulie.app/canvaslink/         |
| Privacy                             | https://site.dulie.app/canvaslink/privacy/ |
| Terms                               | https://site.dulie.app/canvaslink/terms/   |
| Authorized domain                   | dulie.app                                  |
| Audience for eventual public launch | External                                   |

The support-email dropdown only offers eligible addresses for the account/project.
If the shared support inbox is not offered, arrange appropriate account/group
access; do not pick an unrelated address or claim it is configured already.

The site and policy pages are explicitly pre-launch. Finalize the hosting,
retention, backups, and live-account checks before removing those notices or
submitting the production verification request.

## Private cloud pilot callback

The Web client must include exactly:

```text
https://canvaslink-4523246116.asia-southeast1.run.app/oauth/callback
```

The operator confirmed this was saved on 30 September 2026. Keep the Google
account used for testing in Audience → Test users if the consent app is in Testing.
The callback is served by Cloud Run; the homepage and policies remain on Pages.

## Local test client

Open [Clients](https://console.cloud.google.com/auth/clients?project=canvaslink-498509).
Create or select an OAuth client of type **Web application** for local testing.
Register exactly `http://localhost:9090/oauth/callback` as an authorized redirect.
No authorized JavaScript origin is needed for this server-side OAuth flow.

Keep the audience in **Testing** and add the Google account(s) used for the test.
Store the client ID and full client secret privately in `.env` as
`CANVASLINK_GOOGLE_CLIENT_ID` and `CANVASLINK_GOOGLE_CLIENT_SECRET`.
JSON is not required. Google only displays a secret when it is created. If the
existing secret is masked and its full value was not saved, select **Add Secret**
on the client's details page and save the new value immediately. Keep the old
secret enabled until the replacement has been tested.
Do not paste client secrets in chat or place them under `docs/` or Git.
See [Google's client-secret guidance](https://support.google.com/cloud/answer/15549257?hl=en#client-secret-hashing).
Google's Testing-mode Calendar refresh tokens expire after seven days; this is a
test limitation, not evidence of a bot persistence bug.

Before public production rollout, separate development/testing OAuth credentials
into a different project and keep only production clients/redirects in the
production project. Production callbacks need a stable HTTPS backend URL. The
static Pages site cannot process OAuth callbacks. A localhost test callback only
works on the computer running the bot (for example, with Telegram Desktop and a
browser on that same computer); it is not usable by arbitrary remote users.

## Current scopes and justifications

Declare exactly these scopes in **Data Access** and the verification submission:

- `https://www.googleapis.com/auth/calendar.events`: CanvasLink creates, reads,
  updates, and safely removes tracked assignment events. Students may explicitly
  choose an existing writable calendar, including a shared calendar they can edit.
  Read-only scopes cannot write events; app-created-calendar access alone cannot
  serve existing destinations, and owned-calendar-only access excludes shared
  writable calendars. The app does not import unrelated events into its planner.
- `https://www.googleapis.com/auth/calendar.app.created`: creates the dedicated
  CanvasLink calendar. The app uses `calendars.insert`; Google supports this
  narrower permission, so the app no longer requests `calendar.calendars`, which
  would permit changing properties of unrelated calendars.
- `https://www.googleapis.com/auth/calendar.calendarlist.readonly`: lists calendars
  and their access roles to offer writable destinations and rediscover a dedicated
  calendar after interrupted creation. The app does not need to edit calendar-list
  subscriptions, so the writable calendar-list scope is not requested.

These statements describe implemented behavior, not a completed live Google test.
Reconfirm them against the deployed release and record an actual account demo.

## Demonstration recording checklist

Use test assignments and an account you control; do not expose private feed URLs,
client secrets, access tokens, or unrelated personal calendar information.

1. Show the CanvasLink homepage and its privacy/terms links.
2. Show Telegram onboarding, optional Google connection, and the complete English
   Google consent screen with the correct app name and requested permissions.
3. Show creation of the dedicated CanvasLink calendar and a test assignment event.
4. Change a test deadline and show the same tracked event update, not duplicate.
5. Show selecting another writable calendar and syncing a new item there.
6. Demonstrate a safe verified reset with test events, and explain that disconnect
   retains existing Google events while reset attempts a verified wipe.
7. Show local planner completion does not submit to Canvas or remove Google events.
8. Upload an unlisted demonstration video and submit its URL with scope reasons.

Google Search Console must confirm ownership of `dulie.app` by an account with
Owner/Editor access to CanvasLink's OAuth project. GitHub Pages DNS configuration
alone is not that verification. Branding publication, data-access verification,
and changing audience publishing status are distinct steps.

## Sources

- [Google brand/domain verification](https://developers.google.com/identity/protocols/oauth2/production-readiness/brand-verification)
- [Sensitive-scope review and video requirements](https://developers.google.com/identity/protocols/oauth2/production-readiness/sensitive-scope-verification)
- [Calendar creation endpoint and accepted scopes](https://developers.google.com/workspace/calendar/api/v3/reference/calendars/insert)
- [Calendar scope definitions](https://developers.google.com/workspace/calendar/api/auth)
- [Testing audience limits](https://support.google.com/cloud/answer/15549945?hl=en)
