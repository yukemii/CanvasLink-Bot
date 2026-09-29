#!/usr/bin/env python3
"""Run one local CanvasLink bot and its encrypted Cloud SQL proxy; no cloud hosting is created."""
import fcntl
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def stop_process(process, timeout):
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def main():
    os.umask(0o077)
    for executable in ['go', 'gcloud', 'cloud-sql-proxy']:
        if not shutil.which(executable):
            print('Missing required tool: ' + executable)
            return 1
    check = subprocess.run([sys.executable, str(ROOT / 'deploy/check-launch.py'), '--local'])
    if check.returncode:
        return check.returncode
    config = dict(line.split('=', 1) for line in (ROOT / '.env').read_text().splitlines()
                  if line and not line.startswith('#') and '=' in line)
    url = urllib.parse.urlsplit(config['CANVASLINK_DATABASE_URL'])
    if (url.hostname, url.port, url.path, url.username) != ('127.0.0.1', 15432, '/canvaslink', 'canvaslink_app'):
        print('This local launcher expects the prepared CanvasLink loopback database URL.')
        return 1
    for port in [15432, 19091, 9090]:
        with socket.socket() as sock:
            try:
                sock.bind(('127.0.0.1', port))
            except OSError:
                print('Port ' + str(port) + ' is occupied. Stop the earlier local process first.')
                return 1
    state = ROOT / '.local'
    state.mkdir(mode=0o700, exist_ok=True)
    with (state / 'launcher.lock').open('w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print('A CanvasLink local launcher is already running.')
            return 1
        binary = state / 'canvaslink'
        build = subprocess.run(['go', 'build', '-tags', 'timetzdata', '-trimpath', '-o', str(binary), '.'], cwd=ROOT)
        if build.returncode:
            return build.returncode
        stopped = threading.Event()
        for signum in [signal.SIGINT, signal.SIGTERM]:
            signal.signal(signum, lambda *_: stopped.set())
        proxy = bot = None
        try:
            proxy = subprocess.Popen([
                'cloud-sql-proxy', '--gcloud-auth', '--address', '127.0.0.1', '--port', '15432',
                '--health-check', '--http-address', '127.0.0.1', '--http-port', '19091',
                'dulie-assistant:asia-southeast1:dulie-db'], cwd=ROOT)
            ready = False
            for _ in range(60):
                if stopped.is_set() or proxy.poll() is not None:
                    break
                try:
                    with urllib.request.urlopen('http://127.0.0.1:19091/startup', timeout=1) as response:
                        ready = response.status == 200
                except Exception:
                    pass
                if ready:
                    break
                stopped.wait(0.5)
            if not ready:
                print('Cloud SQL proxy did not become ready; bot was not started.')
                return 1
            env = os.environ.copy()
            env.update(config)
            # The local callback must remain private even if cloud deployment settings change.
            env['CANVASLINK_OAUTH_LISTEN_ADDR'] = '127.0.0.1:9090'
            bot = subprocess.Popen([str(binary)], cwd=ROOT, env=env)
            print('Local CanvasLink started. It runs only while this computer and launcher remain active.', flush=True)
            print('Press Ctrl+C to stop the bot and proxy together. Do not run this token elsewhere simultaneously.', flush=True)
            while not stopped.wait(0.5):
                if bot.poll() is not None:
                    return bot.returncode
                if proxy.poll() is not None:
                    print('Cloud SQL proxy stopped; stopping the bot.')
                    return 1
            return 0
        finally:
            stop_process(bot, 15)
            stop_process(proxy, 5)


if __name__ == '__main__':
    sys.exit(main())
