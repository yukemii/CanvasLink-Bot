#!/usr/bin/env python3
"""Read local launch configuration without printing secrets or starting the bot."""
import argparse
import base64
import json
import os
from pathlib import Path
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--env-file', type=Path, default=Path(__file__).resolve().parents[1] / '.env')
    parser.add_argument('--local', action='store_true', help='Check Telegram-only local testing; Google credentials are optional')
    parser.add_argument('--check-telegram', action='store_true', help='Read-only getMe/getWebhookInfo checks; no messages or polling')
    args = parser.parse_args()
    if not args.env_file.is_file():
        print('BLOCKED: private environment file is missing.')
        return 1
    config = {}
    for raw in args.env_file.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith('#'):
            continue
        if '=' not in line:
            print('BLOCKED: invalid environment-file line (contents withheld).')
            return 1
        key, value = line.split('=', 1)
        config[key.strip()] = value.strip().strip('\"\'')
    problems = 0

    def result(ok, message):
        nonlocal problems
        print(('OK: ' if ok else 'BLOCKED: ') + message)
        problems += not ok

    result(not stat.S_IMODE(args.env_file.stat().st_mode) & 0o077,
           'private environment file permissions')
    for key in ['CANVASLINK_DATABASE_URL', 'CANVASLINK_ENCRYPTION_KEY', 'CANVASLINK_TELEGRAM_BOT_TOKEN']:
        result(bool(config.get(key)), key + (' is set' if config.get(key) else ' is missing'))
    try:
        key = base64.b64decode(config.get('CANVASLINK_ENCRYPTION_KEY', ''), validate=True)
        result(len(key) == 32, 'encryption key is 32 bytes')
    except (ValueError, TypeError):
        result(False, 'encryption key format is invalid')
    google_id = bool(config.get('CANVASLINK_GOOGLE_CLIENT_ID'))
    google_secret = bool(config.get('CANVASLINK_GOOGLE_CLIENT_SECRET'))
    result(google_id == google_secret, 'Google client ID and secret are both set or both absent')
    google = google_id and google_secret
    if not args.local:
        result(google, 'Google OAuth credentials are configured' if google else 'Google OAuth credentials are missing (required for Calendar launch)')
    elif not google:
        print('NOTE: Google Calendar is disabled until OAuth credentials are supplied.')
    callback = urllib.parse.urlsplit(config.get('CANVASLINK_OAUTH_REDIRECT_URL', ''))
    if not args.local:
        valid_callback = callback.scheme == 'https' and bool(callback.hostname) and callback.path not in ('', '/')
        result(valid_callback, 'production HTTPS OAuth callback is configured' if valid_callback else 'production HTTPS OAuth callback is missing')
    for flag in ['CANVASLINK_ALLOW_INSECURE_FEEDS', 'CANVASLINK_ALLOW_PRIVATE_FEEDS']:
        result(config.get(flag, 'false').lower() in ('false', '0'), flag + ' is disabled')
    if args.check_telegram and config.get('CANVASLINK_TELEGRAM_BOT_TOKEN'):
        token = config['CANVASLINK_TELEGRAM_BOT_TOKEN']
        for method in ['getMe', 'getWebhookInfo']:
            try:
                req = urllib.request.Request('https://api.telegram.org/bot' + token + '/' + method, method='POST')
                with urllib.request.urlopen(req, timeout=15) as response:
                    body = json.load(response)
                if not body.get('ok'):
                    result(False, 'Telegram ' + method + ' failed (response details withheld)')
                elif method == 'getMe':
                    username = body['result'].get('username', '')
                    result(username.lower() == 'canvaslink_bot', 'Telegram token belongs to @CanvasLink_bot')
                else:
                    result(not body['result'].get('url'), 'Telegram webhook is absent, as required by polling mode')
            except Exception:
                # Exception strings can contain Telegram token-bearing request URLs.
                result(False, 'Telegram ' + method + ' could not be verified (details withheld)')
    print('This check does not deploy the bot, verify Google approval, or test live delivery.')
    return 1 if problems else 0


if __name__ == '__main__':
    sys.exit(main())
