# Same PostgreSQL server, separate CanvasLink database

Status: database and restricted login provisioned on 29 September 2026.

| Setting          | Value                                          |
| ---------------- | ---------------------------------------------- |
| Project          | `dulie-assistant`                              |
| Instance         | `dulie-db`                                     |
| Region / engine  | `asia-southeast1` / PostgreSQL 18              |
| Connection name  | `dulie-assistant:asia-southeast1:dulie-db`     |
| Database / login | `canvaslink` / `canvaslink_app`                |
| Local proxy      | `127.0.0.1:15432`                              |
| Local secrets    | Repository `.env`, mode `0600`, ignored by Git |

The application role has no superuser, database-creation, role-creation, or RLS
bypass privileges and no inherited role memberships. Transactional DDL/read/write
and session advisory-lock checks passed. A metadata-only audit found no table
access or public-schema creation privilege for this role in Dulie's database.
The application initially created 14 `canvaslink_` tables; the webhook migration
added a receipts table, for 15 tables as of 30 September 2026. The complete Go test suite
passed with PostgreSQL integration tests enabled; no temporary test schemas
remained afterward. The temporary setup administrator was removed. Dulie's database, credentials,
and instance network settings were unchanged.

At inspection the instance had 15 connections against a configured maximum of 800. This is a point-in-time check, not a load test. Automated backups were
reported disabled; the instance setting was left unchanged and must be reviewed
before public launch.

## Local connection

Google Cloud CLI, Cloud SQL Auth Proxy, and Go are installed on this computer.
Start the local proxy in a terminal before using the `.env` connection:

```sh
./deploy/start-db-proxy.sh
```

Use `gcloud auth login` if the current account is no longer authenticated. Do not
start a second proxy on the same port. The `.env` URL connects to the local proxy,
not directly to a public database endpoint. Keep `.env` and its encryption key
backed up securely. The Telegram token has been supplied and verified for
`@CanvasLink_bot`; Google credentials are also configured. The bot now runs on
Cloud Run through a managed Cloud SQL Unix socket and dedicated service account.
The local polling bot and proxy are stopped. `deploy/run-local.py` remains a
rollback/testing option and refuses to start while a webhook is active. Do not
start a standalone proxy on the same port as the launcher. See the
[Cloud Run runbook](cloud-run.md) before changing runtime mode.

## Cloud SQL access

Authenticate the local Google Cloud CLI with an account authorized for the existing
project. First inspect the instance and list databases/users without changing it.
Google Cloud IAM access and PostgreSQL login privileges are separate: CLI login
does not by itself supply the existing database administrator password.

Prefer Cloud SQL Auth Proxy on loopback for local database access. It encrypts the
connection to Cloud SQL; the trusted local hop to the proxy can use `sslmode=disable`.
Never use that setting for a direct remote connection. Do not open broad authorized
networks or reset Dulie's existing database password to obtain access.

Cloud SQL built-in users created through the management API default to broad
`cloudsqlsuperuser` privileges unless custom database roles are supplied. Create a
restricted SQL role through an existing administrator connection, or explicitly
review/restrict the new role before giving its credentials to the application.
See [Cloud SQL role defaults](https://docs.cloud.google.com/sql/docs/postgres/users).

## Required connection information

Identify Dulie's actual PostgreSQL provider/server and an administrator connection
that supports creating databases and roles. Use the provider dashboard or a local
secret file; do not paste passwords into chat or commit them. Confirm the provider
supports additional databases on this server without a new paid project. Use a
direct administrator connection, not a transaction pooler.

Keep the Dulie and CanvasLink repositories, bot processes, Telegram tokens, Google
OAuth clients, and encryption keys independent. Do not copy Dulie's user records
or Google grants into CanvasLink.

## Inspect before provisioning

Run `database-preflight.sql` through the administrator connection. It is read-only
and lists database names, sizes, role capabilities, and aggregate connection use.
Confirm the server is the intended Dulie server and that neither proposed name
already exists. If either exists, inspect ownership and contents before proceeding;
do not overwrite, reset a password, or drop anything to make the names available.

CanvasLink currently allows up to 15 PostgreSQL connections per bot instance:
10 for normal work and 5 for session-level locks. Check capacity alongside Dulie
and provider-reserved connections. Use one polling instance per Telegram token.

## Provisioning reference (already completed; do not rerun)

These are reviewable administrator commands, not an automatically executed script.
Provider-managed Postgres may require equivalent dashboard operations or extra
role membership. `CREATE DATABASE` cannot run inside a transaction. If any command
fails, stop and inspect partial state rather than rerunning the whole sequence.

```sql
CREATE ROLE canvaslink_app NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
  NOINHERIT NOREPLICATION NOBYPASSRLS;
CREATE DATABASE canvaslink OWNER canvaslink_app TEMPLATE template0;
REVOKE ALL ON DATABASE canvaslink FROM PUBLIC;
```

Connect to **canvaslink** on that same server as the administrator and run:

```sql
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO canvaslink_app;
```

Use the provider's secret-setting interface, or psql's `\password canvaslink_app`,
to set a strong unique password without placing it in SQL files or shell history.
Then enable the new role:

```sql
ALTER ROLE canvaslink_app LOGIN;
```

The new role is the database owner because CanvasLink automatically creates and
migrates its own tables on startup. Never make it a server superuser or grant it
membership in Dulie's application role. Server administrators still retain access.
Review Dulie's existing PUBLIC grants: a new login can inherit PUBLIC access to
other databases or objects. Do not silently change Dulie's permissions; review
those separately if they expose application data. A separate database by itself
is not a guarantee of complete isolation from an existing permissive setup.

## Configure CanvasLink

Set its private `CANVASLINK_DATABASE_URL` to the **canvaslink** database using the
**canvaslink_app** login, a direct/session-mode endpoint, and verified TLS for a
remote server. Use the exact provider host, port, username format, and CA settings;
do not guess these from another provider's example. Keep Dulie's URL unchanged.

Create a separate random 32-byte `CANVASLINK_ENCRYPTION_KEY` and store a secure
backup outside the database. Generate/store it privately rather than printing it
into a conversation. Keep `CANVASLINK_INSTANCE_ID` stable for the deployment.
A local `.env` is ignored by Git; production should use the chosen host's secrets
configuration.

## Verify and release

1. Connect using the new app credentials and verify `current_database()` is
   `canvaslink` and `current_user` is `canvaslink_app`.
2. Test CREATE/INSERT/SELECT/DROP on a temporary scratch table in a transaction
   that is rolled back, plus session advisory lock/unlock on one connection.
3. Confirm Dulie's application role has no new access to CanvasLink and inspect
   CanvasLink's inherited access to Dulie. Check database backups and restore scope.
4. Initialize/test CanvasLink using a separate test Telegram token before starting
   the intended deployment. Startup creates/migrates tables; it is not read-only.
5. Test real reminders, Google sync, restart, and disconnect/reset behavior.
6. Record the actual provider, processing location, and log/backup retention in
   the privacy policy before removing the pre-launch label.

Database provisioning alone does not deploy the bot. The bot host, credentials,
HTTPS OAuth callback, Google verification, and public website publication are
separate launch steps.

References: [CREATE DATABASE](https://www.postgresql.org/docs/current/sql-createdatabase.html),
[CREATE ROLE](https://www.postgresql.org/docs/current/sql-createrole.html),
[psql password handling](https://www.postgresql.org/docs/current/app-psql.html).
