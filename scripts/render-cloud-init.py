#!/usr/bin/env python3
"""Render a reproducible cloud-config from the same assets shipped by Docker/CLI."""
import json
import pathlib

ROOT = pathlib.Path(__file__).resolve().parents[1]
ASSETS = {'configure.py': '/usr/local/bin/mole-configure',
          'brutal-keeper.py': '/usr/local/bin/mole-brutal-keeper',
          'install-functions.sh': '/usr/local/lib/mole-install-functions.sh',
          'install-brutal.sh': '/usr/local/bin/mole-brutal-install',
          'bootstrap.sh': '/usr/local/bin/mole-bootstrap'}


def render():
    files = [{'path': dest, 'permissions': '0755', 'content': (ROOT / 'deploy' / source).read_text()}
             for source, dest in ASSETS.items()]
    return '#cloud-config\n' + json.dumps({'write_files': files, 'runcmd': ['/usr/local/bin/mole-bootstrap']}, indent=2) + '\n'


if __name__ == '__main__':
    print(render(), end='')
