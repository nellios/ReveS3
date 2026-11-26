"""Exercise the Windows package against a loopback mock only."""
import argparse
import datetime
from contextlib import ExitStack
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid
import zipfile

import boto3
from botocore.config import Config

ROOT = Path(__file__).resolve().parents[1]
APP = 'cloudreve-s3'
TEST_EMAIL = 'launcher@example.invalid'
TEST_INPUT = str(uuid.uuid4()) + chr(0x5bc6) + chr(0x7801)


def check(name, passed):
    print(json.dumps({'check': name, 'passed': bool(passed)}), flush=True)
    if not passed:
        raise RuntimeError(name)


class Mock(BaseHTTPRequestHandler):
    logins, reject, input_ok, html_error = 0, False, False, False
    observed_uris = []

    def log_message(self, *_):
        pass

    def reply(self, data=None, code=0):
        # A rejected upstream may reflect private input; the CLI must never print it.
        body = json.dumps({'code': code, 'data': data, 'msg': TEST_INPUT if code else ''}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        Mock.logins += 1
        Mock.input_ok = data == {'email': TEST_EMAIL, 'password': TEST_INPUT}
        if Mock.html_error:
            body = ('<html>' + TEST_INPUT + '</html>').encode()
            self.send_response(502)
            self.send_header('Content-Type', 'text/html')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if Mock.reject or not Mock.input_ok:
            return self.reply(code=40020)
        future = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=1)).isoformat()
        self.reply({'token': {'access_token': 'fixture-access', 'refresh_token': 'fixture-refresh',
                             'access_expires': future, 'refresh_expires': future}})

    @staticmethod
    def folder(uri):
        return {'id': 'fixture-dir', 'name': urllib.parse.unquote(uri.rsplit('/', 1)[-1]),
                'path': uri, 'type': 1, 'size': 0, 'metadata': {},
                'created_at': '2026-11-26T00:00:00Z', 'updated_at': '2026-11-26T00:00:00Z'}

    def do_GET(self):
        parsed = urllib.parse.urlsplit(self.path)
        if self.headers.get('Authorization') != 'Bearer fixture-access':
            return self.reply(code=401)
        uri = urllib.parse.parse_qs(parsed.query)['uri'][0]
        Mock.observed_uris.append(uri)
        if parsed.path == '/api/v4/file/info':
            return self.reply(self.folder(uri))
        files = [] if uri.endswith('/test') else [self.folder(uri + '/test')]
        self.reply({'parent': self.folder(uri), 'files': files, 'pagination': {'is_cursor': True}})


def free_port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dist', type=Path, default=ROOT / 'dist',
                        help='distribution directory (matches build_release.py --output)')
    distribution = parser.parse_args().dist.resolve()
    if os.name != 'nt':
        raise SystemExit('This acceptance tool requires Windows.')
    scratch_directory = ROOT / '.research'
    scratch_directory.mkdir(exist_ok=True)
    powershell = str(Path(os.environ['SystemRoot']) / 'System32/WindowsPowerShell/v1.0/powershell.exe')
    archive_path = distribution / f'{APP}-windows-amd64.zip'
    common = {'README.txt', 'deploy/gateway.local.example.json', 'docs/running.md', 'third-party/NOTICES.txt'}
    with zipfile.ZipFile(archive_path) as archive:
        prefix = f'{APP}-windows-amd64/'
        expected = common | {f'{APP}.exe', 'gateway.json', 'start.cmd', 'login.cmd', 'Start-Gateway.ps1'}
        check('windows_archive_allowlist', set(archive.namelist()) == {prefix + p for p in expected})
        check('packaged_config_is_template', archive.read(prefix + 'gateway.json') ==
              (ROOT / 'deploy/gateway.local.example.json').read_bytes())
        for name in ('start.cmd', 'login.cmd'):
            contents = archive.read(prefix + name)
            check(name + '_crlf_and_current', b'\r\n' in contents and
                  b'\n' not in contents.replace(b'\r\n', b'') and
                  contents.replace(b'\r\n', b'\n') == (ROOT / 'deploy/windows' / name).read_bytes().replace(b'\r\n', b'\n'))
        check('packaged_script_and_docs_current', archive.read(prefix + 'Start-Gateway.ps1') ==
              (ROOT / 'deploy/windows/Start-Gateway.ps1').read_bytes() and
              archive.read(prefix + 'docs/running.md') == (ROOT / 'docs/running.md').read_bytes())
    with tarfile.open(distribution / f'{APP}-linux-amd64.tar.gz') as archive:
        prefix = f'{APP}-linux-amd64/'
        files = {m.name: m for m in archive.getmembers() if m.isfile()}
        check('linux_archive_allowlist_and_executable', set(files) == {prefix + p for p in common | {APP}} and
              files[prefix + APP].mode == 0o755)
    sums = (distribution / 'SHA256SUMS').read_text(encoding='ascii').splitlines()
    expected_archives = {f'{APP}-linux-amd64.tar.gz', f'{APP}-windows-amd64.zip'}
    entries = [line.split() for line in sums]
    check('both_archive_sha256s', {name for _, name in entries} == expected_archives and
          all(hashlib.sha256((distribution / name).read_bytes()).hexdigest() == digest for digest, name in entries))
    outputs = []
    processes = []
    server = ThreadingHTTPServer(('127.0.0.1', 0), Mock)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def stop(process):
        if process.poll() is None:
            subprocess.run(['taskkill', '/PID', str(process.pid), '/T', '/F'],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
        output, _ = process.communicate(timeout=15)
        outputs.append(output)

    try:
        with tempfile.TemporaryDirectory(prefix='launcher-', dir=scratch_directory) as temporary, ExitStack() as cleanup:
            workspace = Path(temporary)
            extracted = workspace / '\u542f\u52a8\u9a8c\u6536 space & path'
            extracted.mkdir()
            with zipfile.ZipFile(archive_path) as archive:
                archive.extractall(extracted)
            package = extracted / f'{APP}-windows-amd64'
            state = package / '.state'
            keys_path = state / 's3-credentials.json'
            session_path = state / 'session.json'
            config_path = package / 'gateway.json'
            original_config = config_path.read_bytes()
            config = json.loads(original_config)
            port = free_port()
            config.update(site=f'http://127.0.0.1:{server.server_port}', allow_http=True,
                          listen=f'127.0.0.1:{port}')
            driver = workspace / 'driver.ps1'
            driver.write_text('''function global:Read-Host {
    param([string]$Prompt, [switch]$AsSecureString)
    if ($env:LAUNCHER_NO_PROMPTS -eq '1') { throw 'Unexpected login prompt.' }
    if ($AsSecureString) { return ConvertTo-SecureString $env:LAUNCHER_TEST_INPUT -AsPlainText -Force }
    return $env:LAUNCHER_TEST_EMAIL
}
& $env:LAUNCHER_SCRIPT @args
exit $LASTEXITCODE
''', encoding='utf-8-sig')
            env = os.environ.copy()
            for key in ('CLOUDREVE_EMAIL', 'CLOUDREVE_PASSWORD', 'S3_ACCESS_KEY_ID', 'S3_SECRET_ACCESS_KEY'):
                env.pop(key, None)
            env.update(LAUNCHER_SCRIPT=str(package / 'Start-Gateway.ps1'),
                       LAUNCHER_TEST_EMAIL=TEST_EMAIL, LAUNCHER_TEST_INPUT=TEST_INPUT)

            def launch(login_only=False, no_prompts=False):
                child_env = env.copy()
                child_env['LAUNCHER_NO_PROMPTS'] = '1' if no_prompts else '0'
                command = [powershell, '-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', str(driver)]
                if login_only:
                    command.append('-LoginOnly')
                process = subprocess.Popen(command, cwd=workspace, env=child_env,
                                           stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                           stderr=subprocess.STDOUT)
                processes.append(process)
                cleanup.callback(lambda child=process: stop(child) if child.poll() is None else None)
                return process

            def finish(login_only=True, no_prompts=False):
                process = launch(login_only, no_prompts)
                output, _ = process.communicate(timeout=30)
                outputs.append(output)
                return process.returncode, output

            def save_config(value):
                config_path.write_text(json.dumps(value), encoding='utf-8')

            def start_ready():
                process = launch(no_prompts=True)
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        stop(process)
                        raise RuntimeError('gateway exited before health check; output retained privately in memory')
                    try:
                        with opener.open(f'http://127.0.0.1:{port}/healthz', timeout=1) as response:
                            if response.status == 200:
                                return process
                    except (OSError, TimeoutError):
                        time.sleep(0.1)
                raise RuntimeError('gateway did not become ready')

            code, _ = finish(no_prompts=True)
            check('template_site_stops_without_prompt_or_state', code != 0 and not state.exists())
            config_path.write_text('{', encoding='utf-8')
            code, _ = finish(no_prompts=True)
            check('invalid_config_preserved', code != 0 and config_path.read_text() == '{' and not state.exists())
            config_path.unlink()
            code, _ = finish(no_prompts=True)
            check('missing_config_copies_template_then_stops', code != 0 and config_path.read_bytes() == original_config)
            for name, invalid, message in (
                ('http_requires_explicit_opt_in', dict(config, allow_http=False), b'"allow_http": true'),
                ('http_requires_boolean_opt_in', dict(config, allow_http='true'), b'JSON boolean'),
                ('site_rejects_webpage_path', dict(config, site=config['site'] + '/home'), b'site must contain only'),
                ('site_rejects_api_path', dict(config, site=config['site'] + '/api/v4'), b'site must contain only'),
                ('site_rejects_query', dict(config, site=config['site'] + '?view=files'), b'site must contain only'),
                ('site_rejects_fragment', dict(config, site=config['site'] + '/#files'), b'site must contain only'),
            ):
                save_config(invalid)
                before_config = config_path.read_bytes()
                code, output = finish(no_prompts=True)
                check(name, code != 0 and message in output and Mock.logins == 0 and not state.exists() and
                      config_path.read_bytes() == before_config)
            no_opt_in = dict(config)
            del no_opt_in['allow_http']
            save_config(no_opt_in)
            code, output = finish(no_prompts=True)
            check('http_missing_opt_in_stops_before_prompt', code != 0 and b'"allow_http": true' in output and
                  Mock.logins == 0 and not state.exists())
            save_config(dict(config, region='invalid/region'))
            code, output = finish()
            check('native_configuration_reason_in_final_line', code != 0 and Mock.logins == 0 and
                  b'invalid region' in output.rstrip().splitlines()[-1] and
                  not keys_path.exists() and not session_path.exists())
            save_config(config)
            Mock.reject = True
            code, output = finish()
            check('failed_initial_login_stops_without_credentials', code != 0 and Mock.logins == 1 and Mock.input_ok and
                  not keys_path.exists() and not session_path.exists())
            check('login_error_code_in_final_line', b'Login failed (exit 1)' in output.rstrip().splitlines()[-1] and
                  b'HTTP 200, code 40020' in output.rstrip().splitlines()[-1])
            Mock.reject = False
            Mock.html_error = True
            code, output = finish()
            check('invalid_upstream_reason_in_final_line', code != 0 and
                  b'Cloudreve response unavailable or invalid' in output.rstrip().splitlines()[-1] and
                  not keys_path.exists() and not session_path.exists())
            Mock.html_error = False
            code, output = finish()
            if code != 0:
                diagnostic = output.decode('utf-8', errors='replace')
                for private in (TEST_INPUT, TEST_EMAIL, 'fixture-access', 'fixture-refresh'):
                    diagnostic = diagnostic.replace(private, '[redacted]')
                print(json.dumps({'launcher_error': diagnostic[-2500:]}, ensure_ascii=True))
            check('first_login_and_unicode_password', code == 0 and Mock.input_ok and keys_path.exists() and session_path.exists())
            keys_bytes, session_bytes = keys_path.read_bytes(), session_path.read_bytes()
            keys = json.loads(keys_bytes)
            check('generated_s3_secret', len(keys['secret_access_key']) == 64 and
                  len(bytes.fromhex(keys['secret_access_key'])) == 32)
            logins = Mock.logins
            save_config(dict(config, allow_http=False))
            code, output = finish(no_prompts=True)
            check('http_preflight_preserves_saved_credentials', code != 0 and b'"allow_http": true' in output and
                  Mock.logins == logins and keys_path.read_bytes() == keys_bytes and session_path.read_bytes() == session_bytes)
            save_config(config)
            gateway = start_ready()
            check('saved_session_start_without_prompt', Mock.logins == logins)
            client = boto3.client('s3', endpoint_url=f'http://127.0.0.1:{port}',
                                  aws_access_key_id=keys['access_key_id'],
                                  aws_secret_access_key=keys['secret_access_key'], region_name='us-east-1',
                                  config=Config(signature_version='s3v4', s3={'addressing_style': 'path'},
                                                proxies={}, retries={'max_attempts': 0},
                                                connect_timeout=3, read_timeout=5))
            check('signed_list_buckets', [b['Name'] for b in client.list_buckets()['Buckets']] == ['test'])
            check('signed_head_bucket', client.head_bucket(Bucket='test')['ResponseMetadata']['HTTPStatusCode'] == 200)
            check('signed_list_objects', client.list_objects_v2(Bucket='test').get('KeyCount') == 0)
            code, output = finish(no_prompts=True)
            check('duplicate_launcher_blocked', code != 0 and b'already running' in output)
            stop(gateway)
            # Select the bucket directory itself, rather than its parent.
            # Only this temporary package's config is changed; saved keys stay intact.
            single_config = dict(config, single_bucket_mode=True,
                                 buckets_root='cloudreve://my/test')
            save_config(single_config)
            Mock.observed_uris.clear()
            gateway = start_ready()
            check('single_mode_signed_list_buckets',
                  [b['Name'] for b in client.list_buckets()['Buckets']] == ['test'])
            check('single_mode_signed_head_bucket',
                  client.head_bucket(Bucket='test')['ResponseMetadata']['HTTPStatusCode'] == 200)
            check('single_mode_signed_list_objects_v1_v2',
                  client.list_objects_v2(Bucket='test').get('KeyCount') == 0 and
                  not client.list_objects(Bucket='test').get('Contents'))
            check('single_mode_uses_exact_configured_directory', Mock.observed_uris and
                  set(Mock.observed_uris) == {'cloudreve://my/test'})
            stop(gateway)
            check('single_mode_notice_and_credentials_preserved',
                  b'Bucket mode: single;' in outputs[-1] and Mock.logins == logins and
                  keys_path.read_bytes() == keys_bytes and session_path.read_bytes() == session_bytes)
            save_config(config)
            gateway = start_ready()
            check('restart_reuses_session_and_keys', Mock.logins == logins and
                  keys_path.read_bytes() == keys_bytes and session_path.read_bytes() == session_bytes)
            stop(gateway)
            Mock.reject = True
            code, _ = finish()
            check('failed_relogin_preserves_saved_files', code != 0 and keys_path.read_bytes() == keys_bytes and
                  session_path.read_bytes() == session_bytes)
            Mock.reject = False
            code, _ = finish()
            check('explicit_relogin_preserves_s3_keys', code == 0 and Mock.logins == logins + 2 and
                  keys_path.read_bytes() == keys_bytes)
            session_bytes = session_path.read_bytes()
            before = Mock.logins
            keys_path.write_text('{', encoding='utf-8')
            code, _ = finish(no_prompts=True)
            check('invalid_keys_stop_without_login_or_overwrite', code != 0 and Mock.logins == before and
                  keys_path.read_text() == '{' and session_path.read_bytes() == session_bytes)
            keys_path.write_bytes(keys_bytes)
            with socket.socket() as occupied:
                occupied.bind(('127.0.0.1', port))
                occupied.listen()
                code, _ = finish(login_only=False, no_prompts=True)
                check('native_serve_failure_exit_code', code != 0)
            # The launcher permits only a dedicated child state directory.
            invalid = dict(config, session_file='session.json')
            save_config(invalid)
            code, _ = finish(no_prompts=True)
            check('package_root_state_rejected', code != 0 and not (package / 'session.json').exists())
            save_config(config)
            env['LAUNCHER_STATE'] = str(state)
            acl_script = '''Import-Module (Join-Path $PSHOME 'Modules/Microsoft.PowerShell.Security/Microsoft.PowerShell.Security.psd1') -ErrorAction Stop
$u = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$items = @($env:LAUNCHER_STATE, (Join-Path $env:LAUNCHER_STATE 'session.json'), (Join-Path $env:LAUNCHER_STATE 's3-credentials.json'))
$valid = $true
foreach ($p in $items) {
    $acl = Get-Acl -LiteralPath $p
    $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    $sids = @($rules | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique)
    if ($sids.Count -ne 2 -or $sids -notcontains $u -or $sids -notcontains 'S-1-5-18') { $valid = $false }
    foreach ($r in $rules) { if ($r.AccessControlType -ne 'Allow' -or $r.FileSystemRights -ne 'FullControl') { $valid = $false } }
}
if (-not $valid) { exit 1 }
'''
            result = subprocess.run([powershell, '-NoProfile', '-Command', acl_script], env=env,
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15)
            outputs.append(result.stdout)
            check('state_session_and_keys_private_acl', result.returncode == 0)
            saved = b'\n'.join(p.read_bytes() for p in state.rglob('*') if p.is_file())
            logs = b'\n'.join(outputs)
            password_forms = [TEST_INPUT.encode('utf-8'), TEST_INPUT.encode('utf-16-le')]
            check('password_not_saved_or_logged', all(value not in saved + logs for value in password_forms))
            check('s3_secret_not_logged', keys['secret_access_key'].encode() not in logs)
            check('session_tokens_not_logged', b'fixture-access' not in logs and b'fixture-refresh' not in logs)
            check('working_directory_independent_with_unicode_space_ampersand_path', not (workspace / '.state').exists())
            print(json.dumps({'windows_launcher_acceptance': 'passed',
                              'binary_sha256': hashlib.sha256((package / f'{APP}.exe').read_bytes()).hexdigest(),
                              'scope': 'Windows PowerShell 5.1; loopback mock; no real account or GUI interaction'}))
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                stop(process)
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


if __name__ == '__main__':
    main()
