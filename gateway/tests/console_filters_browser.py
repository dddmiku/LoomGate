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
        # Grouped by provider+realm+task key: balance = one CN row + one global
        # row, with the 3 accounts listed in each detail pane.
        assert page.locator('[data-task=balance]').count() == 2
        choose_account(page, '#task-account', '1003'); settled()
        assert requests[-1]['query'] == {'provider': ['workbuddy'], 'accounts': ['1003']}
        assert page.locator('[data-task=balance]').count() == 1
        assert page.locator('[data-task=checkin]').count() == 0
        assert page.locator('#task-account').evaluate('(e)=>e===document.activeElement')
        choose_account(page, '#task-account', ''); settled()
        assert requests[-1]['query'] == {'provider': ['workbuddy']}
        assert page.locator('[data-task=balance]').count() == 2
        # Rows are ordered by Chinese task name, so realm order is not fixed:
        # one row is the CN group (2 accounts) and the other is global (1).
        rows = page.locator('[data-task=balance]'); counts = []
        for i in range(rows.count()):
            page.locator('[data-action=task-back]').click() if page.locator('[data-action=task-back]').is_visible() else None
            rows.nth(i).click(); page.locator('#task-detail').wait_for()
            counts.append(page.locator('#task-detail .account-task-row').count())
        assert sorted(counts) == [1, 2], counts
        report['actions'].append('single_and_all_scope_reach_actual_catalog_and_change_task_rows')
        shot('filters-tasks-desktop.png')
        page.locator('#task-account').click(); shot('filters-account-desktop.png'); page.keyboard.press('Escape')

        stage = 'search-cancel'
        choose_provider(page, '#task-provider', 'doubao'); settled()
        assert requests[-1]['query'] == {'provider': ['doubao']}
        page.locator('#task-account').click()
        assert page.locator('#scope-search').evaluate('(e)=>e===document.activeElement')
        assert page.locator('#scope-accounts button').count() == 23
        assert page.locator('[data-account-choice=c-disabled]').count() == 0
        count = len(requests)
        page.locator('#scope-search').fill('Z-PAGE-20')
        assert page.locator('#scope-accounts button').count() == 1
        assert page.locator('#scope-accounts button').inner_text().startswith('z-page-20')
        page.locator('#scope-search').fill('not-an-account')
        assert page.locator('#scope-accounts button').count() == 0
        assert '没有匹配' in page.locator('#scope-accounts').inner_text()
        page.locator('.scope-footer [data-action=close]').click()
        assert len(requests) == count
        assert '全部已启用' in page.locator('#task-account').inner_text()
        page.locator('#task-account').click(); page.locator('#scope-search').fill('a-fixture')
        page.keyboard.press('Escape')
        assert page.locator('#task-account').evaluate('(e)=>e===document.activeElement')
        assert len(requests) == count
        report['actions'].append('search_empty_state_disabled_exclusion_cancel_escape_no_filter_request')

        stage = 'keyboard'
        page.locator('#task-account').press('Enter')
        page.locator('#scope-search').fill('a-fixture')
        page.keyboard.press('Tab')  # all accounts
        page.keyboard.press('Tab')  # matching account
        assert page.locator('[data-account-choice=a-fixture]').evaluate('(e)=>e===document.activeElement')
        page.keyboard.press('Enter')
        page.wait_for_function('document.querySelector("#task-account strong")?.textContent==="a-fixture"')
        settled()
        assert requests[-1]['query'] == {'provider': ['doubao'], 'accounts': ['a-fixture']}
        choose_provider(page, '#task-provider', 'workbuddy'); settled()
        assert requests[-1]['query'] == {'provider': ['workbuddy']}
        assert '全部已启用' in page.locator('#task-account').inner_text()
        assert page.locator('#task-provider [aria-pressed=true]').evaluate('(e)=>e===document.activeElement')
        report['actions'].append('keyboard_select_and_provider_change_resets_scope')

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
        nav('tasks'); settled()
        for width, height in ((1440, 1000), (1024, 768), (768, 844), (390, 844), (320, 568)):
            page.set_viewport_size({'width': width, 'height': height}); bounds()
            if width == 390: shot('filters-tasks-mobile.png')
            page.locator('#task-account').click(); bounds()
            assert page.locator('#dialog').get_attribute('class') == 'account-dialog'
            for _ in range(10):
                page.keyboard.press('Tab')
                assert page.locator('#dialog').evaluate('(d)=>d.contains(document.activeElement)')
            page.locator('#scope-search').fill('100')
            page.locator('#scope-search').focus()
            if width == 390: shot('filters-account-mobile.png')
            page.keyboard.press('Escape')
            assert page.locator('#task-account').evaluate('(e)=>e===document.activeElement')
        report['actions'].append('five_widths_no_overflow_modal_focus_containment_and_return')
        page.set_viewport_size({'width': 1440, 'height': 1000})
        page.locator('#theme').click(); page.locator('#task-account').click(); shot('filters-account-dark.png')
        page.keyboard.press('Escape'); page.locator('#theme').click()
        page.locator('[data-action=run-all]').click()
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
