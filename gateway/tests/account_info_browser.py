"""Legacy account information on the accounts page, against synthetic auth files.

The console used to drop three AuthSummary fields: parsable, mobile and
enterpriseId. A credential file that cannot be parsed then looked exactly like
"enabled but not loaded", and Trae's masked mobile / WorkBuddy's enterprise id
were invisible. Covers:
  - an unparsable auth file gets an explicit "凭证无法解析" state;
  - the masked mobile and enterprise id render in the account details;
  - searching by mobile finds the account.

python tests/account_info_browser.py --runtime <D:/.../catalog-fixture> \\
    --output <D:/.../evidence> --fixture <D:/.../catalog-fixture.exe> [--port 17973]
"""
import argparse
import json
import subprocess
import sys
import time
import traceback
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument('--runtime', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--fixture', required=True)
parser.add_argument('--port', type=int, default=17973)
args = parser.parse_args()
runtime, output, binary = Path(args.runtime).resolve(), Path(args.output).resolve(), Path(args.fixture).resolve()
if runtime.name != 'catalog-fixture' or binary.name != 'catalog-fixture.exe' or any(p.drive.upper() != 'D:' for p in [runtime, output, binary]):
    raise SystemExit('synthetic_runtime_required')
output.mkdir(parents=True, exist_ok=True)
base = f'http://127.0.0.1:{args.port}'
report = {'actions': [], 'screenshots': [], 'page_errors': [], 'console_errors': [], 'external_requests': []}
stage = 'start'


def done(name):
    report['actions'].append(name)


def main():
    global stage
    auths = runtime / 'auths'
    auths.mkdir(parents=True, exist_ok=True)
    # Malformed on purpose: a truncated legacy file. Never real account data.
    (auths / 'workbuddy-broken01.json').write_text('{"account":{"uid":"broken01",', encoding='utf-8')
    (auths / 'workbuddy-trae01.json').write_text(json.dumps({
        'account': {'uid': 'trae01', 'nickname': '合成 Trae 用户', 'mobile': '157****3894', 'enterpriseId': 'ent-synthetic'},
        'auth': {'accessToken': 'synthetic', 'realm': 'trae', 'domain': 'api.trae.cn', 'expiresAt': 4102444800},
    }), encoding='utf-8')
    (auths / 'workbuddy-global01.json').write_text(json.dumps({
        'account': {'uid': 'global01', 'nickname': '合成国际用户', 'enterpriseId': 'ent-global-synthetic'},
        'auth': {'accessToken': 'synthetic', 'realm': 'global', 'domain': 'www.workbuddy.ai', 'expiresAt': 4102444800},
    }), encoding='utf-8')

    proc = subprocess.Popen([str(binary), '--root', str(runtime), '--listen', f'127.0.0.1:{args.port}'],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(100):
            try:
                urllib.request.urlopen(base + '/admin/', timeout=0.5).read()
                break
            except Exception:
                time.sleep(0.1)
        else:
            raise RuntimeError('fixture_not_ready')
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            context = browser.new_context(viewport={'width': 1280, 'height': 960})
            page = context.new_page()
            page.on('pageerror', lambda e: report['page_errors'].append(type(e).__name__))
            page.on('console', lambda m: report['console_errors'].append(m.text[:180]) if m.type == 'error' else None)
            page.on('request', lambda r: report['external_requests'].append(urlsplit(r.url).hostname)
                    if urlsplit(r.url).hostname not in [None, '127.0.0.1'] else None)
            fields = dict(line.split(':', 1) for line in (runtime / 'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
            stage = 'login'
            page.goto(base + '/admin/')
            page.locator('#username').fill(fields['username'].strip())
            page.locator('#password').fill(fields['password'].strip())
            page.locator('#login-submit').click()
            page.locator('[data-workspace-agenda]').wait_for()
            done('independent_login')

            def nav(route):
                if not page.locator('#navigation').is_visible():
                    page.locator('#navigation-toggle').click()
                page.locator('#navigation a[href="#' + route + '"]').click()
                page.locator('#view[data-load-state="ready"]').wait_for()

            stage = 'unparsable-credentials'
            nav('accounts')
            page.locator('#account-provider').wait_for()
            # WorkBuddy page: the malformed file must be called out, not shown as a
            # normal enabled account that merely has not been loaded.
            page.locator('[data-provider-choice="workbuddy"]').click()
            page.locator('#account-list tbody tr').first.wait_for()
            page.locator('#account-search').fill('broken01')
            text = page.locator('#account-list').inner_text()
            assert '凭证无法解析' in text, text[:400]
            assert '无法解析' in text and '网关不会加载' in text, text[:400]
            done('unparsable_credential_is_flagged_with_reason')

            stage = 'mobile-and-enterprise'
            page.locator('[data-provider-choice="trae"]').click()
            page.locator('#account-search').fill('')
            page.locator('#account-list tbody tr').first.wait_for()
            page.locator('#account-list details.account-id summary').first.click()
            text = page.locator('#account-list').inner_text()
            assert '157****3894' in text, text[:500]
            assert 'ent-synthetic' in text, text[:500]
            done('trae_mobile_and_enterprise_rendered')

            stage = 'search-by-mobile'
            page.locator('#account-search').fill('157****3894')
            rows = page.locator('#account-list tbody tr').count()
            assert rows == 1, f'按手机号搜索应命中 1 行，实际 {rows}'
            done('account_search_matches_masked_mobile')

            stage = 'workbuddy-enterprise'
            page.locator('[data-provider-choice="workbuddy"]').click()
            page.locator('#account-search').fill('global01')
            page.locator('#account-list tbody tr').first.wait_for()
            page.locator('#account-list details.account-id summary').first.click()
            text = page.locator('#account-list').inner_text()
            assert 'ent-global-synthetic' in text, text[:500]
            done('workbuddy_enterprise_rendered')

            page.screenshot(path=str(output / 'account-info.png'), full_page=True, animations='disabled')
            report['screenshots'].append('account-info.png')
            allowed = ['net::ERR_INTERNET_DISCONNECTED', 'Failed to fetch', 'status of 503', 'status of 401']
            unexpected = [x for x in report['console_errors'] if not any(t in x for t in allowed)]
            assert not report['page_errors'] and not unexpected and not report['external_requests'], (
                report['page_errors'], unexpected, report['external_requests'])
            context.close()
            browser.close()
    finally:
        try:
            proc.terminate()
            proc.wait(timeout=10)
        except Exception:
            proc.kill()


try:
    main()
    report['result'] = 'PASS'
except Exception as e:
    report.update(result='FAIL', failed_stage=stage, error_type=type(e).__name__,
                  detail=str(e)[:600], source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__)
                                                     if x.filename.endswith('account_info_browser.py')])
finally:
    (output / 'account-info-browser.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
print(json.dumps({k: report[k] for k in ['result', 'failed_stage', 'error_type', 'detail'] if k in report}, ensure_ascii=False))
print('completed_actions=' + str(len(report['actions'])))
sys.exit(0 if report['result'] == 'PASS' else 1)
