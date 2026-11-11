"""Safety/quality, what-x-what: shipped deployment paths must serve usable identities."""
import ast
import io
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class DeploymentContract(unittest.TestCase):
    # Why: Docker and CLI must expose exactly the same keeper protocol and bytes.
    def test_keeper_bulk_delivers_binary_payload(self):
        tree = ast.parse((ROOT / 'deploy/brutal-keeper.py').read_text())
        nodes = [n for n in tree.body if isinstance(n, (ast.Import, ast.ImportFrom, ast.FunctionDef, ast.ClassDef))]
        ns = {}
        exec(compile(ast.Module(body=nodes, type_ignores=[]), 'keeper', 'exec'), ns)
        cls = ns.get('H', ns.get('Handler'))
        h = object.__new__(cls)
        h.path = '/bulk?bytes=12345'
        h.wfile = io.BytesIO()
        h.send_response = lambda code: self.assertEqual(code, 200)
        headers = {}
        h.send_header = lambda k, v: headers.update({k: v})
        h.end_headers = lambda: None
        ns['BLOCK'] = b'x' * (1 << 20)
        h.do_GET()
        self.assertEqual(len(h.wfile.getvalue()), 12345)
        self.assertEqual(headers['Content-Type'], 'application/octet-stream')

    # Why: credentials must survive restart and links must match their serving keypair.
    def test_generated_identity_and_config_are_usable_and_persistent(self):
        script = ROOT / 'deploy/configure.py'
        self.assertTrue(script.exists(), 'missing shared deployment configurator')
        with tempfile.TemporaryDirectory(prefix='mole-config-test-') as root:
            env = {k: v for k, v in os.environ.items() if k not in ('USER_UUID', 'REALITY_PRIV', 'REALITY_SHORT', 'HY2_PASSWORD')}
            subprocess.run(['python3', str(script), '--root', root, '--ip', '192.0.2.4'], env=env, check=True, capture_output=True, preexec_fn=lambda: os.umask(0o077))
            etc = pathlib.Path(root) / 'etc'
            self.assertEqual((etc / 'mole').stat().st_mode & 0o777, 0o755, 'service accounts must traverse certificate directory')
            identity = (etc / 'mole/identity.json').read_bytes()
            public = json.loads((etc / 'mole/config.json').read_text())
            cfg = json.loads((etc / 'sing-box/config.json').read_text())
            self.assertNotIn('${', json.dumps(cfg))
            self.assertEqual([i['type'] for i in cfg['inbounds']], ['vless'])
            private = json.loads(identity)['reality_private_key']
            self.assertNotIn(private, json.dumps(public))
            self.assertIn('pbk=', public['reality_uri'])
            self.assertIn('obfs=salamander', public['hy2_uri'])
            self.assertIn('obfs', json.loads((etc / 'hysteria/config.yaml').read_text()))
            self.assertEqual((etc / 'mole/config.json').stat().st_mode & 0o777, 0o600)
            if shutil.which('sing-box'):
                subprocess.run(['sing-box', 'check', '-c', str(etc / 'sing-box/config.json')], check=True, capture_output=True)
            subprocess.run(['python3', str(script), '--root', root, '--ip', '192.0.2.4'], env=env, check=True, capture_output=True)
            self.assertEqual((etc / 'mole/identity.json').read_bytes(), identity)

    # Why: hand-maintained copies of configs/keeper caused independent deployment failures.
    def test_cloud_init_is_generated_from_shared_assets(self):
        generator = ROOT / 'scripts/render-cloud-init.py'
        self.assertTrue(generator.exists(), 'missing reproducible cloud-init generator')
        generated = subprocess.check_output(['python3', str(generator)], text=True)
        self.assertEqual(generated, (ROOT / 'deploy/cloud-init.yaml').read_text())
        self.assertTrue(generated.startswith('#cloud-config\n'))
        cloud = json.loads(generated.split('\n', 1)[1])
        files = {x['path']: x for x in cloud['write_files']}
        self.assertEqual(files['/usr/local/bin/mole-brutal-keeper']['content'], (ROOT / 'deploy/brutal-keeper.py').read_text())
        self.assertEqual(files['/usr/local/bin/mole-configure']['content'], (ROOT / 'deploy/configure.py').read_text())
        self.assertNotIn('|| true', cloud['runcmd'][-1])

    # Quality/what: a failed installer download must return failure, never empty-shell success.
    def test_install_redirect_propagates_download_failure(self):
        with tempfile.TemporaryDirectory(prefix='mole-installer-test-') as root:
            curl = pathlib.Path(root) / 'curl'
            curl.write_text('#!/bin/sh\nexit 22\n')
            curl.chmod(0o755)
            env = dict(os.environ, PATH=root + os.pathsep + os.environ['PATH'])
            result = subprocess.run(['bash', str(ROOT / 'docs/install.sh')], env=env, capture_output=True)
            self.assertNotEqual(result.returncode, 0)

    # Quality/what-x-what: the entrypoint must return child failure and reap siblings.
    def test_entrypoint_reaps_children_on_failure_and_sigterm(self):
        import signal
        import time
        for failure in (True, False):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory(prefix='mole-entrypoint-test-') as root:
                directory = pathlib.Path(root)
                configure = directory / 'configure.py'
                configure.write_text('')
                worker = directory / 'worker.py'
                worker.write_text("import os,pathlib,signal,sys,time\npathlib.Path(sys.argv[1]).write_text(str(os.getpid()))\nsignal.signal(signal.SIGTERM,lambda *a:sys.exit(0))\ntime.sleep(0.3 if sys.argv[2]=='fail' else 30)\nsys.exit(7 if sys.argv[2]=='fail' else 0)\n")
                keeper = directory / 'keeper.py'
                keeper.write_text(worker.read_text().replace('sys.argv[1]', repr(str(directory / 'keeper.pid'))).replace('sys.argv[2]', repr('hold')))
                for name in ('sing-box', 'hysteria'):
                    executable = directory / name
                    mode = 'fail' if failure and name == 'hysteria' else 'hold'
                    executable.write_text('#!/bin/sh\n[ "$1" = check ] && exit 0\nexec python3 ' + str(worker) + ' ' + str(directory / (name + '.pid')) + ' ' + mode + '\n')
                    executable.chmod(0o755)
                entry = directory / 'entrypoint.sh'
                source = (ROOT / 'deploy/entrypoint.sh').read_text()
                source = source.replace('/usr/local/bin/mole-configure', str(configure)).replace('/usr/local/bin/mole-brutal-keeper', str(keeper)).replace('/usr/local/bin/hysteria', str(directory / 'hysteria'))
                entry.write_text(source)
                env = dict(os.environ, MOLE_SERVER_IP='192.0.2.4', PATH=root + os.pathsep + os.environ['PATH'])
                proc = subprocess.Popen(['bash', str(entry)], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
                try:
                    deadline = time.monotonic() + 5
                    while len(list(directory.glob('*.pid'))) < 3 and time.monotonic() < deadline:
                        time.sleep(0.02)
                    self.assertEqual(len(list(directory.glob('*.pid'))), 3)
                    if not failure:
                        proc.send_signal(signal.SIGTERM)
                    _, err = proc.communicate(timeout=10)
                    self.assertEqual(proc.returncode, 7 if failure else 143, err.decode())
                    for pidfile in directory.glob('*.pid'):
                        with self.assertRaises(ProcessLookupError):
                            os.kill(int(pidfile.read_text()), 0)
                finally:
                    if proc.poll() is None:
                        proc.terminate()
                    proc.communicate(timeout=10)


if __name__ == '__main__':
    unittest.main()
