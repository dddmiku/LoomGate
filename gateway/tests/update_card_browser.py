"""Update card truthfulness against synthetic envelopes only.

The settings card must render the *real* reason from `status.unavailable_reason`,
the real state machine (`idle/checking/downloading/handover/failed`) and the real
signature-verification capability. It must never claim "可检查官方发布并下载已签名的
运行包" when the manager says the release repo is unreadable, and never label a
failed update as "待独立验收".

python tests/update_card_browser.py --runtime <D:/.../catalog-fixture> \\
    --output <D:/.../evidence> --fixture <D:/.../catalog-fixture.exe> [--port 17971]
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
parser.add_argument('--port', type=int, default=17971)
args = parser.parse_args()
runtime, output, binary = Path(args.runtime).resolve(), Path(args.output).resolve(), Path(args.fixture).resolve()
if runtime.name != 'catalog-fixture' or binary.name != 'catalog-fixture.exe' or any(p.drive.upper() != 'D:' for p in [runtime, output, binary]):
    raise SystemExit('synthetic_runtime_required')
output.mkdir(parents=True, exist_ok=True)
base = f'http://127.0.0.1:{args.port}'
report = {'actions': [], 'screenshots': [], 'page_errors': [], 'console_errors': [], 'external_requests': [], 'observed': []}
stage = 'start'


def done(name):
    report['actions'].append(name)


PRIVATE = {
    'ok': True,
    'status': {
        'enabled': True, 'visibility': 'private',
        'unavailable_reason': '发布仓库已转为私有，匿名读不到 Release；把仓库改回公开，或配置 update.token',
        'current': 'v1.0.0', 'commit': 'abcdef1234567890', 'state': 'idle',
        'repo': 'owner/loomgate', 'signature_verification': True,
    },
}
FAILED = {
    'ok': True,
    'status': {
        'enabled': True, 'visibility': 'public', 'unavailable_reason': '',
        'current': 'v1.0.0', 'commit': 'abcdef1234567890', 'state': 'failed',
        'repo': 'owner/loomgate', 'latest_tag': 'v1.0.1',
        'last_error': 'synthetic-network-error: dial tcp: timeout',
        'signature_verification': False, 'checked_at': '2026-11-18T00:00:00Z',
    },
}


def main():
    global stage
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
            current = {'body': PRIVATE}

            def fulfill(route):
                route.fulfill(status=200, content_type='application/json',
                              body=json.dumps(current['body'], ensure_ascii=False))

            page.route('**/admin/api/update', fulfill)

            fields = dict(line.split(':', 1) for line in (runtime / 'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
            stage = 'login'
            page.goto(base + '/admin/')
            page.locator('#username').fill(fields['username'].strip())
            page.locator('#password').fill(fields['password'].strip())
            page.locator('#login-submit').click()
            page.locator('[data-workspace-overview]').wait_for()
            done('independent_login')

            def show(envelope, name):
                current['body'] = envelope
                page.evaluate("location.hash='#overview'")
                page.locator('#view[data-load-state="ready"]').wait_for()
                page.evaluate("location.hash='#settings'")
                page.locator('#view h1').filter(has_text='工作区设置').wait_for()
                page.locator('#view[data-load-state="ready"]').wait_for()
                text = page.locator('#view').inner_text()
                report['observed'].append({'case': name, 'text_tail': text[-700:]})
                return text

            stage = 'private-repo'
            text = show(PRIVATE, 'private')
            assert '发布仓库已转为私有' in text, 'unavailable_reason 未渲染：' + text[-400:]
            assert '不可用' in text, '私有仓库不得显示成可用：' + text[-400:]
            assert '可检查官方发布并下载已签名的运行包' not in text, '不可用时仍在宣称可以检查并下载：' + text[-400:]
            assert 'owner/loomgate' in text, '发布仓库标识缺失：' + text[-400:]
            done('private_repo_reason_and_repo_rendered_not_claimed_available')

            stage = 'failed-state'
            text = show(FAILED, 'failed')
            assert '失败' in text, 'failed 状态未如实显示：' + text[-400:]
            assert 'synthetic-network-error' in text, 'last_error 未显示：' + text[-400:]
            assert '待独立验收' not in text, '失败不得显示成待独立验收：' + text[-400:]
            assert '未内置发布公钥' in text, '签名校验能力未如实显示：' + text[-400:]
            done('failed_state_and_last_error_and_signature_capability_rendered')

            stage = 'up-to-date'
            current['body'] = {
                'ok': True,
                'status': {'enabled': True, 'visibility': 'public', 'unavailable_reason': '',
                           'current': 'v1.0.0', 'commit': 'abcdef1234567890', 'state': 'idle',
                           'repo': 'owner/loomgate', 'checked_at': '2026-11-18T00:00:00Z',
                           'signature_verification': True},
            }
            page.evaluate("location.hash='#overview'")
            page.locator('#view[data-load-state="ready"]').wait_for()
            page.evaluate("location.hash='#settings'")
            page.locator('#view h1').filter(has_text='工作区设置').wait_for()
            page.locator('#view[data-load-state="ready"]').wait_for()
            text = page.locator('#view').inner_text()
            report['observed'].append({'case': 'uptodate', 'text_tail': text[-700:]})
            assert '已是最新' in text, '已检查且无更新必须显示已是最新：' + text[-400:]
            # 事实行以 dt/dd 两个元素渲染，inner_text 会换行；断言两段都出现即可。
            assert '签名校验' in text and '已启用（内置发布公钥）' in text, '签名校验启用态未显示：' + text[-400:]
            assert '可升级' not in text, '无更新时不得显示可升级：' + text[-400:]
            page.screenshot(path=str(output / 'update-card.png'), full_page=True, animations='disabled')
            report['screenshots'].append('update-card.png')
            done('uptodate_and_signature_enabled_rendered')

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
                                                     if x.filename.endswith('update_card_browser.py')])
finally:
    (output / 'update-card-browser.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
print(json.dumps({k: report[k] for k in ['result', 'failed_stage', 'error_type', 'detail'] if k in report}, ensure_ascii=False))
print('completed_actions=' + str(len(report['actions'])))
sys.exit(0 if report['result'] == 'PASS' else 1)
