"""Real panel/authflow regression with a synthetic official login client.

Requires a fresh, dedicated catalog-fixture directory. No real client is used.
"""
import argparse
import json
import os
import socket
import subprocess
import sys
import time
import traceback
import urllib.request
from pathlib import Path

from playwright.sync_api import sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument('--runtime', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--fixture', required=True)
args = parser.parse_args()
root, out, binary = (Path(p).resolve() for p in (args.runtime, args.output, args.fixture))
if root.name != 'catalog-fixture' or any(p.drive.upper() != 'D:' for p in (root, out, binary)):
    raise SystemExit('dedicated_synthetic_runtime_required')
if root.exists() and any(root.iterdir()):
    raise SystemExit('existing_fixture_requires_inspection')
with socket.socket() as probe:
    if probe.connect_ex(('127.0.0.1', 17863)) == 0:
        raise SystemExit('fixture_port_busy')
root.mkdir(parents=True, exist_ok=True)
out.mkdir(parents=True, exist_ok=True)
temp = root/'tmp'
temp.mkdir()
env = {**os.environ, 'TEMP': str(temp), 'TMP': str(temp)}
base = 'http://127.0.0.1:17863'
report = {'synthetic': True, 'real_mfa': 'NOT TRIGGERED', 'checks': [],
          'page_errors': 0, 'console_errors': 0, 'external_requests': 0}
child = None
stage = 'start'


def main():
    global child, stage
    with (root/'server.log').open('wb') as log:
        child = subprocess.Popen(
            [str(binary), '-root', str(root), '-listen', '127.0.0.1:17863',
             '-desktop-connection', '-managed-login'], cwd=root, env=env,
            stdout=log, stderr=log, creationflags=subprocess.CREATE_NO_WINDOW)
    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True)
        try:
            page = browser.new_page(viewport={'width': 1440, 'height': 1000})
            page.set_default_timeout(18000)

            def count(key):
                report[key] += 1

            page.on('pageerror', lambda _: count('page_errors'))
            page.on('console', lambda msg: count('console_errors') if msg.type == 'error' else None)

            def only_fixture(route):
                if route.request.url.startswith(base+'/'):
                    route.continue_()
                else:
                    count('external_requests')
                    route.abort()

            page.route('**/*', only_fixture)
            deadline = time.monotonic()+12
            while time.monotonic() < deadline:
                try:
                    if page.request.get(base+'/__fixture/stats', timeout=500).status == 200:
                        break
                except Exception:
                    if child.poll() is not None:
                        raise AssertionError('fixture_exited') from None
                time.sleep(.1)
            else:
                raise AssertionError('fixture_not_ready')
            fields = dict(line.split(':', 1) for line in
                          (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
            page.goto(base+'/admin/')
            page.locator('#username').fill(fields['username'].strip())
            page.locator('#password').fill(fields['password'].strip())
            page.locator('#login-submit').click()
            page.locator('[data-workspace-agenda]').wait_for()

            def mode(value):
                assert page.request.post(base+'/__fixture/mode/'+value).status == 204

            def get(path):
                response = page.request.get(base+'/admin/api/'+path)
                assert response.status == 200
                return response.json()

            def flows():
                return get('connections/flows')['flows']

            def current(flow_id):
                return next(f for f in flows() if f['id'] == flow_id)

            def begin():
                if not page.url.endswith('#accounts'):
                    if not page.locator('#navigation').is_visible():
                        page.locator('#navigation-toggle').click()
                    page.locator('#navigation a[href="#accounts"]').click()
                page.locator('[data-action="connect-account"]').click()
                page.locator('input[name="provider"][value="doubao"]').check()
                page.locator('input[name="method"][value="qr"]').check()
                page.locator('#connection-begin').click()
                page.locator('#connection-verify').wait_for()
                return flows()[-1]

            assert not flows()
            before = len(get('workbench/accounts')['accounts'])
            stage = 'mfa'
            mode('challenge')
            challenge = begin()
            assert challenge['status'] == 'manual_action' and challenge['error_code'] == 'official_mfa_required'
            assert '安全验证' in page.locator('#dialog-body').inner_text()
            assert page.locator('#dialog-body input, #connection-qr').count() == 0
            response = page.request.post(base+'/admin/api/connections/step',
                data={'id': challenge['id'], 'action': 'verify', 'input': {'code': '000000'}},
                headers={'Origin': base, 'X-Admin-Request': '1'})
            assert response.status == 400 and current(challenge['id'])['status'] == 'manual_action'
            report['checks'].append('mfa_official_only_sensitive_input_rejected')
            page.screenshot(path=str(out/'mfa-desktop-synthetic.png'), full_page=True)
            page.keyboard.press('Escape')
            page.reload()
            page.locator('[data-connection-resume="'+challenge['id']+'"]').click()
            page.locator('#connection-verify').wait_for()
            assert '安全验证' in page.locator('#dialog-body').inner_text()
            report['checks'].append('mfa_page_reload_restores_pending_state')
            page.set_viewport_size({'width': 390, 'height': 844})
            assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
            page.screenshot(path=str(out/'mfa-mobile-synthetic.png'), full_page=True)
            page.locator('#connection-cancel').click()
            page.locator('#dialog-body').get_by_text('此次登录已取消。', exact=True).wait_for()
            assert current(challenge['id'])['status'] == 'canceled'
            report['checks'].append('mfa_cancel_confirmed')
            page.keyboard.press('Escape')
            stage = 'expiry'
            mode('client-login')
            expired = begin()
            mode('qr-expired')
            page.wait_for_function("document.getElementById('dialog-title')?.textContent.includes('已过期')")
            assert current(expired['id'])['status'] == 'expired'
            assert page.locator('#connection-verify, #connection-commit, #dialog-body input').count() == 0
            report['checks'].append('official_expiry_terminal_without_auto_refresh')
            stage = 'late_confirmation'
            mode('success')
            page.locator('#connection-recover-session').click()
            page.locator('#connection-commit').wait_for()
            recovery = flows()[-1]
            assert recovery['method'] == 'desktop' and recovery['authentication'] == 'existing_session'
            assert recovery['status'] == 'verified' and current(expired['id'])['status'] == 'expired'
            assert '没有新发起登录' in page.locator('#dialog-body').inner_text()
            assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
            page.screenshot(path=str(out/'late-login-mobile-synthetic.png'), full_page=True)
            page.locator('#connection-commit').click()
            page.wait_for_function("document.getElementById('dialog-title')?.textContent.includes('账号已接入')")
            saved = current(recovery['id'])
            assert saved['status'] == 'succeeded' and current(expired['id'])['status'] == 'expired'
            accounts = get('workbench/accounts')['accounts']
            assert len(accounts) == before+1 and any(a['id'] == saved['account_id'] for a in accounts)
            stats = page.request.get(base+'/__fixture/stats').json()
            assert stats['sms_calls'] == 0 and stats['qr_calls'] == 2 and stats['mutations'] == 0
            report['checks'].append('expired_retained_existing_session_saved_without_new_login')
            assert not report['page_errors'] and not report['console_errors'] and not report['external_requests']
            assert page.request.post(base+'/__fixture/stop').status == 204
            child.wait(timeout=8)
            assert child.returncode == 0
        finally:
            browser.close()


try:
    main()
    report['result'] = 'PASS'
except Exception as error:
    report.update(result='FAIL', stage=stage, error_type=type(error).__name__,
                  lines=[f.lineno for f in traceback.extract_tb(error.__traceback__) if f.filename == __file__])
finally:
    if child is not None and child.poll() is None:
        try:
            urllib.request.urlopen(urllib.request.Request(base+'/__fixture/stop', method='POST'), timeout=2).close()
            child.wait(timeout=8)
        except Exception:
            report.update(result='FAIL', cleanup_unconfirmed=True)
    if child is not None:
        report['server_exit_code'] = child.poll()
    (out/'managed-connection-browser.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(json.dumps(report))
sys.exit(0 if report.get('result') == 'PASS' else 1)
