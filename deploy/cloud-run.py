#!/usr/bin/env python3
"""Prepare a private Cloud Run configuration; cloud mutations require --deploy.
Activation is separate because Google redirects and a stopped local poller must
be confirmed first. No credential is placed in command arguments or printed.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
PROJECT = 'dulie-assistant'
PROJECT_NUMBER = '4523246116'
REGION = 'asia-southeast1'
SERVICE = 'canvaslink'
SQL = f'{PROJECT}:{REGION}:dulie-db'
URL = f'https://{SERVICE}-{PROJECT_NUMBER}.{REGION}.run.app'
RUNTIME = f'canvaslink-runtime@{PROJECT}.iam.gserviceaccount.com'
BUILDER = f'canvaslink-build@{PROJECT}.iam.gserviceaccount.com'
SCHEDULER = f'canvaslink-scheduler@{PROJECT}.iam.gserviceaccount.com'
PRIVATE = ROOT / '.local'


def private_json(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(data, stream, indent=2)
    os.chmod(path, 0o600)


def command(*args, parse=False):
    print('Cloud step: ' + ' '.join(args[:3]), flush=True)
    result = subprocess.run(['gcloud', *args, '--project', PROJECT, '--quiet'], cwd=ROOT, capture_output=True, text=True)
    if result.returncode:
        # Provider errors may embed values. Keep diagnostics private.
        private_json(PRIVATE / 'deploy-error.json', {'stderr': result.stderr, 'stdout': result.stdout})
        raise RuntimeError('Cloud command failed; private diagnostics: .local/deploy-error.json')
    return json.loads(result.stdout) if parse else result.stdout.strip()


def prepare():
    PRIVATE.mkdir(mode=0o700, exist_ok=True)
    source = {}
    for raw in (ROOT / '.env').read_text().splitlines():
        if raw and not raw.startswith('#') and '=' in raw:
            key, value = raw.split('=', 1)
            source[key.strip()] = value.strip().strip('"\'')
    required = ['CANVASLINK_DATABASE_URL', 'CANVASLINK_TELEGRAM_BOT_TOKEN', 'CANVASLINK_ENCRYPTION_KEY',
                'CANVASLINK_GOOGLE_CLIENT_ID', 'CANVASLINK_GOOGLE_CLIENT_SECRET']
    if any(not source.get(key) for key in required):
        raise RuntimeError('Required private credentials are missing.')
    db = urllib.parse.urlsplit(source['CANVASLINK_DATABASE_URL'])
    if db.path != '/canvaslink' or db.username != 'canvaslink_app':
        raise RuntimeError('Refusing an unexpected database or database login.')
    # Cloud Run mounts the managed Cloud SQL connector at this Unix socket.
    db_url = urllib.parse.urlunsplit(('postgres', db.netloc.rsplit('@', 1)[0] + '@localhost', db.path,
                  urllib.parse.urlencode({'host': '/cloudsql/' + SQL, 'sslmode': 'disable'}), ''))
    secret_file = PRIVATE / 'webhook-secret.json'
    secret = json.loads(secret_file.read_text())['secret'] if secret_file.exists() else secrets.token_urlsafe(32)
    private_json(secret_file, {'secret': secret})
    # Explicit allowlist: never upload unrelated environment variables.
    keys = required + ['CANVASLINK_ENCRYPTION_KEY_ID', 'CANVASLINK_PREVIOUS_ENCRYPTION_KEYS',
            'CANVASLINK_INSTANCE_ID', 'CANVASLINK_DEFAULT_TIMEZONE', 'CANVASLINK_SYNC_INTERVAL',
            'CANVASLINK_COURSE_REGEX', 'CANVASLINK_REMOVAL_GRACE_PERIOD', 'CANVASLINK_REMOVAL_MISSES']
    env = {key: source[key] for key in keys if source.get(key)}
    env.update(CANVASLINK_DATABASE_URL=db_url, CANVASLINK_RUNTIME_MODE='webhook',
        CANVASLINK_WEBHOOK_SECRET=secret, CANVASLINK_SCHEDULER_AUDIENCE=URL,
        CANVASLINK_SCHEDULER_EMAIL=SCHEDULER, CANVASLINK_OAUTH_REDIRECT_URL=URL + '/oauth/callback',
        CANVASLINK_OAUTH_LISTEN_ADDR=':8080', CANVASLINK_ALLOW_INSECURE_FEEDS='false',
        CANVASLINK_ALLOW_PRIVATE_FEEDS='false')
    private_json(PRIVATE / 'cloud-run.env.json', env)
    plan = {'project': PROJECT, 'region': REGION, 'service': SERVICE, 'url': URL,
        'callback': URL + '/oauth/callback', 'database': SQL + '/canvaslink',
        'cpu': 1, 'memory': '512Mi', 'minimum_instances': 0, 'maximum_instances': 1,
        'concurrency': 8, 'billing': 'request-based', 'request_timeout_seconds': 300,
        'scheduler': 'one authenticated job, every minute; created at activation',
        'secrets': 'private environment file uploaded at runtime; not in build context',
        'cost': 'metered; free allowances are shared and do not impose a $0 cap'}
    private_json(PRIVATE / 'cloud-run-plan.json', plan)
    print(json.dumps(plan, indent=2))
    return env


def deploy():
    command('services', 'enable', 'run.googleapis.com', 'cloudbuild.googleapis.com',
            'artifactregistry.googleapis.com', 'cloudscheduler.googleapis.com',
            'sqladmin.googleapis.com', 'iam.googleapis.com', 'logging.googleapis.com')
    accounts = command('iam', 'service-accounts', 'list', '--format=json', parse=True)
    emails = {a['email'] for a in accounts}
    for name, email in [('canvaslink-runtime', RUNTIME), ('canvaslink-build', BUILDER), ('canvaslink-scheduler', SCHEDULER)]:
        if email not in emails:
            command('iam', 'service-accounts', 'create', name, '--display-name=' + name)
    for email, role in [(RUNTIME, 'roles/cloudsql.client'), (BUILDER, 'roles/cloudbuild.builds.builder'), (BUILDER, 'roles/logging.logWriter')]:
        command('projects', 'add-iam-policy-binding', PROJECT, '--member=serviceAccount:' + email, '--role=' + role, '--condition=None')
    repos = command('artifacts', 'repositories', 'list', '--location=' + REGION, '--format=json', parse=True)
    if not any(r['name'].endswith('/canvaslink') for r in repos):
        command('artifacts', 'repositories', 'create', 'canvaslink', '--location=' + REGION, '--repository-format=docker', '--labels=app=canvaslink')
    # Exclude OAuth callback request logs before the endpoint becomes public.
    # Other project/org sinks must also be reviewed by the operator.
    exclusion = 'canvaslink-oauth-callback'
    sink = command('logging', 'sinks', 'describe', '_Default', '--format=json', parse=True)
    log_filter = 'resource.type="cloud_run_revision" AND resource.labels.service_name="canvaslink" AND httpRequest.requestUrl:"/oauth/callback"'
    flag = '--update-exclusion=' if any(x['name'] == exclusion for x in sink.get('exclusions', [])) else '--add-exclusion='
    command('logging', 'sinks', 'update', '_Default', flag + '^~^name=' + exclusion + '~filter=' + log_filter)
    revision = subprocess.check_output(['git', 'rev-parse', '--short=12', 'HEAD'], cwd=ROOT, text=True).strip()
    # A fresh tag avoids silently replacing an earlier image from the same commit.
    image = f'{REGION}-docker.pkg.dev/{PROJECT}/canvaslink/bot:{revision}-{secrets.token_hex(3)}'
    command('builds', 'submit', '.', '--region=' + REGION, '--config=deploy/cloudbuild.yaml',
        '--service-account=projects/' + PROJECT + '/serviceAccounts/' + BUILDER, '--substitutions=_IMAGE=' + image,
        '--default-buckets-behavior=regional-user-owned-bucket')
    command('run', 'deploy', SERVICE, '--region=' + REGION, '--image=' + image,
        '--service-account=' + RUNTIME, '--execution-environment=gen2', '--add-cloudsql-instances=' + SQL,
        '--env-vars-file=' + str(PRIVATE / 'cloud-run.env.json'), '--port=8080', '--cpu=1', '--memory=512Mi',
        '--min=0', '--max=1', '--concurrency=8', '--timeout=300', '--cpu-throttling',
        '--no-cpu-boost', '--no-invoker-iam-check', '--ingress=all', '--default-url', '--labels=app=canvaslink')
    print('Cloud deployment finished. Telegram and scheduled delivery are not activated.')
    print('Save the exact callback in Google Console, stop the local poller, then activate.')


def activate(env):
    # All existing local pollers must be stopped before setWebhook.
    rows = subprocess.check_output(['ps', '-axo', 'command='], text=True)
    if any(line.strip().endswith('deploy/run-local.py') for line in rows.splitlines()):
        raise RuntimeError('Stop the local launcher before activating cloud delivery.')
    with urllib.request.urlopen(URL + '/health', timeout=30) as response:
        if response.status != 200:
            raise RuntimeError('Cloud health check failed.')
    deployed = command('run', 'services', 'describe', SERVICE, '--region=' + REGION, '--format=json', parse=True)
    deployed_env = {e['name']: e.get('value', '') for e in deployed['spec']['template']['spec']['containers'][0].get('env', [])}
    if any(deployed_env.get(key) != value for key, value in env.items()):
        raise RuntimeError('Private local configuration differs from deployed revision; redeploy before activation.')
    token = env['CANVASLINK_TELEGRAM_BOT_TOKEN']
    payload = {'url': URL + '/telegram/webhook', 'secret_token': env['CANVASLINK_WEBHOOK_SECRET'],
               'max_connections': 1, 'allowed_updates': ['message', 'callback_query'], 'drop_pending_updates': False}
    req = urllib.request.Request('https://api.telegram.org/bot' + token + '/setWebhook',
          data=json.dumps(payload).encode(), headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            if not json.load(response).get('ok'):
                raise RuntimeError('Telegram rejected webhook setup.')
    except Exception:
        raise RuntimeError('Webhook setup failed; provider details withheld to protect the token.') from None
    jobs = command('scheduler', 'jobs', 'list', '--location=' + REGION, '--format=json', parse=True)
    exists = any(j['name'].endswith('/canvaslink-tick') for j in jobs)
    command('scheduler', 'jobs', 'update' if exists else 'create', 'http', 'canvaslink-tick',
        '--location=' + REGION, '--schedule=* * * * *', '--time-zone=Etc/UTC',
        '--uri=' + URL + '/internal/tick', '--http-method=POST', '--oidc-service-account-email=' + SCHEDULER,
        '--oidc-token-audience=' + URL, '--attempt-deadline=270s', '--max-retry-attempts=0')
    if exists:
        command('scheduler', 'jobs', 'resume', 'canvaslink-tick', '--location=' + REGION)
    print('Webhook enabled and scheduler resumed. Complete real Telegram/Google/reminder tests before announcing launch.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group()
    action.add_argument('--deploy', action='store_true')
    action.add_argument('--activate', action='store_true')
    parser.add_argument('--accept-metered-costs', action='store_true')
    parser.add_argument('--google-redirect-saved', action='store_true')
    args = parser.parse_args()
    if (args.deploy or args.activate) and not args.accept_metered_costs:
        parser.error('Cloud hosting is metered; explicit cost authorization is required. Default preparation is local only.')
    if args.activate and not args.google_redirect_saved:
        parser.error('Save the public Google OAuth redirect first, then pass --google-redirect-saved.')
    env = prepare()
    if args.deploy:
        deploy()
    if args.activate:
        activate(env)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Never emit arbitrary SDK/HTTP errors, which may contain credentials.
        message = str(error) if type(error) is RuntimeError else 'Check prerequisites and private deployment diagnostics.'
        print('Setup stopped: ' + message, file=sys.stderr)
        sys.exit(1)
