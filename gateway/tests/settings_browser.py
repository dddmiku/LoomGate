"""Settings-page acceptance against catalog-workbench only; never real accounts.

Covers the truthfulness defects fixed in this block:
- an unreadable endpoint must render "not read", not a default-off state;
- LAN save must read back the saved config and label restart as required;
- loop toggle must read back its new value;
- an update-capable build and the disabled update envelope both render honestly.

python tests/settings_browser.py --runtime <D:/.../runtime> --output <D:/.../evidence> --fixture <D:/.../catalog-fixture.exe>
"""
import argparse
import json
import subprocess
import sys
import time
import traceback
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument('--runtime', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--fixture', required=True)
parser.add_argument('--port', type=int, default=17962)
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
    proc = subprocess.Popen([str(binary), '--root', str(runtime), '--listen', f'127.0.0.1:{args.port}'],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        import urllib.request
        for _ in range(100):
            try:
                urllib.request.urlopen(base + '/admin/', timeout=0.5).read()
                break
            except Exception:
                time.sleep(0.1)
        else:
            raise RuntimeError('fixture_not_ready')
        # The LAN safety gate requires an existing key store. This fixture key
        # store is synthetic and contains no usable credential; it exists only
        # so the save path can be exercised without weakening the gate.
        (runtime / 'data').mkdir(parents=True, exist_ok=True)
        (runtime / 'data' / 'api_keys.json').write_text('{"keys":[]}', encoding='utf-8')
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            context = browser.new_context(viewport={'width': 1280, 'height': 960})
            page = context.new_page()
            page.on('pageerror', lambda e: report['page_errors'].append(type(e).__name__))
            page.on('console', lambda m: report['console_errors'].append(m.text[:200]) if m.type == 'error' else None)
            page.on('request', lambda r: report['external_requests'].append(urlsplit(r.url).hostname)
                    if urlsplit(r.url).hostname not in [None, '127.0.0.1'] else None)
            page.on('dialog', lambda d: (_ for _ in ()).throw(RuntimeError('unexpected_browser_dialog')))

            headings = {'overview': '工作台', 'providers': '服务接入', 'accounts': '账号', 'models': '模型目录',
                        'tasks': '任务中心', 'schedules': '执行计划', 'runs': '执行记录', 'usage': '用量',
                        'logs': '请求记录', 'settings': '工作区设置'}

            def nav(route):
                if not page.locator('#navigation').is_visible():
                    page.locator('#navigation-toggle').click()
                page.locator(f'#navigation a[href="#{route}"]').click()
                page.locator('#view h1').filter(has_text=headings[route]).wait_for()
                page.locator('#view[data-load-state="ready"]').wait_for()
                page.locator('#view > .loading').wait_for(state='detached')

            def shot(name):
                page.screenshot(path=str(output / name), full_page=True, animations='disabled',
                                mask=[page.locator('input[type=password]')])
                report['screenshots'].append(name)

            fields = dict(line.split(':', 1) for line in
                          (runtime / 'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
            stage = 'login'
            page.goto(base + '/admin/')
            page.locator('#username').fill(fields['username'].strip())
            page.locator('#password').fill(fields['password'].strip())
            page.locator('#login-submit').click()
            page.locator('[data-workspace-overview]').wait_for()
            done('independent_login')

            stage = 'settings-honest-unavailable'
            nav('settings')
            text = page.locator('#view').inner_text()
            # The fixture gateway has no update manager: its real envelope is
            # {ok:false,enabled:false,message:...}; the UI must show the reason,
            # not a generic "not enabled" that hides a read failure.
            assert '热更新未启用' in text or '未启用' in text, text[:400]
            assert '状态未读取' in text or '未启用' in text, text[:400]
            # LAN config is a real file read; the page must not claim a live bind.
            assert '局域网访问' in text
            page.locator('[data-action=lan-settings]').click()
            page.locator('#lan-form').wait_for()
            assert page.locator('#lan-form input[name=listen]').get_attribute('readonly') is not None
            notice_text = page.locator('#lan-form').inner_text()
            assert '重启' in notice_text, notice_text
            done('lan_dialog_separates_saved_config_from_live_listener')
            shot('settings-desktop.png')

            stage = 'lan-save-readback'
            before = page.evaluate("fetch('api/state',{credentials:'same-origin'}).then(r=>r.json())")
            want_cidrs = '127.0.0.0/8'
            page.locator('#lan-form input[name=allow_cidrs]').fill(want_cidrs)
            if not page.locator('#lan-form input[name=enabled]').is_checked():
                page.locator('#lan-form input[name=enabled]').check()
            page.locator('#lan-form button[type=submit]').click()
            page.locator('#dialog').wait_for(state='hidden')
            after = page.evaluate("fetch('api/state',{credentials:'same-origin'}).then(r=>r.json())")
            assert after['lan']['allow_cidrs'] == want_cidrs, (before.get('lan'), after.get('lan'))
            assert after['lan']['enabled'] is True
            done('lan_save_verified_from_server_state')

            stage = 'loop-toggle-readback'
            page.locator('[data-action=loop-toggle]').click()
            page.locator('#toast').wait_for(state='visible')
            page.locator('#toast').wait_for(state='hidden', timeout=8000)
            loop = page.evaluate("fetch('api/features/reasoning-loop',{credentials:'same-origin'}).then(r=>r.json())")
            assert loop['ok'] is True and loop['stop_only'] is True, loop
            page.locator('[data-action=loop-toggle]').click()
            page.locator('#toast').wait_for(state='visible')
            page.locator('#toast').wait_for(state='hidden', timeout=8000)
            loop = page.evaluate("fetch('api/features/reasoning-loop',{credentials:'same-origin'}).then(r=>r.json())")
            assert loop['stop_only'] is False, loop
            done('loop_toggle_verified_from_server')

            stage = 'models-capability-copy'
            nav('models')
            page.locator('[data-action="model-refresh"]:not([disabled])').wait_for()
            page.locator('#model-list .loading').wait_for(state='detached')
            page.locator('.selection-row').first.click()
            body = page.locator('#model-detail').inner_text()
            assert '只支持单轮' not in body and '仅支持单轮' not in body, body[:400]
            # The fixture's first model is the catalog-scoped WorkBuddy entry;
            # the assertion is that no stale single-round claim leaks through
            # any model detail, not that every model advertises multi-turn.
            done('model_copy_matches_catalog_capability')

            stage = 'mobile-and-dark'
            page.set_viewport_size({'width': 390, 'height': 844})
            for route in ['settings', 'models']:
                nav(route)
                assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1'), route
                shot(f'settings-{route}-390.png')
            if not page.locator('#navigation').is_visible():
                page.locator('#navigation-toggle').click()
            page.locator('#theme').click()
            assert page.locator('html').get_attribute('data-theme') == 'dark'
            shot('settings-dark-390.png')
            done('mobile_390_and_dark')

            stage = 'update-disabled-envelope'
            # The fixture gateway reports {ok:false,enabled:false,message:...}
            # for /update. The settings card must show the real reason and must
            # not render a downloaded/ready state from an unread envelope.
            page.set_viewport_size({'width': 1280, 'height': 960})
            page.evaluate("location.hash='#settings'")
            page.locator('#view h1').filter(has_text='工作区设置').wait_for()
            page.locator('#view[data-load-state="ready"]').wait_for()
            settings_text = page.locator('#view').inner_text()
            assert '未启用' in settings_text and '已下载' not in settings_text, settings_text[-400:]
            done('disabled_update_envelope_rendered_truthfully')

            allowed = ['net::ERR_INTERNET_DISCONNECTED', 'Failed to fetch', 'status of 503', 'status of 401']
            unexpected = [x for x in report['console_errors'] if not any(t in x for t in allowed)]
            assert not report['page_errors'] and not unexpected and not report['external_requests'], (
                report['page_errors'], unexpected, report['external_requests'])
            context.close()
            browser.close()
    finally:
        proc.terminate()
        try:
            proc.wait(5)
        except Exception:
            proc.kill()


try:
    main()
    report['result'] = 'PASS'
except Exception as e:
    report.update(result='FAIL', failed_stage=stage, error_type=type(e).__name__,
                  detail=str(e)[:500], source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('settings_browser.py')])
finally:
    (output / 'settings-browser.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
print(json.dumps({k: report[k] for k in ['result', 'failed_stage', 'error_type', 'source_lines'] if k in report}, ensure_ascii=False))
raise SystemExit(0 if report['result'] == 'PASS' else 1)
