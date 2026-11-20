"""Exercise credit readback and lazy request charges with local fixtures only."""

import threading
from urllib.parse import urlsplit

from playwright.sync_api import expect, sync_playwright

from console_interactions_browser import Fixture, account, answer, assets_server


class CreditFixture(Fixture):
    provider_reads = 0
    wallet_reads = 0
    detail_reads = 0

    def route(self, route):
        url = urlsplit(route.request.url)
        if url.hostname != '127.0.0.1':
            return super().route(route)
        if url.path == '/api/workbench/providers':
            self.provider_reads += 1
            first = self.provider_reads == 1
            providers = [dict(id=name, name=label, catalog_scope=scope, accounts=dict(total=1, enabled=1)) for name, label, scope in (
                ('workbuddy', 'WorkBuddy', 'provider'), ('trae', 'Trae', 'provider'),
                ('autoclaw', 'AutoClaw', 'account'), ('doubao', 'Doubao', 'account'),
            )]
            providers[2]['credit_totals'] = dict(accounts=1, ok=0, unknown=1) if first else dict(accounts=1, ok=1, remain=42, expiring=3, complete=True)
            return answer(route, dict(providers=providers))
        if url.path == '/api/state':
            workbuddy = dict(account('fixture-workbuddy', 'workbuddy'), credits=dict(remain=14, expiring=0))
            trae = dict(account('fixture-trae', 'trae'), realm='trae', credits=dict(remain=7))
            return answer(route, dict(accounts=[workbuddy, trae]))
        if url.path == '/api/workbench/accounts':
            self.wallet_reads += 1
            credit = dict(pending=True) if self.wallet_reads == 1 else dict(remain=42, expiring=3, expiring_text='明天到期')
            return answer(route, dict(accounts=[dict(account('fixture-wallet', 'autoclaw'), credits=credit)]))
        if url.path == '/api/requests':
            row = dict(request_id='fixture-request', model='fixture:model', status='success', attempt_count=2,
                       upstream_started=True, credit=0, missing_credit_attempts=1)
            return answer(route, dict(items=[row], total=1))
        if url.path == '/api/requests/fixture-request':
            self.detail_reads += 1
            return answer(route, dict(record=dict(request_id='fixture-request', attempts=[dict(number=1, credit=0), dict(number=2, credit=None)])))
        return super().route(route)


def exercise(browser, base):
    fixture = CreditFixture(browser, base)
    try:
        page = fixture.page
        page.goto(base + '/#overview')
        section = page.locator('#overview-provider-credits')
        expect(section.locator('[data-credit-provider="workbuddy"]')).to_contain_text('余额 14')
        expect(section.locator('[data-credit-provider="trae"]')).to_contain_text('临期 未知')
        expect(section.locator('[data-credit-provider="autoclaw"]')).to_contain_text('临期 3')
        expect(section.locator('[data-credit-provider="doubao"]')).to_have_count(0)
        assert fixture.provider_reads == 2

        page.locator('#navigation a[href="#accounts"]').click()
        page.locator('#account-provider [data-provider-choice="autoclaw"]').click()
        expect(page.locator('#account-credit-summary')).to_contain_text('42')
        expect(page.locator('[data-credit-account="fixture-wallet"]')).to_contain_text('明天到期')
        assert fixture.wallet_reads == 2

        page.locator('#navigation a[href="#logs"]').click()
        row = page.locator('[data-request-row="0"]')
        expect(row).to_contain_text('0 部分上报')
        assert fixture.detail_reads == 0
        row.locator('[data-action="request-expand"]').click()
        attempts = page.locator('[data-request-attempt-credit]')
        expect(attempts).to_contain_text('第 1 次：0')
        expect(attempts).to_contain_text('第 2 次：上游未上报')
        assert fixture.detail_reads == 1
        assert not fixture.external and not fixture.errors
    finally:
        fixture.close()


if __name__ == '__main__':
    server = assets_server()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            try:
                exercise(browser, f'http://127.0.0.1:{server.server_port}')
            finally:
                browser.close()
        print('PASS: supplier credits, account wallet readback, lazy attempt charges')
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
