-- Read-only checks. Run on the intended PostgreSQL server as its administrator.
-- No credentials, tokens, table rows, or connection strings are selected.
SELECT current_database() AS connected_database,
       current_user AS connected_role,
       current_setting('server_version') AS postgres_version,
       current_setting('max_connections')::integer AS max_connections;

SELECT rolname, rolsuper, rolcreatedb, rolcreaterole
FROM pg_roles WHERE rolname = current_user;

SELECT datname, pg_get_userbyid(datdba) AS owner,
       pg_size_pretty(pg_database_size(oid)) AS size
FROM pg_database WHERE NOT datistemplate ORDER BY datname;

SELECT count(*) AS existing_connections FROM pg_stat_activity;

SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'canvaslink')
         AS canvaslink_database_already_exists,
       EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'canvaslink_app')
         AS canvaslink_role_already_exists;
