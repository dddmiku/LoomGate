"""Real console filtering over the local task-modules fixture; no real accounts."""
import argparse
import json
import sys
import traceback
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import sync_playwright
from console_actions import choose_account, choose_provider, choose_capability

parser = argparse.ArgumentParser()
parser.add_argument('--runtime', required=True)
parser.add_argument('--output', required=True)
args = parser.parse_args()
root, out = Path(args.runtime).resolve(), Path(args.output).resolve()
if root.name != 'catalog-fixture' or root.drive.upper() != 'D:' or out.drive.upper() != 'D:':
    raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True, exist_ok=True)
base = 'http://127.0.0.1:17863'
report = {'actions': [], 'screenshots': [], 'page_errors': [], 'console_errors': [], 'external_requests': []}
stage = 'start'


def main():
    global stage
    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True)
        page = browser.new_page(viewport={'width': 1440, 'height': 1000})
        page.on('pageerror', lambda e: report['page_errors'].append(type(e).__name__))
        page.on('console', lambda m: report['console_errors'].append('console_error') if m.type == 'error' else None)
        requests = []
        def observe(request):
            url = urlsplit(request.url)
            if url.hostname not in (None, '127.0.0.1'):
                report['external_requests'].append('external')
            if url.path.endswith('/workbench/catalog') or url.path.endswith('/workbench/models'):
                requests.append({'path': url.path, 'query': parse_qs(url.query)})
        page.on('request', observe)
        def nav(route):
            if not page.locator('#navigation').is_visible():
                page.locator('#navigation-toggle').click()
            page.locator('#navigation a[href="#' + route + '"]').click()
            page.locator('#view > .loading').wait_for(state='detached')
        def settled():
            if page.url.endswith('#tasks'):
                page.locator('#task-list.loading').wait_for(state='detached')
            if page.url.endswith('#models'):
                page.locator('[data-action=model-refresh]:not([disabled])').wait_for()
                page.locator('#model-list .loading').wait_for(state='detached')
        def shot(name):
            page.screenshot(path=str(out/name), full_page=False, animations='disabled')
            report['screenshots'].append(name)
        def bounds():
            assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1'), 'page_overflow'
            if page.locator('#dialog').is_visible():
                assert page.locator('#dialog').evaluate('(d)=>d.scrollWidth<=d.clientWidth+1'), 'dialog_overflow'
                box = page.locator('#dialog').bounding_box()
                assert box['x'] >= 0 and box['y'] >= 0
                assert box['x'] + box['width'] <= page.viewport_size['width'] + 1
                assert box['y'] + box['height'] <= page.viewport_size['height'] + 1
        assert page.request.get(base+'/__fixture/task-stats').status == 200
        initial = page.request.get(base+'/__fixture/task-stats').json()
        fields = dict(line.split(':', 1) for line in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
        page.goto(base+'/admin/')
        page.locator('#username').fill(fields['username'].strip())
        page.locator('#password').fill(fields['password'].strip())
        page.locator('#login-submit').click()
        page.locator('[data-workspace-agenda]').wait_for()

        stage = 'task-scope'
        nav('tasks'); settled()
        assert page.locator('.scope-toolbar select,.scope-toolbar details').count() == 0
        assert page.locator('#task-provider [aria-pressed=true]').inner_text() == 'WorkBuddy'
        # 任务页现在以随包计划为行：没有账号范围选择器，目录请求也不带 accounts。
        assert page.locator('#task-account').count() == 0
        plans = page.locator('[data-plan-row]')
        plans.first.wait_for()
        assert plans.count() == 11, plans.count()
        assert page.locator('[data-task=balance]').count() == 1
        assert page.locator('#task-list.loading').count() == 0
        report['actions'].append('task_page_rows_come_from_shipped_plans_without_account_scope')
        shot('filters-tasks-desktop.png')

        stage = 'search-cancel'
        choose_provider(page, '#task-provider', 'doubao'); settled()
        assert requests[-1]['query'] == {'provider': ['doubao']}
        # 任务页已经没有任何账号范围选择器：账号范围由「运行」时目录返回的
        # 可执行账号决定，页面不维护第二份选中状态。
        assert page.locator('#task-account').count() == 0
        assert page.locator('#scope-search').count() == 0
        report['actions'].append('task_page_has_no_account_scope_picker')

        stage = 'keyboard'
        # 键盘只切服务；切回 WorkBuddy 后加载的仍是随包计划。
        choose_provider(page, '#task-provider', 'workbuddy'); settled()
        assert requests[-1]['query'] == {'provider': ['workbuddy']}
        assert page.locator('#task-provider [aria-pressed=true]').evaluate('(e)=>e===document.activeElement')
        page.locator('[data-plan-row]').first.wait_for()
        report['actions'].append('keyboard_provider_change_keeps_task_plans')

        stage = 'model-and-account-pages'
        nav('models'); choose_provider(page, '#model-provider', 'doubao'); settled()
        choose_account(page, '#model-account', 'c-disabled'); settled()
        assert requests[-1]['query']['account'] == ['c-disabled']
        assert page.locator('[data-account-status=disabled]').count() == 1
        choose_account(page, '#model-account', 'a-fixture'); settled()
        assert requests[-1]['query']['offset'] == ['0']
        choose_capability(page, 'supports_tool_call')
        assert page.locator('#model-list .selection-row').count() == 0
        choose_capability(page, 'native_tools')
        assert page.locator('#model-list .selection-row').count() == 1
        choose_provider(page, '#model-provider', 'trae'); settled()
        assert page.locator('#model-account').is_disabled()
        nav('accounts'); choose_provider(page, '#account-provider', 'doubao')
        page.locator('#account-list.loading').wait_for(state='detached')
        page.locator('#account-search').fill('c-disabled')
        assert page.locator('#account-list tbody tr').count() == 1
        page.locator('[data-action=connect-account]').click()
        assert page.locator('#connection-provider input:checked').get_attribute('value') == 'doubao'
        page.keyboard.press('Escape')
        report['actions'].append('model_scope_disabled_catalog_capabilities_and_connect_default')

        stage = 'responsive-and-modal-focus'
        # 账号范围对话框现在只存在于模型页（按账号取目录）；任务页不再有
        # 「当前选中账号」这种状态。这里锁的是同一个复用组件的焦点约束。
        nav('models'); choose_provider(page, '#model-provider', 'doubao'); settled()
        for width, height in ((1440, 1000), (1024, 768), (768, 844), (390, 844), (320, 568)):
            page.set_viewport_size({'width': width, 'height': height}); bounds()
            page.locator('#model-account:not([disabled])').click(); bounds()
            assert page.locator('#dialog').get_attribute('class') == 'account-dialog'
            for _ in range(10):
                page.keyboard.press('Tab')
                assert page.locator('#dialog').evaluate('(d)=>d.contains(document.activeElement)')
            page.locator('#scope-search').fill('a-fixture')
            page.locator('#scope-search').focus()
            page.keyboard.press('Escape')
            assert page.locator('#model-account').evaluate('(e)=>e===document.activeElement')
        report['actions'].append('five_widths_no_overflow_modal_focus_containment_and_return')
        page.set_viewport_size({'width': 1440, 'height': 1000})
        page.locator('#theme').click(); page.locator('#model-account:not([disabled])').click(); shot('filters-account-dark.png')
        page.keyboard.press('Escape'); page.locator('#theme').click()
        # 运行确认框仍使用普通 dialog 样式，关闭后样式复原。
        nav('tasks'); page.locator('[data-plan-row] [data-action=run-plan]').first.click()
        assert page.locator('#dialog').get_attribute('class') == ''
        assert page.locator('#run-form').is_visible()
        page.keyboard.press('Escape')
        report['actions'].append('drawer_theme_and_other_dialog_style_restored')
        final = page.request.get(base+'/__fixture/task-stats').json()
        assert final['signins'] == initial['signins'] and final['claims'] == initial['claims']
        assert page.request.get(base+'/admin/api/workbench').json()['runs'] == []
        assert not report['page_errors'] and not report['console_errors'] and not report['external_requests']
        browser.close()


try:
    main()
    report['result'] = 'PASS'
except Exception as error:
    report.update(result='FAIL', stage=stage, error_type=type(error).__name__,
                  lines=[x.lineno for x in traceback.extract_tb(error.__traceback__) if x.filename.endswith('console_filters_browser.py')])
(out/'console-filters-browser.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
print(json.dumps(report, ensure_ascii=False))
sys.exit(report['result'] != 'PASS')
