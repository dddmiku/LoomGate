"""Verify hot-update completion loads fresh frontend code using synthetic loopback only."""
import argparse
import json
from pathlib import Path
import sys
import threading
from urllib.parse import urlsplit

from playwright.sync_api import expect, sync_playwright

sys.path.insert(0, str(Path(__file__).resolve().parent))
from console_interactions_browser import ASSETS, PANEL, answer, assets_server
from token_units_browser import TokenFixture


class ReloadFixture(TokenFixture):
    def __init__(self, browser, base, status_patch=None):
        super().__init__(browser, base)
        self.status_patch = status_patch or {}
        self.apply_count = 0
        self.final_reads = 0
        self.documents = []
        self.app_versions = []
        self.update = dict(ok=True, status=dict(enabled=True, repo='fixture-owner/fixture-product',
            current='v1.0.0', commit='fixture-old', state='idle', latest_tag='v1.0.2',
            asset_name='fixture-runtime.zip', update_ready=True, signature_verification=True,
            checked_at='2026-11-20T00:00:00Z'))

    def route(self, route):
        request = route.request
        parsed = urlsplit(request.url)
        if parsed.hostname != '127.0.0.1':
            return super().route(route)
        path = parsed.path
        if request.resource_type == 'document':
            self.documents.append(path)
        version = 'fixture-new' if self.apply_count else 'fixture-old'
        if path == '/console/app.js':
            self.app_versions.append(version)
            body = (PANEL / 'console/app.js').read_text(encoding='utf-8')
            route.fulfill(content_type='application/javascript',
                          body=body + '\nwindow.fixtureLoadedVersion=' + json.dumps(version) + ';')
        elif path == '/legacy':
            body = (PANEL / 'index.html').read_text(encoding='utf-8')
            body = body.replace('</body>', '<script>window.fixtureLoadedVersion=' +
                                json.dumps(version) + ';</script></body>')
            route.fulfill(content_type='text/html', body=body)
        elif path == '/api/update/apply' and request.method == 'POST':
            self.requests.append(('POST', path))
            self.apply_count += 1
            assert request.post_data_json == dict(tag='v1.0.2')
            self.update['status'].update(current='v1.0.2', commit='fixture-new', state='idle',
                                         update_ready=False, target_version='v1.0.2')
            self.update['status'].update(self.status_patch)
            answer(route, self.update)
        else:
            if path == '/api/update' and self.apply_count:
                self.final_reads += 1
            super().route(route)


def open_settings(f, legacy=False, broken_storage=False):
    f.context.add_cookies([dict(name='fixture-session', value='synthetic', url=f.base)])
    if broken_storage:
        f.context.add_init_script("Storage.prototype.setItem=function(){throw new Error('fixture storage unavailable')};")
    f.page.goto(f.base + ('/legacy#system' if legacy else '/#settings'))
    if legacy:
        f.page.locator('[data-settings=updates]').click()
    button = f.page.locator('#btnUpdApply' if legacy else '[data-action=update-apply]')
    expect(button).to_be_enabled()
    expect(button).to_be_visible()
    f.page.wait_for_function("window.fixtureLoadedVersion==='fixture-old'")
    if not broken_storage:
        assert f.page.evaluate("sessionStorage.getItem('fixture-preserved')") is None
        # Seed only this loaded document; a later navigation cannot recreate this evidence.
        f.page.evaluate("sessionStorage.setItem('fixture-preserved','keep');")
    return button


def checked(f):
    assert not f.external, 'external_request'
    assert not f.errors, f.errors
    assert f.apply_count == 1, 'apply request was replayed'


def successful_reload(browser, base, legacy=False):
    f = ReloadFixture(browser, base)
    try:
        button = open_settings(f, legacy)
        with f.page.expect_navigation(wait_until='domcontentloaded'):
            button.click()
        f.page.wait_for_function("window.fixtureLoadedVersion==='fixture-new'")
        expect(f.page).to_have_url(base + ('/legacy#system' if legacy else '/#settings'))
        assert len(f.documents) == 2, 'updated frontend document was not loaded exactly once'
        assert f.page.evaluate("sessionStorage.getItem('fixture-preserved')") == 'keep'
        assert any(c['name'] == 'fixture-session' and c['value'] == 'synthetic' for c in f.context.cookies())
        assert not f.page.evaluate("sessionStorage.getItem('loomgate.unconfirmedUpdate')")
        marker = f.page.evaluate("JSON.parse(sessionStorage.getItem('loomgate.frontendReload'))")
        assert marker == dict(repo='fixture-owner/fixture-product', tag='v1.0.2', commit='fixture-new')
        if legacy:
            f.page.locator('[data-settings=updates]').click()
            expect(f.page.locator('#updRows')).to_contain_text('v1.0.2')
            f.page.evaluate('loadUpdate()')
        else:
            expect(f.page.locator('#setting-update .update-facts')).to_contain_text('v1.0.2')
            f.page.locator('[data-action=update-check]').click()
            assert f.app_versions == ['fixture-old', 'fixture-new'], 'app.js was not fetched again'
        f.page.wait_for_timeout(120)
        assert len(f.documents) == 2, 'status revisit caused another reload'
        f.page.reload(wait_until='domcontentloaded')
        f.page.wait_for_timeout(120)
        assert len(f.documents) == 3, 'manual refresh entered a reload loop'
        assert f.page.evaluate("sessionStorage.getItem('fixture-preserved')") == 'keep'
        if legacy:
            f.page.locator('button[data-view=usage]').first.click()
            expect(f.page.locator('#usageTiles .usage-tile').filter(has=f.page.locator('.k', has_text='合计 tokens')).locator('.v')).to_have_text('1.24B')
        else:
            f.page.locator('#navigation a[href="#usage"]').click()
            tile = f.page.locator('article.usage-tile').filter(has=f.page.locator('span', has_text='总 Token'))
            expect(tile.locator('strong')).to_have_text('1.24B')
            expect(tile.locator('strong [title]')).to_have_attribute('title', '1,235,567,889 tokens')
        checked(f)
        return dict(documents_after_update=2, apply_requests=f.apply_count, exact_units_visible=True,
                    fresh_app_requested=legacy or f.app_versions[:2] == ['fixture-old', 'fixture-new'])
    finally:
        f.close()


def no_reload(browser, base, patch, legacy=False, broken_storage=False):
    f = ReloadFixture(browser, base, patch)
    try:
        button = open_settings(f, legacy, broken_storage)
        button.click()
        for _ in range(100):
            if f.apply_count and (legacy or f.final_reads):
                break
            f.page.wait_for_timeout(10)
        assert f.apply_count == 1
        if not legacy:
            assert f.final_reads, 'accepted update was never read back'
        f.page.wait_for_timeout(120)
        assert len(f.documents) == 1, 'unverified update or unavailable storage refreshed the frontend'
        assert f.page.evaluate('window.fixtureLoadedVersion') == 'fixture-old'
        if patch.get('state') == 'failed':
            expect(f.page.locator('#updModalText' if legacy else '#setting-update')).to_contain_text('fixture update rejected')
        if legacy and patch.get('repo'):
            assert not f.page.evaluate('UP.modalDone'), 'different repository was reported as complete'
        checked(f)
        return dict(documents=1, apply_requests=1)
    finally:
        f.close()


def historical_check_does_not_reload(browser, base):
    f = ReloadFixture(browser, base)
    try:
        f.update['status'].update(current='v1.0.2', target_version='v1.0.2', update_ready=False)
        f.page.goto(base + '/legacy#system')
        f.page.locator('[data-settings=updates]').click()
        expect(f.page.locator('#updRows')).to_contain_text('v1.0.2')
        assert len(f.documents) == 1
        f.update['status']['state'] = 'checking'
        f.page.evaluate('loadUpdate()')
        assert f.page.evaluate('Boolean(UP.timer)'), 'pure check did not retain polling'
        assert not f.page.evaluate('UP.awaiting'), 'pure check was promoted to an application'
        f.update['status']['state'] = 'idle'
        f.page.wait_for_function('UP.timer === null')
        assert len(f.documents) == 1, 'historical completion plus a pure check refreshed frontend'
        assert f.apply_count == 0 and not f.external and not f.errors
        return dict(documents=1, apply_requests=0, check_poll_finished=True)
    finally:
        f.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    output = Path(args.output).resolve()
    if output.drive.upper() != 'D:' or output.is_relative_to(PANEL.parents[1]):
        raise SystemExit('external_D_drive_evidence_required')
    output.mkdir(parents=True, exist_ok=True)
    ASSETS['/legacy'] = ('index.html', 'text/html')
    server = assets_server()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    report = dict(result='FAIL', fixture_only=True, cases=[])
    try:
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            try:
                base = f'http://127.0.0.1:{server.server_port}'
                for legacy in (False, True):
                    name = 'legacy' if legacy else 'console'
                    report['cases'].append(dict(name=name + '_verified_update_loads_frontend_once',
                                               **successful_reload(browser, base, legacy)))
                    for label, patch in [
                        ('old_idle', dict(current='v1.0.0')),
                        ('failed', dict(state='failed', last_error='fixture update rejected')),
                        ('unhealthy', dict(healthy=False)),
                        ('not_ready', dict(ready=False)),
                        ('restart_required', dict(restart_required=True)),
                        ('different_repo', dict(repo='different-owner/different-product')),
                    ]:
                        report['cases'].append(dict(name=name + '_' + label + '_does_not_reload',
                                                   **no_reload(browser, base, patch, legacy)))
                    report['cases'].append(dict(name=name + '_storage_failure_keeps_manual_refresh',
                                               **no_reload(browser, base, {}, legacy, True)))
                report['cases'].append(dict(name='legacy_historical_target_pure_check_does_not_reload',
                                           **historical_check_does_not_reload(browser, base)))
                report['result'] = 'PASS'
            finally:
                browser.close()
    except Exception as error:
        report['error'] = type(error).__name__ + ': ' + str(error)
        raise
    finally:
        (output / 'update-frontend-reload-browser.json').write_text(
            json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
    print(json.dumps(dict(result=report['result'], cases=len(report['cases']), fixture_only=True)))


if __name__ == '__main__':
    main()
