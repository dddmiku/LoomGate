"""Click the real usage filters over synthetic loopback API responses only.

python -I -B tests/usage_filters_browser.py --output D:/LoomGate-work/evidence/20261120-usage-filter
"""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import sys
import threading
from urllib.parse import urlsplit

from playwright.sync_api import expect, sync_playwright

sys.path.insert(0, str(Path(__file__).resolve().parent))
from console_interactions_browser import Fixture, answer, assets_server


def totals(requests, prompt, completion, credit=0):
    return dict(requests=requests, prompt_tokens=prompt, completion_tokens=completion,
                total_tokens=prompt+completion, cached_tokens=0, credit=credit,
                failed_requests=0, unreported_requests=0)


def snapshot():
    first, second = totals(2, 180, 20, 1), totals(3, 270, 30, 2)
    model = lambda name, t: dict(model=name, totals=t)
    return dict(ok=True, since='2026-11-01T00:00:00Z', totals=totals(25, 2250, 250, 20),
                days=[dict(day='2026-11-19', totals=first), dict(day='2026-11-20', totals=second)],
                keys=[
                    dict(key_id='fixture-active', name='当前调用方', totals=totals(10, 900, 100, 10),
                         days=[dict(day='2026-11-19', totals=first), dict(day='2026-11-20', totals=second)],
                         models=[model('fixture:lifetime-model', totals(10, 900, 100, 10))],
                         day_models=[dict(day='2026-11-19', models=[model('fixture:daily-model', first)]),
                                     dict(day='2026-11-20', models=[model('fixture:daily-model', second)])]),
                    dict(key_id='fixture-old', name='历史调用方', totals=totals(15, 1350, 150, 10),
                         days=[dict(day='2026-11-01', totals=totals(15, 1350, 150, 10))],
                         models=[model('fixture:old-model', totals(15, 1350, 150, 10))],
                         day_models=[dict(day='2026-11-01', models=[model('fixture:old-model', totals(15, 1350, 150, 10))])]),
                ])


class UsageFixture(Fixture):
    def __init__(self, browser, base):
        super().__init__(browser, base)
        self.usage_reads = 0
        self.usage_payload = snapshot()
        self.usage_error = None

    def route(self, route):
        if urlsplit(route.request.url).path == '/api/usage':
            self.usage_reads += 1
            key = ('GET', '/api/usage')
            if self.holds.get(key):
                self.pending[key].append(route)
            elif self.usage_error:
                answer(route, {'ok': False, 'error': 'unauthorized' if self.usage_error == 401 else 'fixture_unavailable'}, self.usage_error)
            else:
                answer(route, self.usage_payload)
        else:
            super().route(route)


class ShanghaiBrowser:
    def __init__(self, browser):
        self.browser = browser

    def new_context(self, **kwargs):
        return self.browser.new_context(timezone_id='Asia/Shanghai', **kwargs)


def exercise(browser, base, output):
    f = UsageFixture(ShanghaiBrowser(browser), base)
    observed = []
    try:
        page = f.page
        page.clock.install(time=datetime(2026, 11, 20, 12, 0, tzinfo=timezone.utc))
        page.goto(base+'/#usage')

        def tile(label):
            return page.locator('article.usage-tile').filter(has=page.locator('span', has_text=label)).locator('strong')

        def card(label):
            return page.locator('section.usage-card').filter(has=page.get_by_role('heading', name=label, exact=True))

        def check(label, requests, callers, tokens, model_name, model_tokens, caller_requests, caller_tokens):
            expect(page.locator('#view[data-load-state=ready]')).to_be_visible()
            expect(tile('总请求')).to_have_text(str(requests))
            expect(tile('总 Token')).to_have_text(f'{tokens:,}')
            expect(tile('调用方')).to_have_text(str(callers))
            rows = card('调用方').locator('tbody tr')
            assert rows.count() == callers
            if model_name:
                expect(card('模型分布')).to_contain_text(model_name)
                expect(card('模型分布').locator('tbody tr').filter(has_text=model_name).locator('td').nth(1)).to_have_text(f'{model_tokens:,}')
                expect(card('按模型明细')).to_contain_text(model_name)
                active = rows.filter(has_text='当前调用方')
                expect(active.locator('td').nth(1)).to_have_text(str(caller_requests))
                expect(active.locator('td').nth(2)).to_have_text(f'{caller_tokens:,}')
            observed.append(dict(case=label, requests=requests, callers=callers, tokens=tokens,
                                 model=model_name, model_tokens=model_tokens, usage_reads=f.usage_reads))

        check('all-time-entry', 25, 2, 2500, 'fixture:lifetime-model', 1000, 10, 1000)

        page.get_by_role('button', name='今天', exact=True).click()
        check('today-click', 3, 1, 300, 'fixture:daily-model', 300, 3, 300)
        expect(page.locator('#view')).not_to_contain_text('fixture:lifetime-model')
        expect(page.locator('#view')).not_to_contain_text('历史调用方')
        expect(card('模型分布')).to_contain_text('100%')
        assert card('模型分布').locator('.donut').evaluate('(node)=>getComputedStyle(node).backgroundImage').startswith('conic-gradient(')
        page.screenshot(path=str(output/'usage-today-filtered.png'), full_page=True, animations='disabled')

        page.locator('#usage-from').fill('2026-11-19')
        page.locator('#usage-to').fill('2026-11-20')
        page.get_by_role('button', name='查询', exact=True).click()
        check('custom-range-query-click', 5, 1, 500, 'fixture:daily-model', 500, 5, 500)

        page.locator('#usage-from').fill('2026-11-10')
        page.locator('#usage-to').fill('2026-11-10')
        page.get_by_role('button', name='查询', exact=True).click()
        check('empty-range-query-click', 0, 0, 0, None, 0, 0, 0)
        assert card('模型分布').locator('.donut').count() == 0
        expect(page.locator('#view')).not_to_contain_text('fixture:daily-model')

        page.get_by_role('button', name='重置', exact=True).click()
        check('reset-click', 25, 2, 2500, 'fixture:lifetime-model', 1000, 10, 1000)
        expect(card('模型分布')).to_contain_text('fixture:old-model')

        page.get_by_role('button', name='近 7 天', exact=True).click()
        check('last-seven-days-click', 5, 1, 500, 'fixture:daily-model', 500, 5, 500)
        page.get_by_role('button', name='刷新用量', exact=True).click()
        check('refresh-preserves-range', 5, 1, 500, 'fixture:daily-model', 500, 5, 500)
        assert f.usage_reads == 2, 'date filters must reuse the loaded ledger; only entry and explicit refresh may download it'
        assert sum(path == '/api/workbench/providers' for _, path in f.requests) == 1, 'date filters must not reload provider metadata'

        # Explicit refresh must expose newly recorded usage without resetting
        # the selected dates; a cache must never turn refresh into a no-op.
        f.usage_payload = snapshot()
        fresh = totals(4, 360, 40, 3)
        f.usage_payload['days'][1]['totals'] = fresh
        f.usage_payload['keys'][0]['days'][1]['totals'] = fresh
        f.usage_payload['keys'][0]['day_models'][1]['models'][0]['totals'] = fresh
        page.get_by_role('button', name='刷新用量', exact=True).click()
        check('refresh-fetches-new-ledger', 6, 1, 600, 'fixture:daily-model', 600, 6, 600)
        assert f.usage_reads == 3

        # Failed refresh retains the prior visible read for context, marks it
        # failed and disables reuse as a fresh local snapshot.
        f.usage_error = 503
        page.get_by_role('button', name='刷新用量', exact=True).click()
        expect(page.locator('#view[data-load-state=failed]')).to_be_visible()
        page.get_by_role('button', name='今天', exact=True).click()
        expect(page.locator('#toast')).to_contain_text('数据已失效')
        expect(tile('总请求')).to_have_text('6')
        assert f.usage_reads == 4
        f.usage_error = None
        page.get_by_role('button', name='刷新用量', exact=True).click()
        check('failed-refresh-can-recover', 4, 1, 400, 'fixture:daily-model', 400, 4, 400)

        # Leaving the route clears page-local data. Returning must read again,
        # and the request from the old page cannot repaint the newer route.
        f.hold('/api/usage')
        page.get_by_role('button', name='刷新用量', exact=True).click()
        f.wait_held('/api/usage')
        page.locator('#navigation a[href="#keys"]').click()
        page.locator('[data-action=key-new]').wait_for()
        f.release('/api/usage', snapshot())
        page.wait_for_timeout(60)
        expect(page.locator('[data-action=key-new]')).to_be_visible()
        assert page.locator('[data-total-requests]').count() == 0
        page.locator('#navigation a[href="#usage"]').click()
        check('navigation-reads-new-snapshot', 4, 1, 400, 'fixture:daily-model', 400, 4, 400)

        # Going offline invalidates the local cache; it cannot silently keep
        # applying date changes after the authenticated connection is gone.
        f.context.set_offline(True)
        expect(page.locator('#connection')).to_be_visible()
        before = f.usage_reads
        page.get_by_role('button', name='全部', exact=True).click()
        expect(page.locator('#toast')).to_contain_text('当前离线')
        expect(tile('总请求')).to_have_text('4')
        assert f.usage_reads == before
        f.context.set_offline(False)
        page.get_by_role('button', name='刷新用量', exact=True).click()
        check('online-explicit-refresh-restores-snapshot', 25, 2, 2500, 'fixture:lifetime-model', 1000, 10, 1000)

        f.usage_error = 401
        page.get_by_role('button', name='刷新用量', exact=True).click()
        expect(page.locator('#view[data-load-state=failed]')).to_be_visible()
        before = f.usage_reads
        page.get_by_role('button', name='今天', exact=True).click()
        expect(page.locator('#toast')).to_contain_text('数据已失效')
        assert f.usage_reads == before
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth+1')
        return dict(result='PASS', cases=observed, usage_reads=f.usage_reads,
                    guards=['filter_uses_same_snapshot','explicit_refresh_reads_new_data','failed_refresh_invalidates_cache',
                            'late_response_does_not_repaint_new_route','returning_route_reads_again',
                            'offline_invalidates_cache','unauthorized_invalidates_cache'],
                    screenshot='usage-today-filtered.png', external_requests=f.external, page_errors=f.errors,
                    timezone='Asia/Shanghai', fixture_only=True)
    finally:
        f.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    output = Path(args.output).resolve()
    if output.drive.upper() != 'D:':
        raise SystemExit('D_drive_evidence_required')
    output.mkdir(parents=True, exist_ok=True)
    server = assets_server()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            try:
                report = exercise(browser, f'http://127.0.0.1:{server.server_port}', output)
            finally:
                browser.close()
        (output/'usage-filters-browser.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
        print(json.dumps({'result': report['result'], 'cases': len(report['cases']), 'usage_reads': report['usage_reads'],
                          'page_errors': len(report['page_errors']), 'external_requests': len(report['external_requests'])}))
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


if __name__ == '__main__':
    main()
