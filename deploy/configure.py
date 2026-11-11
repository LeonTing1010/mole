#!/usr/bin/env python3
"""Generate both server configs and client links from a persistent private identity."""
import argparse
import base64
import ipaddress
import json
import os
import pathlib
import secrets
import subprocess
import tempfile
import urllib.parse
import uuid


def publish(path, value, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix='.mole-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, indent=2)
            stream.write('\n')
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def raw_key(value):
    data = base64.b64decode(value + '=' * (-len(value) % 4), altchars=b'-_', validate=True)
    if len(data) != 32:
        raise ValueError('REALITY key must contain 32 bytes')
    return data


def public_key(private):
    der = bytes.fromhex('302e020100300506032b656e04220420') + raw_key(private)
    out = subprocess.check_output(['openssl', 'pkey', '-inform', 'DER', '-pubout', '-outform', 'DER'], input=der, stderr=subprocess.DEVNULL)
    return base64.urlsafe_b64encode(out[-32:]).decode().rstrip('=')


def configure(root, ip):
    ipaddress.ip_address(ip)
    etc = root / 'etc'
    directory = etc / 'mole'
    directory.mkdir(parents=True, exist_ok=True)
    directory.chmod(0o755)
    path = directory / 'identity.json'
    if path.exists():
        identity = json.loads(path.read_text())
    else:
        private = os.environ.get('REALITY_PRIV')
        if not private:
            der = subprocess.check_output(['openssl', 'genpkey', '-algorithm', 'X25519', '-outform', 'DER'], stderr=subprocess.DEVNULL)
            private = base64.urlsafe_b64encode(der[-32:]).decode().rstrip('=')
        identity = {'reality_private_key': private,
                    'short_id': os.environ.get('REALITY_SHORT') or secrets.token_hex(8),
                    'uuid': os.environ.get('USER_UUID') or str(uuid.uuid4()),
                    'hy2_password': os.environ.get('HY2_PASSWORD') or secrets.token_urlsafe(24)}
    # Validate before persisting or replacing any working config.
    public = public_key(identity['reality_private_key'])
    uuid.UUID(identity['uuid'])
    sid = identity['short_id']
    if len(sid) != 16 or len(bytes.fromhex(sid)) != 8:
        raise ValueError('REALITY_SHORT must be 8 bytes of hex')
    if not identity['hy2_password']:
        raise ValueError('empty Hysteria password')
    for env, key in [('REALITY_PRIV', 'reality_private_key'), ('REALITY_SHORT', 'short_id'), ('USER_UUID', 'uuid'), ('HY2_PASSWORD', 'hy2_password')]:
        if os.environ.get(env) and os.environ[env] != identity[key]:
            raise ValueError(f'{env} conflicts with persisted identity')
    publish(path, identity)
    cert, key = directory / 'server.crt', directory / 'server.key'
    if not cert.exists() or not key.exists():
        subprocess.run(['openssl', 'req', '-x509', '-nodes', '-days', '3650', '-newkey', 'rsa:2048', '-keyout', str(key), '-out', str(cert), '-subj', '/CN=www.microsoft.com'], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.chmod(key, 0o600)
    dest = 'www.microsoft.com'
    reality = {'log': {'level': 'warn'}, 'dns': {'servers': [{'type': 'local', 'tag': 'local'}]},
               'inbounds': [{'type': 'vless', 'tag': 'reality-in', 'listen': '::', 'listen_port': 443,
                             'users': [{'uuid': identity['uuid'], 'flow': 'xtls-rprx-vision'}],
                             'multiplex': {'enabled': True},
                             'tls': {'enabled': True, 'server_name': dest,
                                     'reality': {'enabled': True, 'handshake': {'server': dest, 'server_port': 443},
                                                 'private_key': identity['reality_private_key'], 'short_id': [sid]}}}],
               'outbounds': [{'type': 'direct', 'tag': 'direct', 'domain_resolver': {'server': 'local', 'strategy': 'prefer_ipv6'}}]}
    publish(etc / 'sing-box/config.json', reality, 0o640)
    # JSON is a YAML subset; serialization also escapes user-supplied passwords safely.
    hy2 = {'listen': ':443', 'tls': {'cert': str(cert), 'key': str(key)},
           'auth': {'type': 'password', 'password': identity['hy2_password']},
           'obfs': {'type': 'salamander', 'salamander': {'password': identity['hy2_password']}}}
    publish(etc / 'hysteria/config.yaml', hy2, 0o640)
    host = f'[{ip}]' if ':' in ip else ip
    query = urllib.parse.urlencode({'encryption': 'none', 'flow': 'xtls-rprx-vision', 'security': 'reality', 'sni': dest, 'fp': 'chrome', 'pbk': public, 'sid': sid, 'type': 'tcp', 'mux': 'smux'})
    reality_uri = f"vless://{identity['uuid']}@{host}:443?{query}#reality"
    query = urllib.parse.urlencode({'sni': dest, 'insecure': '1', 'obfs': 'salamander', 'obfs-password': identity['hy2_password']})
    hy2_uri = f"hysteria2://{urllib.parse.quote(identity['hy2_password'], safe='')}@{host}:443/?{query}#hy2"
    publish(directory / 'config.json', {'server_ip': ip, 'reality_uri': reality_uri, 'hy2_uri': hy2_uri,
                                      'next_step': f"mole server import '{reality_uri}' --name mole-reality"})


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=pathlib.Path, default=pathlib.Path('/'))
    parser.add_argument('--ip', required=True, help='Public server IP used in client links')
    args = parser.parse_args()
    configure(args.root, args.ip)
