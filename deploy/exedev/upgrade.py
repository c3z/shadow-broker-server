#!/usr/bin/env python3
"""Swap the running `shadow-broker` container for a new image, keeping a rollback.

Runs ON the broker VM (needs docker; run with sudo if your user is not in the
docker group). Steps:

  1. Inspect the running container; refuse to touch anything unexpected
     (port binding other than 127.0.0.1:8080, storage other than the
     `broker-data` volume).
  2. Stop it, archive the encrypted SQLite volume (consistent, container stopped).
  3. Rename it to `shadow-broker-before-<stamp>` (the rollback container).
  4. Start the new image with the SAME env (secrets never leave the VM) and
     the same volume; BIND_HOST/TRUST_PROXY come from the image.
  5. Wait for GET /health == 200. On failure: remove the new container,
     rename the rollback back and start it again.

Usage:  upgrade.py --image shadow-broker:<tag> [--keep 2]

--keep N  how many older rollback containers to retain (default 2). Older
          `shadow-broker-before-*` containers are removed, images are not.

Manual rollback:
  docker stop shadow-broker && docker rm shadow-broker
  docker rename shadow-broker-before-<stamp> shadow-broker && docker start shadow-broker
"""
import argparse
import datetime
import json
import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

NAME = 'shadow-broker'
HEALTH = 'http://127.0.0.1:8080/health'
BACKUPS = Path(os.environ.get('SHADOW_BROKER_BACKUPS', str(Path.home() / '.local/share/shadow-broker-backups')))


def docker(*args, capture=False, check=True):
    return subprocess.run(['docker', *args], check=check, capture_output=capture, text=True)


def healthy(timeout_s=20):
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(HEALTH, timeout=2) as r:
                if r.status == 200:
                    return True
        except Exception:
            pass
        time.sleep(0.5)
    return False


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--image', required=True)
    ap.add_argument('--keep', type=int, default=2)
    a = ap.parse_args()

    if docker('image', 'inspect', a.image, capture=True, check=False).returncode != 0:
        sys.exit(f'image {a.image} not found on this host')
    old = json.loads(docker('inspect', NAME, capture=True).stdout)[0]
    if old['HostConfig']['PortBindings'] != {'8080/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '8080'}]}:
        sys.exit('Unexpected port bindings; stopped before mutation.')
    m = old['Mounts']
    if len(m) != 1 or m[0]['Type'] != 'volume' or m[0]['Name'] != 'broker-data' or m[0]['Destination'] != '/data':
        sys.exit('Unexpected storage; stopped before mutation.')

    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = BACKUPS / stamp
    backup.mkdir(parents=True, mode=0o700)
    os.chmod(BACKUPS, 0o700)
    rollback = f'{NAME}-before-{stamp.lower()}'

    env = os.environ.copy()
    run = ['run', '-d', '--name', NAME, '--restart', 'unless-stopped',
           '-p', '127.0.0.1:8080:8080', '-v', 'broker-data:/data']
    for item in old['Config']['Env']:
        key, value = item.split('=', 1)
        if key in ('PATH', 'BIND_HOST', 'TRUST_PROXY'):
            continue  # image-provided
        env[key] = value
        run += ['-e', key]  # value taken from env, never from argv
    run.append(a.image)

    print(f'stopping {NAME} ({old["Config"]["Image"]})')
    docker('stop', NAME, capture=True)
    try:
        docker('run', '--rm', '--user', '0', '--entrypoint', 'tar',
               '-v', 'broker-data:/data:ro', '-v', f'{backup}:/backup', old['Image'],
               '-czf', '/backup/broker-data.tgz', '-C', '/data', '.', capture=True)
        docker('rename', NAME, rollback)
        subprocess.run(['docker', *run], env=env, check=True, capture_output=True, text=True)
        if not healthy():
            raise RuntimeError('new container did not become healthy')
        (backup / 'rollback.json').write_text(json.dumps({'container': rollback, 'image': old['Config']['Image'], 'new_image': a.image}) + '\n')
        print(f'OK  {NAME} now runs {a.image}')
        print(f'    rollback container: {rollback}')
        print(f'    encrypted data backup: {backup / "broker-data.tgz"}')
    except Exception as e:
        print(f'FAILED: {e}', file=sys.stderr)
        if docker('inspect', rollback, capture=True, check=False).returncode == 0:
            docker('rm', '-f', NAME, capture=True, check=False)
            docker('rename', rollback, NAME)
        docker('start', NAME, capture=True)
        sys.exit('Upgrade failed; previous broker restarted.')

    # prune old rollback containers beyond --keep
    names = docker('ps', '-a', '--filter', f'name=^{NAME}-before-', '--format', '{{.Names}}', capture=True).stdout.split()
    for n in sorted(names)[:-a.keep] if a.keep >= 0 else []:
        docker('rm', n, capture=True, check=False)
        print(f'    removed old rollback container {n}')


if __name__ == '__main__':
    main()
