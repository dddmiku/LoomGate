"""Exercise current console DOM with local, synthetic HTTP responses only.

python -I -B tests/console_interactions_browser.py --output D:/LoomGate-work/evidence/console-interactions
"""
import argparse
import errno
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import sync_playwright

PANEL = Path(__file__).resolve().parents[1] / 'panel'
ASSETS = {
    '/': ('console/index.html', 'text/html'),
    '/console/style.css': ('console/style.css', 'text/css'),
    '/console/app.js': ('console/app.js', 'application/javascript'),
    '/console/connections.js': ('console/connections.js', 'application/javascript'),
    '/console/plan-editor.js': ('console/plan-editor.js', 'application/javascript'),
    '/console/model-picker.js': ('console/model-picker.js', 'application/javascript'),
    '/vendor/qrcode.js': ('vendor/qrcode.js', 'application/javascript'),
}


class Assets(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        spec = ASSETS.get(urlsplit(self.path).path)
        if not spec:
            self.send_error(404)
            return
        content = (PANEL / spec[0]).read_bytes()
        self.send_response(200)
        self.send_header('Content-Type', spec[1] + '; charset=utf-8')
        self.send_header('Content-Length', str(len(content)))
        self.send_header('Cache-Control', 'no-store')
        self.end_headers()
        try:
            self.wfile.write(content)
        except (ConnectionError, BrokenPipeError):
            pass


def assets_server():
    # Windows can allocate a browser-blocked ephemeral port (observed: 6666).
    # Keep the fixture on a bounded high-port range without bypassing browser checks.
    for port in range(49152, 49216):
        try:
            return ThreadingHTTPServer(('127.0.0.1', port), Assets)
        except OSError as error:
            if error.errno != errno.EADDRINUSE and getattr(error, 'winerror', None) != 10048:
                raise
    raise RuntimeError('no free browser-safe fixture port in the reserved range')


def answer(route, body, status=200):
    route.fulfill(status=status, content_type='application/json', body=json.dumps(body))


def account(uid, provider):
    return {'id': uid, 'uid': uid, 'provider': provider, 'nickname': uid,
            'enabled': True, 'disabled': False, 'parsable': True, 'revision': 1, 'realm': 'cn'}


class Fixture:
    def __init__(self, browser, base):
        self.context = browser.new_context(viewport={'width': 1280, 'height': 900})
        self.page = self.context.new_page()
        self.page.set_default_timeout(5000)
        self.base = base
        self.errors = []
        self.external = []
        self.requests = []
        self.request_urls = []
        self.holds = {}
        self.pending = {}
        self.starts = 0
        self.run_bodies = []
        self.catalog_tasks = None
        self.start_status = 'uncertain'
        self.latest_flow = None
        self.account_enabled = {'workbuddy': True, 'autoclaw': True}
        self.key = dict(id='fixture-key', name='Fixture client', masked_key='fixture-mask', enabled=True, models=[], total_tokens=0)
        self.key_present = True
        self.created_keys = []
        self.update = {'ok': True, 'status': {'enabled': False, 'current': 'fixture'}}
        self.session = {'ok': True, 'username': 'fixture-admin', 'expiresAt': 4070908800}
        self.lan = {'enabled': True, 'listen': '0.0.0.0:7864', 'allow_cidrs': '192.0.2.0/24', 'has_key': True}
        self.plans = [dict(id='fixture-plan-'+name, provider_id='workbuddy', task_key=name,
                           account_ids=['fixture-workbuddy'], hours=[6], minute=10,
                           timezone='Asia/Shanghai', enabled=False, once_per_day=True)
                      for name in ('checkin', 'balance')]
        self.plans.append(dict(id='fixture-plan-sequence', provider_id='workbuddy', task_key='growth-sequence',
                               task_sequence=['travel/', 'growth/'], account_ids=['fixture-workbuddy'],
                               hours=[12], minute=0, timezone='Asia/Shanghai', enabled=False,
                               once_per_day=False))
        self.page.on('pageerror', lambda err: self.errors.append(type(err).__name__))
        self.context.route('**/*', self.route)

    def hold(self, path, method='GET'):
        self.holds[(method, path)] = True
        self.pending[(method, path)] = []

    def wait_held(self, path, method='GET', timeout_ms=1000):
        for _ in range(max(1, timeout_ms//10)):
            if self.pending.get((method, path)):
                return self.pending[(method, path)][0]
            self.page.wait_for_timeout(10)
        raise AssertionError('synthetic request did not enter held route: ' + path)

    def release(self, path, payload, method='GET', status=200):
        key = (method, path)
        route = self.pending[key].pop(0)
        self.holds[key] = False
        answer(route, payload, status)

    def route(self, route):
        request = route.request
        parsed = urlsplit(request.url)
        if parsed.hostname != '127.0.0.1':
            self.external.append(parsed.hostname)
            route.abort()
            return
        path, method = parsed.path, request.method
        if not path.startswith('/api/'):
            route.continue_()
            return
        self.requests.append((method, path))
        self.request_urls.append(request.url)
        if self.holds.get((method, path)):
            self.pending[(method, path)].append(route)
            return
        if path == '/api/workbench/providers':
            answer(route, {'providers': [
                dict(id='workbuddy', name='WorkBuddy', catalog_scope='provider', inference_available=True, accounts={'total': 1, 'enabled': 1}),
                dict(id='autoclaw', name='AutoClaw', catalog_scope='account', inference_available=True, accounts={'total': 1, 'enabled': 1}),
            ]})
        elif path == '/api/state':
            record = account('fixture-workbuddy', 'workbuddy')
            record['disabled'] = not self.account_enabled['workbuddy']
            answer(route, {'accounts': [record],
                           'pool': {'total': 1, 'healthy': int(self.account_enabled['workbuddy'])}, 'creditTotals': {'remain': 12, 'size': 20}, 'lan': self.lan})
        elif path == '/api/workbench/accounts':
            record = account('fixture-autoclaw', 'autoclaw')
            record['enabled'] = self.account_enabled['autoclaw']
            answer(route, {'accounts': [record]})
        elif path == '/api/workbench':
            answer(route, {'schedules': self.plans, 'runs': []})
        elif path == '/api/workbench/catalog':
            tasks = [dict(provider_id='workbuddy', realm='cn', account_id='fixture-workbuddy', key=key,
                          name=name, manual_only=False, once=False, once_per_day=False)
                     for key, name in [('checkin', '每日签到'), ('balance', '余额刷新'),
                                       ('travel/arrive', 'Buddy旅行'), ('growth/live', '成长奖励')]]
            answer(route, {'tasks': tasks if self.catalog_tasks is None else self.catalog_tasks, 'issues': []})
        elif path == '/api/workbench/run' and method == 'POST':
            self.run_bodies.append(request.post_data_json)
            answer(route, {'ok': True, 'run': {'id': 'fixture-sequence-run', 'status': 'queued'}})
        elif path == '/api/connections/providers':
            answer(route, {'providers': [dict(id='autoclaw', methods=[dict(id='sms', name='短信登录', available=True)])]})
        elif path == '/api/keys' and method == 'GET':
            answer(route, {'ok': True, 'keys': ([self.key] if self.key_present else []) + self.created_keys})
        elif path == '/api/key-limits':
            answer(route, {'ok': True, 'keys': {}})
        elif path == '/api/migration':
            answer(route, {'ok': True, 'marker': None, 'tool': {'available': False}})
        elif path == '/api/task/log':
            answer(route, {'ok': True, 'key': 'checkin', 'lines': ['fixture synthetic task output'], 'running': False, 'endedAt': '2099-01-01T00:00:00Z'})
        elif path == '/api/session':
            answer(route, self.session)
        elif path == '/api/models':
            answer(route, {'ok': True, 'items': []})
        elif path == '/api/features/reasoning-loop':
            answer(route, {'ok': True, 'stop_only': False})
        elif path == '/api/update':
            answer(route, self.update)
        elif path == '/api/update/check' and method == 'POST':
            answer(route, self.update)
        elif path == '/api/keys/update' and method == 'POST':
            self.key.update(request.post_data_json)
            answer(route, {'ok': True})
        elif path == '/api/keys/delete' and method == 'POST':
            self.key_present = False
            answer(route, {'ok': True})
        elif path == '/api/auth/password' and method == 'POST':
            self.session['username'] = request.post_data_json['username']
            answer(route, self.session)
        elif path == '/api/lan' and method == 'POST':
            self.lan.update(request.post_data_json)
            answer(route, {'ok': True, 'message': '合成配置已保存，实际监听未改变。'})
        elif path == '/api/connections/start':
            self.starts += 1
            self.latest_flow = self.flow('fixture-flow-'+str(self.starts))
            self.latest_flow['status'] = self.start_status
            answer(route, {'flow': self.latest_flow})
        elif path == '/api/connections/step' and request.post_data_json.get('action') == 'commit':
            self.latest_flow['status'] = 'succeeded'
            answer(route, {'flow': self.latest_flow})
        elif path == '/api/connections/flows':
            answer(route, {'flows': [self.latest_flow] if self.latest_flow else []})
        else:
            answer(route, {'ok': False, 'error': 'fixture_route_unexpected'}, 404)

    @staticmethod
    def flow(identifier):
        return dict(id=identifier, provider='autoclaw', status='uncertain', can_verify=True,
                    input_mode='official', method='sms', expires_at='2099-01-01T00:00:00Z', notice='Fixture flow')

    def open_accounts(self):
        self.page.goto(self.base+'/#accounts')
        self.page.locator('[data-action=toggle-account]').wait_for()

    def open_connection(self):
        self.page.locator('[data-action=connect-account]').click()
        self.page.locator('#connection-begin:not([disabled])').wait_for()

    def close(self):
        self.holds.clear()
        for routes in self.pending.values():
            for route in routes:
                route.abort()
        self.pending.clear()
        self.context.unroute_all(behavior='wait')
        self.context.close()
        assert not self.errors, 'console page raised errors: ' + str(self.errors)
        assert not self.external, 'console attempted external network access'


def stale_connection_step(browser, base, failed):
    f = Fixture(browser, base)
    try:
        f.open_accounts()
        f.open_connection()
        f.page.locator('#connection-begin').click()
        f.page.locator('#connection-verify').wait_for()
        f.hold('/api/connections/step', 'POST')
        f.page.locator('#connection-verify').click()
        f.wait_held('/api/connections/step', 'POST')
        f.page.locator('#dialog-close').click()
        f.open_connection()
        f.hold('/api/connections/start', 'POST')
        f.page.locator('#connection-begin').click()
        f.wait_held('/api/connections/start', 'POST')
        assert f.page.locator('#connection-begin').is_disabled()
        f.release('/api/connections/step', {'ok': False} if failed else {'flow': f.flow('fixture-flow-1')},
                  'POST', status=503 if failed else 200)
        f.page.wait_for_timeout(60)
        assert f.page.locator('#connection-begin').is_disabled(), 'old step unlocked new login submission'
        assert f.page.locator('#connection-error').is_hidden(), 'old step wrote an error into new login'
        f.page.locator('#connection-begin').evaluate('(button)=>button.click()')
        f.page.wait_for_timeout(30)
        assert len(f.pending[('POST', '/api/connections/start')]) == 1, 'duplicate new login request was submitted'
        f.release('/api/connections/start', {'flow': f.flow('fixture-flow-2')}, 'POST')
        f.page.locator('#connection-verify:not([disabled])').wait_for()
    finally:
        f.close()


def late_plan_save(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#tasks')
        f.page.locator('[data-plan-row=fixture-plan-checkin]').wait_for()
        f.page.locator('[data-plan-row=fixture-plan-checkin] [data-action=edit-plan-time]').click()
        f.page.locator('[name=plan-time]').fill('07:10')
        f.hold('/api/workbench/schedule', 'POST')
        f.page.locator('#plan-save').click()
        held = f.wait_held('/api/workbench/schedule', 'POST')
        submitted = held.request.post_data_json
        f.page.locator('#dialog-close').click()
        f.page.locator('[data-plan-row=fixture-plan-balance] [data-action=edit-plan-time]').click()
        f.page.locator('[name=plan-time]').fill('09:10')
        f.plans[0].update(submitted)
        f.release('/api/workbench/schedule', {'ok': True, 'schedule': f.plans[0]}, 'POST')
        f.page.wait_for_function("document.querySelector('[data-plan-row=fixture-plan-checkin] [data-plan-timing]').textContent.includes('07:10')")
        assert f.page.locator('#dialog').is_visible(), 'late save closed second editor'
        assert f.page.locator('[name=plan-time]').input_value() == '09:10', 'late save discarded second draft'
        assert f.page.locator('h1').count() == 1, 'route duplicated its heading'
    finally:
        f.close()


def scope_switch(browser, base):
    f = Fixture(browser, base)
    try:
        f.open_accounts()
        f.page.locator('[data-provider-choice=autoclaw]').click()
        f.page.locator('#account-list .account-name').filter(has_text='fixture-autoclaw').wait_for()
        f.hold('/api/state')
        f.page.locator('[data-provider-choice=workbuddy]').click()
        f.wait_held('/api/state')
        f.page.locator('[data-provider-choice=autoclaw]').click()
        f.page.locator('#account-list .account-name').filter(has_text='fixture-autoclaw').wait_for()
        try:
            f.release('/api/state', {'accounts': [account('fixture-workbuddy', 'workbuddy')]})
        except Exception:
            # Aborting the superseded fetch may already have closed its route.
            pass
        f.page.wait_for_timeout(50)
        assert f.page.locator('[data-provider-choice=autoclaw]').get_attribute('aria-pressed') == 'true'
        assert f.page.locator('[data-action=toggle-account]').get_attribute('data-provider') == 'autoclaw'
        assert f.page.locator('#account-list').get_by_text('fixture-workbuddy', exact=True).count() == 0
        assert f.page.locator('h1').count() == 1
        assert f.page.locator('[data-route-loading]').count() == 0
    finally:
        f.close()


def commit_once(browser, base, response_lost):
    f = Fixture(browser, base)
    try:
        f.start_status = 'verified'
        f.open_accounts()
        f.open_connection()
        f.hold('/api/connections/step', 'POST')
        f.page.locator('#connection-begin').click()
        f.wait_held('/api/connections/step', 'POST')
        if response_lost:
            f.release('/api/connections/step', {'error': 'fixture_lost_commit_response'}, 'POST', status=503)
            f.page.locator('#connection-retry-save').wait_for()
            assert not f.holds[('POST', '/api/connections/step')]
            commits = [call for call in f.requests if call == ('POST', '/api/connections/step')]
            assert len(commits) == 1, 'lost response automatically replayed commit'
            f.hold('/api/connections/flows')
            f.page.locator('#connection-retry-save').click()
            f.wait_held('/api/connections/flows')
            f.release('/api/connections/flows', {'error': 'fixture_status_unreadable'}, status=503)
            f.page.wait_for_timeout(50)
            commits = [call for call in f.requests if call == ('POST', '/api/connections/step')]
            assert len(commits) == 1, 'commit repeated without checking status'
        else:
            succeeded = dict(f.latest_flow, status='succeeded')
            f.release('/api/connections/step', {'flow': succeeded}, 'POST')
            f.page.locator('#connection-done').wait_for()
            commits = [call for call in f.requests if call == ('POST', '/api/connections/step')]
            assert len(commits) == 1
            assert f.page.locator('#dialog-title').inner_text().endswith('账号已接入')
    finally:
        f.close()


def key_toggle(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#keys')
        f.page.locator('[data-action=key-toggle]').wait_for()
        f.page.evaluate("window.fixtureEndpoint=document.querySelector('.endpoint-card');window.fixtureKeyRow=document.querySelector('[data-action=key-toggle]').closest('tr')")
        before = len(f.requests)
        f.page.locator('[data-action=key-toggle]').click()
        f.page.wait_for_function("document.querySelector('[data-action=key-toggle]').dataset.enabled==='0'")
        assert f.page.evaluate("window.fixtureEndpoint===document.querySelector('.endpoint-card')&&window.fixtureKeyRow===document.querySelector('[data-action=key-toggle]').closest('tr')")
        assert f.page.locator('[data-key-enabled]').inner_text() == '停用'
        assert f.requests[before:] == [('POST', '/api/keys/update'), ('GET', '/api/keys')]
    finally:
        f.close()


def canceled_page_reads(browser, base, route_name, held_path):
    f = Fixture(browser, base)
    try:
        f.open_accounts()
        f.hold(held_path)
        f.page.evaluate('(route)=>location.hash=route', '#'+route_name)
        f.wait_held(held_path)
        f.page.evaluate("location.hash='#tasks'")
        f.page.locator('[data-plan-row=fixture-plan-checkin]').wait_for()
        f.release(held_path, {'ok': True})
        f.page.wait_for_timeout(60)
        assert f.page.locator('[data-plan-row=fixture-plan-checkin]').count() == 1, 'canceled read replaced the task page'
        assert f.page.locator('h1').count() == 1
        assert f.page.locator('h1').inner_text() == '任务'
    finally:
        f.close()


def provider_scoped_task_log(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#tasks')
        f.page.locator('[data-plan-row=fixture-plan-checkin]').wait_for()
        f.page.locator('[data-plan-row=fixture-plan-checkin] [data-action=task-log]').last.click()
        f.page.wait_for_function("document.querySelector('#task-log-body')?.textContent.includes('fixture')")
        task_request = next(url for url in f.request_urls if '/api/task/log?' in url)
        query = parse_qs(urlsplit(task_request).query)
        assert query.get('provider') == ['workbuddy'] and query.get('key') == ['checkin']
        assert 'fixture' in f.page.locator('#task-log-body').inner_text()
    finally:
        f.close()


def responsive_pages(browser, base):
    f = Fixture(browser, base)
    try:
        for width in (320, 390, 1440):
            f.page.set_viewport_size({'width': width, 'height': 900})
            for route in ('accounts', 'tasks', 'settings', 'keys'):
                f.page.goto(base+'/#'+route)
                marker = {'accounts': '[data-action=toggle-account]', 'tasks': '[data-plan-row=fixture-plan-checkin]',
                          'settings': '#setting-update', 'keys': '.endpoint-card'}[route]
                f.page.locator(marker).wait_for()
                assert f.page.locator('h1').count() == 1, 'duplicate page heading after rendering '+route
                assert f.page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'horizontal page overflow: '+route+' at '+str(width)
    finally:
        f.close()


def captured_account_intent(browser, base, provider):
    f = Fixture(browser, base)
    try:
        f.open_accounts()
        if provider == 'autoclaw':
            f.page.locator('[data-provider-choice=autoclaw]').click()
            f.page.locator('[data-action=toggle-account][data-provider=autoclaw]').wait_for()
        path = '/api/workbench/account/toggle' if provider == 'autoclaw' else '/api/account/toggle'
        f.page.locator('[data-action=toggle-account]').click()
        f.hold(path, 'POST')
        f.page.locator('#account-toggle-form button[type=submit]').click()
        first = f.wait_held(path, 'POST').request.post_data_json
        f.page.locator('#dialog-close').click()
        f.page.locator('[data-action=toggle-account]').click()
        assert f.page.locator('#account-toggle-form button[type=submit]').inner_text() == '确认停用'
        f.account_enabled[provider] = False
        f.release(path, {'ok': True}, 'POST')
        f.page.wait_for_function("document.querySelector('[data-action=toggle-account]').textContent==='启用'")
        f.hold(path, 'POST')
        f.page.locator('#account-toggle-form button[type=submit]').click()
        second = f.wait_held(path, 'POST').request.post_data_json
        if provider == 'autoclaw':
            assert first['enabled'] is False and second['enabled'] is False, 'late readback reversed the confirmed account operation'
            assert second['revision'] == first['revision'], 'confirmation silently adopted a different account revision'
        else:
            assert first['disabled'] is True and second['disabled'] is True, 'late readback reversed the confirmed account operation'
        f.release(path, {'ok': True}, 'POST')
    finally:
        f.close()


def commit_refresh_overlap(browser, base, commit_first):
    f = Fixture(browser, base)
    try:
        f.start_status = 'verified'
        f.open_accounts()
        f.open_connection()
        f.hold('/api/connections/step', 'POST')
        f.page.locator('#connection-begin').click()
        f.wait_held('/api/connections/step', 'POST')
        snapshot = dict(f.latest_flow)
        f.hold('/api/connections/flows')
        f.page.locator('#connection-refresh').click()
        f.wait_held('/api/connections/flows')
        if not commit_first:
            f.release('/api/connections/flows', {'flows': [snapshot]})
            f.page.wait_for_timeout(50)
        f.release('/api/connections/step', {'flow': dict(snapshot, status='succeeded')}, 'POST')
        if commit_first:
            f.page.locator('#connection-done').wait_for()
            f.release('/api/connections/flows', {'flows': [snapshot]})
        f.page.wait_for_timeout(60)
        assert f.page.locator('#connection-done').count() == 1, 'refresh lost the confirmed saved state'
        assert f.page.locator('#dialog-title').inner_text().endswith('账号已接入'), 'old verified snapshot replaced saved account'
        assert '正在安全保存' not in f.page.locator('#dialog-body').inner_text()
        assert sum(call == ('POST', '/api/connections/step') for call in f.requests) == 1, 'refresh replayed commit'
    finally:
        f.close()


def same_log_overlap(browser, base, old_fails):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#tasks')
        f.page.locator('[data-plan-row=fixture-plan-checkin]').wait_for()
        f.hold('/api/task/log')
        f.page.locator('[data-plan-row=fixture-plan-checkin] [data-action=task-log]').last.click()
        f.wait_held('/api/task/log')
        f.page.locator('[data-action=task-log-refresh]').click()
        for _ in range(100):
            if len(f.pending[('GET', '/api/task/log')]) == 2:
                break
            f.page.wait_for_timeout(10)
        assert len(f.pending[('GET', '/api/task/log')]) == 2
        newer = f.pending[('GET', '/api/task/log')].pop(1)
        answer(newer, {'running': False, 'lines': ['fixture-finished'], 'endedAt': '2099-01-01T00:00:00Z'})
        f.page.wait_for_function("document.querySelector('#task-log-body').textContent==='fixture-finished'")
        f.release('/api/task/log', {'error': 'fixture_old_read_failed'} if old_fails else {'running': True, 'lines': ['fixture-old']}, status=503 if old_fails else 200)
        f.page.wait_for_timeout(60)
        assert f.page.locator('#task-log-body').inner_text() == 'fixture-finished', 'older log request overwrote the newest output'
        assert f.page.locator('.task-log-status').inner_text().startswith('最近一次'), 'older response reverted the running state'
    finally:
        f.close()


def late_key_result(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#keys')
        f.page.locator('[data-action=key-new]').click()
        f.page.locator('#key-form [name=name]').fill('Fixture first')
        f.hold('/api/keys', 'POST')
        f.page.locator('#key-form button[type=submit]').click()
        f.wait_held('/api/keys', 'POST')
        f.page.locator('#dialog-close').click()
        f.page.locator('[data-action=key-new]').click()
        f.page.locator('#key-form [name=name]').fill('Fixture second draft')
        f.page.evaluate("window.fixtureSecondForm=document.querySelector('#key-form');window.fixtureFocused=document.activeElement")
        f.release('/api/keys', {'ok': True, 'key': 'synthetic-placeholder-not-a-credential'}, 'POST')
        f.page.wait_for_timeout(60)
        assert f.page.locator('#key-form').count() == 1, 'late creation removed the second form'
        assert f.page.locator('#key-form [name=name]').input_value() == 'Fixture second draft', 'late creation discarded the new draft'
        assert f.page.evaluate("window.fixtureSecondForm===document.querySelector('#key-form')&&window.fixtureFocused===document.activeElement"), 'late creation replaced the form or moved focus'
        f.page.locator('#dialog-close').click()
        assert f.page.locator('[data-key-created-result] textarea').input_value() == 'synthetic-placeholder-not-a-credential', 'completed creation result was lost'
        assert f.page.evaluate("![...Object.values(localStorage),...Object.values(sessionStorage)].some(value=>value.includes('synthetic-placeholder-not-a-credential'))"), 'creation result leaked into browser storage'
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1
    finally:
        f.close()


def sequence_plan_run_preserves_selector_and_scope(browser, base, default_scope=False):
    f = Fixture(browser, base)
    if default_scope:
        f.plans[2]['account_ids'] = []
    try:
        f.page.goto(base+'/#tasks')
        row = f.page.locator('[data-plan-row=fixture-plan-sequence]')
        row.wait_for()
        row.locator('[data-action=run-plan]').click()
        f.page.locator('#run-form').wait_for()
        assert 'fixture-workbuddy' in f.page.locator('#run-form').inner_text(), 'confirmation hid the accounts resolved from the current catalog'
        f.page.locator('#run-form button[type=submit]').click()
        for _ in range(100):
            if f.run_bodies:
                break
            f.page.wait_for_timeout(10)
        assert len(f.run_bodies) == 1, 'sequence plan did not submit exactly one run request'
        body = f.run_bodies[0]
        assert body.get('task_key') == 'growth-sequence'
        assert body.get('task_sequence') == ['travel/', 'growth/'], 'sequence selectors were dropped or reordered'
        assert body.get('account_ids') == ['fixture-workbuddy'], 'saved account scope was dropped or broadened'
    finally:
        f.close()


def sequence_plan_save_keeps_selectors(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#tasks')
        row = f.page.locator('[data-plan-row=fixture-plan-sequence]')
        row.wait_for()
        f.page.evaluate("window.fixtureSequenceRow=document.querySelector('[data-plan-row=fixture-plan-sequence]')")
        f.hold('/api/workbench/schedule', 'POST')
        row.locator('[data-action=toggle-schedule]').click()
        submitted = f.wait_held('/api/workbench/schedule', 'POST').request.post_data_json
        assert submitted.get('task_sequence') == ['travel/', 'growth/']
        assert submitted.get('account_ids') == ['fixture-workbuddy']
        f.plans[2].update(submitted)
        f.release('/api/workbench/schedule', {'ok': True, 'schedule': f.plans[2]}, 'POST')
        f.page.wait_for_function("document.querySelector('[data-plan-row=fixture-plan-sequence] [role=switch]').getAttribute('aria-checked')==='true'")
        assert f.page.evaluate("window.fixtureSequenceRow===document.querySelector('[data-plan-row=fixture-plan-sequence]')"), 'sequence toggle replaced the row'
        row.locator('[data-action=edit-plan-time]').click()
        f.page.locator('[name=plan-time]').fill('13:00')
        f.hold('/api/workbench/schedule', 'POST')
        f.page.locator('#plan-save').click()
        submitted = f.wait_held('/api/workbench/schedule', 'POST').request.post_data_json
        assert submitted.get('task_sequence') == ['travel/', 'growth/'], 'editing time discarded the sequence'
        assert submitted.get('account_ids') == ['fixture-workbuddy']
        f.plans[2].update(submitted)
        f.release('/api/workbench/schedule', {'ok': True, 'schedule': f.plans[2]}, 'POST')
        f.page.wait_for_function("document.querySelector('[data-plan-row=fixture-plan-sequence] [data-plan-timing]').textContent.includes('13:00')")
        assert not f.run_bodies, 'saving a sequence dispatched business actions'
    finally:
        f.close()


def sequence_plan_missing_scope_never_runs(browser, base):
    f = Fixture(browser, base)
    f.plans[2]['account_ids'] = []
    f.catalog_tasks = []
    try:
        f.page.goto(base+'/#tasks')
        row = f.page.locator('[data-plan-row=fixture-plan-sequence]')
        row.wait_for()
        row.locator('[data-action=run-plan]').click()
        f.page.wait_for_timeout(40)
        assert f.page.locator('#run-form').count() == 0, 'unread account scope opened a write confirmation'
        assert not f.run_bodies, 'missing scope became all accounts'
    finally:
        f.close()


def open_key_page(f):
    f.page.goto(f.base+'/#keys')
    f.page.locator('[data-action=key-edit]').wait_for()
    f.page.evaluate("window.fixtureEndpoint=document.querySelector('.endpoint-card');window.fixtureKeyRow=document.querySelector('[data-key-row]').closest('tr')")


def open_new_key_draft(f):
    if f.page.locator('#dialog').evaluate('(node)=>node.open'):
        f.page.locator('#dialog-close').click()
    f.page.locator('[data-action=key-new]').click()
    f.page.locator('#key-form [name=name]').fill('Fixture newer draft')
    f.page.evaluate("window.fixtureNewForm=document.querySelector('#key-form');window.fixtureDraftFocus=document.activeElement")


def assert_new_key_draft(f, message):
    assert f.page.locator('#dialog').evaluate('(node)=>node.open'), message
    assert f.page.locator('#key-form [name=name]').count() == 1, message
    assert f.page.locator('#key-form [name=name]').input_value() == 'Fixture newer draft', message
    assert f.page.evaluate("window.fixtureNewForm===document.querySelector('#key-form')&&window.fixtureDraftFocus===document.activeElement"), message


def late_key_edit_read(browser, base, failed=False):
    f = Fixture(browser, base)
    try:
        open_key_page(f)
        f.hold('/api/keys')
        f.page.locator('[data-action=key-edit]').click()
        f.wait_held('/api/keys')
        open_new_key_draft(f)
        f.release('/api/keys', {'error': 'fixture_old_read_failed'} if failed else {'ok': True, 'keys': [f.key]}, status=503 if failed else 200)
        f.page.wait_for_timeout(70)
        assert_new_key_draft(f, 'late key edit read replaced the new draft')
        if failed:
            assert not f.page.locator('#toast').is_visible(), 'obsolete key read reported an error into the new action'
    finally:
        f.close()


def late_key_edit_save(browser, base):
    f = Fixture(browser, base)
    try:
        open_key_page(f)
        f.page.locator('[data-action=key-edit]').click()
        f.page.locator('#key-form [name=name]').fill('Fixture renamed')
        f.page.locator('#key-form [name=note]').fill('Fixture note')
        f.page.locator('#key-form [name=requests_per_minute]').fill('23')
        f.hold('/api/keys/update', 'POST')
        f.page.locator('#key-form button[type=submit]').click()
        submitted = f.wait_held('/api/keys/update', 'POST').request.post_data_json
        open_new_key_draft(f)
        f.key.update(submitted)
        f.release('/api/keys/update', {'ok': True}, 'POST')
        f.page.wait_for_function("document.querySelector('[data-key-row] strong')?.textContent==='Fixture renamed'")
        assert_new_key_draft(f, 'late key edit save closed or replaced the new draft')
        assert f.page.evaluate("window.fixtureEndpoint===document.querySelector('.endpoint-card')&&window.fixtureKeyRow===document.querySelector('[data-key-row]').closest('tr')"), 'key editing rebuilt the endpoint or key row'
        assert 'Fixture note' in f.page.locator('[data-key-row]').inner_text()
        assert '23' in f.page.locator('[data-key-row]').locator('xpath=ancestor::tr').inner_text()
        assert sum(call == ('POST', '/api/keys/update') for call in f.requests) == 1
        assert sum(call == ('GET', '/api/key-limits') for call in f.requests) == 1, 'key edit reloaded the entire key page'
    finally:
        f.close()


def mismatched_key_edit(browser, base, field):
    f = Fixture(browser, base)
    try:
        open_key_page(f)
        f.page.locator('[data-action=key-edit]').click()
        f.page.locator('#key-form [name=note]').fill('Fixture changed note')
        f.page.locator('#key-form [name=requests_per_minute]').fill('23')
        f.hold('/api/keys/update', 'POST')
        f.page.locator('#key-form button[type=submit]').click()
        body = f.wait_held('/api/keys/update', 'POST').request.post_data_json
        f.key.update(body)
        f.key[field] = 'Fixture wrong note' if field == 'note' else {'requests_per_minute': 24}
        f.release('/api/keys/update', {'ok': True}, 'POST')
        f.page.wait_for_timeout(80)
        assert f.page.locator('#dialog').evaluate('(node)=>node.open'), 'key edit claimed saved despite mismatched '+field
        assert f.page.locator('#form-message').is_visible(), 'key edit did not report the readback mismatch'
        assert f.page.locator('#key-form [name=note]').input_value() == 'Fixture changed note', 'readback mismatch discarded the form'
    finally:
        f.close()


def late_key_delete(browser, base):
    f = Fixture(browser, base)
    try:
        open_key_page(f)
        f.page.locator('[data-action=key-delete]').click()
        f.page.locator('#key-delete-form input[type=checkbox]').check()
        f.hold('/api/keys/delete', 'POST')
        f.page.locator('#key-delete-form button[type=submit]').click()
        f.wait_held('/api/keys/delete', 'POST')
        open_new_key_draft(f)
        f.key_present = False
        f.release('/api/keys/delete', {'ok': True}, 'POST')
        f.page.wait_for_function("document.querySelector('[data-key-row]')===null")
        assert_new_key_draft(f, 'late deletion destroyed the new key draft')
        assert f.page.evaluate("window.fixtureEndpoint===document.querySelector('.endpoint-card')"), 'key deletion rebuilt the endpoint card'
        assert sum(call == ('POST', '/api/keys/delete') for call in f.requests) == 1
    finally:
        f.close()


def late_key_copy(browser, base):
    f = Fixture(browser, base)
    try:
        open_key_page(f)
        f.hold('/api/keys/copy', 'POST')
        f.page.locator('[data-action=key-copy]').click()
        f.wait_held('/api/keys/copy', 'POST')
        open_new_key_draft(f)
        f.release('/api/keys/copy', {'ok': True, 'key': 'synthetic-copy-placeholder-not-a-credential'}, 'POST')
        f.page.wait_for_timeout(70)
        assert_new_key_draft(f, 'late key display replaced the new draft')
        assert 'synthetic-copy-placeholder-not-a-credential' not in f.page.content(), 'canceled reveal exposed the old sensitive result'
        assert f.page.evaluate("![...Object.values(localStorage),...Object.values(sessionStorage)].some(value=>value.includes('synthetic-copy-placeholder-not-a-credential'))")
    finally:
        f.close()


def open_password_draft(f):
    if f.page.locator('#dialog').evaluate('(node)=>node.open'):
        f.page.locator('#dialog-close').click()
    f.page.locator('[data-action=password]').click()
    f.page.locator('#password-form [name=username]').fill('fixture_second_admin')
    f.page.evaluate("window.fixtureNewForm=document.querySelector('#password-form');window.fixtureDraftFocus=document.activeElement")


def assert_password_draft(f, message):
    assert f.page.locator('#dialog').evaluate('(node)=>node.open'), message
    assert f.page.locator('#password-form [name=username]').count() == 1, message
    assert f.page.locator('#password-form [name=username]').input_value() == 'fixture_second_admin', message
    assert f.page.evaluate("window.fixtureNewForm===document.querySelector('#password-form')&&window.fixtureDraftFocus===document.activeElement"), message


def late_password_save(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#settings')
        f.page.locator('[data-action=password]').click()
        # All input is synthetic and never sent outside the loopback fixture.
        for name, value in {'current': 'fixture-old-value', 'username': 'fixture_new_admin', 'password': 'fixture-new-value', 'confirm': 'fixture-new-value'}.items():
            f.page.locator('#password-form [name='+name+']').fill(value)
        f.page.evaluate("window.fixtureSettings=document.querySelector('#setting-loop')")
        f.hold('/api/auth/password', 'POST')
        f.page.locator('#password-form button[type=submit]').click()
        f.wait_held('/api/auth/password', 'POST')
        open_password_draft(f)
        f.session['username'] = 'fixture_new_admin'
        f.release('/api/auth/password', f.session, 'POST')
        f.page.wait_for_timeout(100)
        assert_password_draft(f, 'late password save closed or reset the newer form')
        assert f.page.evaluate("window.fixtureSettings===document.querySelector('#setting-loop')"), 'password save rebuilt unrelated settings'
        assert sum(call == ('POST', '/api/auth/password') for call in f.requests) == 1
        assert sum(call == ('GET', '/api/session') for call in f.requests) == 2, 'changed session was not read back'
    finally:
        f.close()


def late_lan_read(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#settings')
        f.page.locator('[data-action=lan-settings]').wait_for()
        f.hold('/api/state')
        f.page.locator('[data-action=lan-settings]').click()
        f.wait_held('/api/state')
        open_password_draft(f)
        f.release('/api/state', {'lan': f.lan})
        f.page.wait_for_timeout(70)
        assert_password_draft(f, 'late LAN read replaced the newer password draft')
    finally:
        f.close()


def late_lan_save(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#settings')
        f.page.locator('[data-action=lan-settings]').click()
        f.page.locator('#lan-form [name=allow_cidrs]').fill('198.51.100.0/24')
        f.page.evaluate("window.fixtureSettings=document.querySelector('#setting-loop')")
        f.hold('/api/lan', 'POST')
        f.page.locator('#lan-form button[type=submit]').click()
        body = f.wait_held('/api/lan', 'POST').request.post_data_json
        open_password_draft(f)
        f.lan.update(body)
        f.release('/api/lan', {'ok': True, 'message': '合成配置已保存，实际监听未改变。'}, 'POST')
        f.page.wait_for_timeout(100)
        assert_password_draft(f, 'late LAN save closed the newer password draft')
        assert f.page.evaluate("window.fixtureSettings===document.querySelector('#setting-loop')"), 'LAN save rebuilt unrelated settings'
        assert '198.51.100.0/24' in f.page.locator('#setting-lan').inner_text(), 'LAN readback was not reflected in its own card'
        assert sum(call == ('POST', '/api/lan') for call in f.requests) == 1
    finally:
        f.close()


def current_key_copy_is_displayed_only_in_its_dialog(browser, base):
    f = Fixture(browser, base)
    try:
        open_key_page(f)
        f.hold('/api/keys/copy', 'POST')
        f.page.locator('[data-action=key-copy]').click()
        f.wait_held('/api/keys/copy', 'POST')
        f.release('/api/keys/copy', {'ok': True, 'key': 'synthetic-copy-placeholder-not-a-credential'}, 'POST')
        f.page.locator('#dialog-body textarea').wait_for()
        assert f.page.locator('#dialog-body textarea').input_value() == 'synthetic-copy-placeholder-not-a-credential'
        f.page.locator('#dialog-close').click()
        assert 'synthetic-copy-placeholder-not-a-credential' not in f.page.content(), 'closing the reveal retained sensitive output'
        assert f.page.evaluate("![...Object.values(localStorage),...Object.values(sessionStorage)].some(value=>value.includes('synthetic-copy-placeholder-not-a-credential'))")
        assert sum(call == ('POST', '/api/keys/copy') for call in f.requests) == 1
    finally:
        f.close()


def key_mutations_on_same_record_are_serialized(browser, base):
    f = Fixture(browser, base)
    try:
        open_key_page(f)
        f.page.locator('[data-action=key-edit]').click()
        f.page.locator('#key-form [name=name]').fill('Fixture serialized edit')
        f.hold('/api/keys/update', 'POST')
        f.page.locator('#key-form button[type=submit]').click()
        body = f.wait_held('/api/keys/update', 'POST').request.post_data_json
        f.page.locator('#dialog-close').click()
        f.page.locator('[data-action=key-toggle]').click()
        f.page.wait_for_timeout(50)
        assert len(f.pending[('POST', '/api/keys/update')]) == 1, 'same-key operations raced before either was read back'
        f.key.update(body)
        f.release('/api/keys/update', {'ok': True}, 'POST')
        f.page.wait_for_function("document.querySelector('[data-key-row] strong')?.textContent==='Fixture serialized edit'")
    finally:
        f.close()


def password_session_mismatch_stays_unconfirmed(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#settings')
        f.page.locator('[data-action=password]').click()
        for name, value in {'current': 'fixture-old-value', 'username': 'fixture_new_admin', 'password': 'fixture-new-value', 'confirm': 'fixture-new-value'}.items():
            f.page.locator('#password-form [name='+name+']').fill(value)
        f.hold('/api/auth/password', 'POST')
        f.page.locator('#password-form button[type=submit]').click()
        f.wait_held('/api/auth/password', 'POST')
        f.release('/api/auth/password', {'ok': True, 'username': 'fixture_new_admin', 'expiresAt': f.session['expiresAt']}, 'POST')
        f.page.locator('#form-message:not([hidden])').wait_for()
        assert f.page.locator('#password-form [name=username]').input_value() == 'fixture_new_admin', 'mismatched session discarded the pending form'
        assert sum(call == ('POST', '/api/auth/password') for call in f.requests) == 1, 'session mismatch replayed the password change'
    finally:
        f.close()


def lan_disabling_clears_cidrs_and_updates_only_its_card(browser, base):
    f = Fixture(browser, base)
    try:
        f.page.goto(base+'/#settings')
        f.page.locator('[data-action=lan-settings]').click()
        f.page.locator('#lan-form [name=enabled]').uncheck()
        f.page.evaluate("window.fixtureSettings=document.querySelector('#setting-loop');window.fixtureLanCard=document.querySelector('#setting-lan')")
        f.hold('/api/lan', 'POST')
        f.page.locator('#lan-form button[type=submit]').click()
        body = f.wait_held('/api/lan', 'POST').request.post_data_json
        assert body == {'enabled': False, 'allow_cidrs': ''}, 'disabling LAN retained an old allowlist in the verification contract'
        f.lan.update(body)
        f.lan['listen'] = '127.0.0.1:7864'
        f.release('/api/lan', {'ok': True, 'message': '合成配置已保存，实际监听未改变。'}, 'POST')
        f.page.wait_for_function("document.querySelector('#setting-lan [data-lan-title]')?.textContent==='配置为仅本机'")
        assert f.page.evaluate("window.fixtureSettings===document.querySelector('#setting-loop')&&window.fixtureLanCard===document.querySelector('#setting-lan')"), 'LAN settings rebuilt its card or siblings'
        assert '实际监听' in f.page.locator('#setting-lan').inner_text(), 'saved LAN configuration was presented as the current listener'
        assert sum(call == ('POST', '/api/lan') for call in f.requests) == 1
    finally:
        f.close()


SYNTHETIC_CREATED_KEY = 'synthetic-created-placeholder-not-a-credential'


def start_key_creation(f, empty=False):
    f.key_present = not empty
    f.page.goto(f.base+'/#keys')
    f.page.locator('[data-action=key-new]').wait_for()
    f.page.evaluate("window.fixtureEndpoint=document.querySelector('.endpoint-card');window.fixtureKeyRow=document.querySelector('[data-key-row]')?.closest('tr')")
    f.page.locator('[data-action=key-new]').click()
    f.page.locator('#key-form [name=name]').fill('Fixture created client')
    f.page.locator('#key-form [name=note]').fill('Fixture created note')
    f.hold('/api/keys', 'POST')
    f.page.locator('#key-form button[type=submit]').click()
    submitted = f.wait_held('/api/keys', 'POST').request.post_data_json
    return dict(submitted, id='fixture-created-key', masked_key='fixture-created-mask',
                enabled=True, total_tokens=0)


def release_created_key(f, entry, readback='match'):
    if readback != 'missing':
        saved = dict(entry)
        if readback == 'mismatch':
            saved['note'] = 'Fixture conflicting note'
        f.created_keys.append(saved)
    f.release('/api/keys', {'ok': True, 'entry': entry, 'key': SYNTHETIC_CREATED_KEY}, 'POST')
    f.page.locator('[data-key-created-result]').wait_for()


def key_creation_ack_is_local(browser, base, empty=False):
    f = Fixture(browser, base)
    try:
        if empty:
            f.page.set_viewport_size({'width': 390, 'height': 844})
        entry = start_key_creation(f, empty)
        release_created_key(f, entry)
        f.page.wait_for_timeout(70)
        f.page.locator('[data-key-created-result] button').click()
        f.page.wait_for_timeout(70)
        assert f.page.evaluate("window.fixtureEndpoint===document.querySelector('.endpoint-card')"), 'acknowledging a created key rebuilt the endpoint card'
        if not empty:
            assert f.page.evaluate("window.fixtureKeyRow===document.querySelector('[data-key-row=fixture-key]').closest('tr')"), 'creating a key replaced the existing key row'
        row = f.page.locator('[data-key-row=fixture-created-key]')
        assert row.count() == 1, 'created key was not inserted exactly once into the local list'
        assert 'Fixture created note' in row.inner_text()
        assert sum(call == ('GET', '/api/key-limits') for call in f.requests) == 1, 'key creation reloaded unrelated live limit snapshots'
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1
        assert f.page.locator('[data-key-created-result]').count() == 0, 'acknowledged creation retained its sensitive result'
        assert SYNTHETIC_CREATED_KEY not in f.page.content(), 'acknowledged creation retained the plaintext in the page'
    finally:
        f.close()


def late_key_creation_sync_keeps_draft(browser, base):
    f = Fixture(browser, base)
    try:
        entry = start_key_creation(f)
        open_new_key_draft(f)
        release_created_key(f, entry)
        f.page.wait_for_timeout(70)
        assert f.page.locator('[data-key-row=fixture-created-key]').count() == 1, 'late creation did not synchronize its new row'
        assert_new_key_draft(f, 'late creation synchronization discarded the newer draft')
        assert f.page.evaluate("window.fixtureEndpoint===document.querySelector('.endpoint-card')&&window.fixtureKeyRow===document.querySelector('[data-key-row=fixture-key]').closest('tr')"), 'late creation synchronization rebuilt existing content'
        f.page.locator('#dialog-close').click()
        result = f.page.locator('[data-key-created-result]')
        assert result.locator('textarea').input_value() == SYNTHETIC_CREATED_KEY
        result.locator('button').click()
        assert result.count() == 0
        assert f.page.locator('[data-key-row=fixture-created-key]').count() == 1
        assert f.page.evaluate("![...Object.values(localStorage),...Object.values(sessionStorage)].some(value=>value.includes('synthetic-created-placeholder-not-a-credential'))"), 'creation result leaked into browser storage'
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1
    finally:
        f.close()


def unconfirmed_key_creation_keeps_result(browser, base, mode):
    f = Fixture(browser, base)
    try:
        entry = start_key_creation(f)
        release_created_key(f, entry, mode)
        f.page.wait_for_timeout(70)
        assert sum(call == ('GET', '/api/keys') for call in f.requests) == 2, 'creation did not read back its returned key id'
        result = f.page.locator('[data-key-created-result]')
        assert result.locator('textarea').input_value() == SYNTHETIC_CREATED_KEY, 'failed readback discarded the one-time creation result'
        assert result.locator('[data-key-sync]').get_attribute('data-state') == 'unconfirmed', 'unmatched creation readback was presented as verified'
        assert f.page.locator('[data-key-row=fixture-created-key]').count() == 0, 'unmatched creation readback inserted an unverified row'
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1, 'failed creation readback replayed the write'
        assert f.page.evaluate("window.fixtureEndpoint===document.querySelector('.endpoint-card')"), 'failed creation readback rebuilt the page'
    finally:
        f.close()


def creation_readback_after_ack_keeps_new_draft(browser, base):
    f = Fixture(browser, base)
    try:
        entry = start_key_creation(f)
        f.hold('/api/keys')
        release_created_key(f, entry)
        f.page.wait_for_timeout(60)
        assert len(f.pending[('GET', '/api/keys')]) == 1, 'creation did not start a scoped readback before acknowledgment'
        f.page.locator('[data-key-created-result] button').click()
        open_new_key_draft(f)
        f.release('/api/keys', {'ok': True, 'keys': [f.key, entry]})
        f.page.wait_for_timeout(70)
        assert_new_key_draft(f, 'creation readback after acknowledgment changed the new draft')
        assert f.page.locator('[data-key-row=fixture-created-key]').count() == 1
        assert f.page.locator('[data-key-created-result]').count() == 0, 'late readback recreated an acknowledged plaintext result'
        assert f.page.evaluate("window.fixtureEndpoint===document.querySelector('.endpoint-card')"), 'late creation readback rebuilt the endpoint'
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1
    finally:
        f.close()


def creation_readback_failure_keeps_result(browser, base):
    f = Fixture(browser, base)
    try:
        entry = start_key_creation(f)
        f.hold('/api/keys')
        release_created_key(f, entry)
        f.wait_held('/api/keys')
        f.release('/api/keys', {'ok': False, 'error': 'fixture_readback_unavailable'}, status=503)
        f.page.wait_for_function("document.querySelector('[data-key-sync]')?.dataset.state==='unconfirmed'")
        assert f.page.locator('[data-key-created-result] textarea').input_value() == SYNTHETIC_CREATED_KEY
        assert f.page.locator('[data-key-row=fixture-created-key]').count() == 0
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1, 'unavailable creation readback replayed the write'
        assert f.page.evaluate("![...Object.values(localStorage),...Object.values(sessionStorage)].some(value=>value.includes('synthetic-created-placeholder-not-a-credential'))")
    finally:
        f.close()


def creation_requires_returned_identity(browser, base, invalid_receipt=False):
    f = Fixture(browser, base)
    try:
        entry = start_key_creation(f)
        if invalid_receipt:
            entry['name'] = 'Fixture mismatched receipt'
        else:
            f.created_keys.append(dict(entry, id='fixture-other-key'))
        release_created_key(f, entry, 'missing')
        f.page.wait_for_function("document.querySelector('[data-key-sync]')?.dataset.state==='unconfirmed'")
        assert f.page.locator('[data-key-created-result] textarea').input_value() == SYNTHETIC_CREATED_KEY
        assert f.page.locator('[data-key-row=fixture-created-key]').count() == 0
        assert f.page.locator('[data-key-row=fixture-other-key]').count() == 0, 'creation matched another key by name instead of returned id'
        assert sum(call == ('GET', '/api/keys') for call in f.requests) == (1 if invalid_receipt else 2)
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1
    finally:
        f.close()


def creation_old_readback_cannot_revert_revisited_page(browser, base):
    f = Fixture(browser, base)
    try:
        entry = start_key_creation(f)
        f.hold('/api/keys')
        release_created_key(f, entry)
        f.wait_held('/api/keys')
        f.page.locator('[data-key-created-result] button').click()
        f.page.evaluate("location.hash='tasks'")
        f.page.locator('[data-plan-row=fixture-plan-checkin]').wait_for()
        f.page.evaluate("location.hash='keys'")
        for _ in range(100):
            if len(f.pending[('GET', '/api/keys')]) == 2:
                break
            f.page.wait_for_timeout(10)
        assert len(f.pending[('GET', '/api/keys')]) == 2
        newer = f.pending[('GET', '/api/keys')].pop(1)
        updated = dict(entry, note='Fixture newer external note')
        answer(newer, {'ok': True, 'keys': [f.key, updated]})
        f.page.wait_for_function("document.querySelector('[data-key-row=fixture-created-key]')?.textContent.includes('Fixture newer external note')")
        f.page.evaluate("window.fixtureRevisitedRow=document.querySelector('[data-key-row=fixture-created-key]').closest('tr');window.fixtureRevisitedEndpoint=document.querySelector('.endpoint-card')")
        f.release('/api/keys', {'ok': True, 'keys': [f.key, entry]})
        f.page.wait_for_timeout(70)
        assert 'Fixture newer external note' in f.page.locator('[data-key-row=fixture-created-key]').inner_text(), 'old creation readback reverted newer list metadata'
        assert f.page.evaluate("window.fixtureRevisitedRow===document.querySelector('[data-key-row=fixture-created-key]').closest('tr')&&window.fixtureRevisitedEndpoint===document.querySelector('.endpoint-card')")
        assert f.page.locator('[data-key-created-result]').count() == 0
        assert sum(call == ('POST', '/api/keys') for call in f.requests) == 1
    finally:
        f.close()


def ready_update(f):
    f.update = {'ok': True, 'status': {'enabled': True, 'current': 'v1.0.0', 'commit': 'fixture-original',
        'repo': 'fixture-owner/fixture-product', 'visibility': 'public', 'state': 'idle',
        'latest_tag': 'v1.0.2', 'asset_name': 'fixture-runtime.zip', 'asset_size': 2097152,
        'update_ready': True, 'signature_verification': True, 'checked_at': '2026-11-20T00:00:00Z',
        'built_at': '2026-11-19T00:00:00Z'}}
    f.page.goto(f.base+'/#settings')
    f.page.locator('#setting-update').wait_for()
    assert f.page.locator('#setting-update [data-action=update-apply]:visible').count() == 1, 'signed ready update has no apply control'
    f.page.evaluate("window.fixtureLoopCard=document.querySelector('#setting-loop');window.fixtureUpdateCard=document.querySelector('#setting-update')")


def stale_update_intent_recovers_automatically(browser, base, failed=False):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.page.evaluate("sessionStorage.setItem('loomgate.unconfirmedUpdate',JSON.stringify({tag:'v1.0.2',repo:'fixture-owner/fixture-product'}))")
        if failed:
            f.update['status'].update(state='failed', last_error='fixture automatic handover failed')
        f.page.locator('[data-action=update-status]').click()
        f.page.wait_for_timeout(80)
        assert not f.page.evaluate("sessionStorage.getItem('loomgate.unconfirmedUpdate')"), 'authoritative idle/failed status left stale browser protection in place'
        assert f.page.locator('[data-action=update-review]:visible').count() == 0, 'old intent still asks for manual protection removal'
        assert f.page.locator('#dialog[open]').count() == 0
        if failed:
            assert 'fixture automatic handover failed' in f.page.locator('#setting-update').inner_text()
            assert f.page.locator('[data-action=update-check]:visible').count() == 1
        else:
            assert f.page.locator('[data-action=update-apply]:visible').count() == 1
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 0
    finally:
        f.close()


def one_click_update_has_no_confirmation_form(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        f.page.wait_for_timeout(80)
        assert f.page.locator('#update-form').count() == 0, 'update still opens an unnecessary form/checkbox confirmation'
        request = f.wait_held('/api/update/apply', 'POST').request.post_data_json
        assert request == {'tag': 'v1.0.2'}
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
    finally:
        f.close()


def accepted_idle_update_reconnects_without_replay(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update/apply', 'POST')
        f.hold('/api/update')
        f.release('/api/update/apply', {'ok': True}, 'POST')
        f.wait_held('/api/update')
        f.release('/api/update', f.update)
        f.page.wait_for_timeout(80)
        assert f.page.evaluate("Boolean(sessionStorage.getItem('loomgate.unconfirmedUpdate'))"), 'accepted operation was unlocked by an early idle status'
        assert f.page.locator('[data-action=update-apply]').is_hidden()
        f.page.locator('[data-action=update-apply]').evaluate("button=>button.dispatchEvent(new MouseEvent('click',{bubbles:true}))")
        f.page.wait_for_timeout(60)
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
        f.hold('/api/update')
        f.wait_held('/api/update', timeout_ms=4000)
        f.release('/api/update', {'ok': False, 'error': 'fixture_reconnect'}, status=503)
        f.update['status'].update(current='v1.0.2', state='idle', update_ready=False, healthy=True)
        f.page.wait_for_function("document.querySelector('[data-update-action-status]')?.textContent.includes('运行版本已核验')")
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
    finally:
        f.close()


def revisited_update_recovers_automatically(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.page.evaluate("sessionStorage.setItem('loomgate.unconfirmedUpdate',JSON.stringify({tag:'v1.0.2',repo:'fixture-owner/fixture-product',started_at:Date.now()}))")
        f.update['status'].update(state='handover', target_version='v1.0.2', auto_restart=True)
        f.page.reload()
        f.page.wait_for_function("document.querySelector('#setting-update')?.textContent.includes('自动切换中')")
        f.update['status'].update(current='v1.0.2', state='idle', update_ready=False)
        f.page.wait_for_function("document.querySelector('[data-update-action-status]')?.textContent.includes('运行版本已核验')")
        assert not f.page.evaluate("sessionStorage.getItem('loomgate.unconfirmedUpdate')")
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 0
    finally:
        f.close()


def abandoned_update_preflight_never_sends(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update')
        f.page.evaluate("location.hash='tasks'")
        f.page.locator('[data-plan-row=fixture-plan-checkin]').wait_for()
        f.release('/api/update', f.update)
        f.page.wait_for_timeout(70)
        assert f.page.locator('[data-plan-row=fixture-plan-checkin]').count() == 1
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 0
    finally:
        f.close()


def update_apply_is_one_click_and_scoped(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        request = f.wait_held('/api/update/apply', 'POST').request.post_data_json
        assert request == {'tag': 'v1.0.2'}, 'update did not freeze the clicked target tag'
        assert f.page.locator('#dialog[open]').count() == 0
        assert f.page.evaluate("JSON.parse(sessionStorage.getItem('loomgate.unconfirmedUpdate')).tag") == 'v1.0.2'
        f.page.locator('[data-action=update-apply]').evaluate("button=>button.dispatchEvent(new MouseEvent('click',{bubbles:true}))")
        f.page.wait_for_timeout(40)
        assert len(f.pending[('POST', '/api/update/apply')]) == 1, 'one click update submitted twice'
        f.update['status'].update(state='downloading', downloaded_bytes=1048576, auto_restart=True)
        f.release('/api/update/apply', {'ok': True, 'status': f.update['status']}, 'POST')
        f.page.wait_for_function("document.querySelector('#setting-update').textContent.includes('已下载')")
        assert f.page.evaluate("window.fixtureLoopCard===document.querySelector('#setting-loop')&&window.fixtureUpdateCard===document.querySelector('#setting-update')")
        assert '运行版本已核验' not in f.page.locator('[data-update-action-status]').inner_text()
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
    finally:
        f.close()

def late_update_read_keeps_password_draft(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update')
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update')
        open_password_draft(f)
        f.release('/api/update', f.update)
        f.wait_held('/api/update/apply', 'POST')
        f.update['status']['state'] = 'handover'
        f.release('/api/update/apply', {'ok': True}, 'POST')
        f.page.wait_for_timeout(70)
        assert_password_draft(f, 'late update preflight replaced the newer password draft')
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
    finally:
        f.close()

def update_target_change_is_not_applied_silently(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update')
        f.update['status']['latest_tag'] = 'v1.0.3'
        f.release('/api/update', f.update)
        f.page.wait_for_function("document.querySelector('[data-update-action-status]')?.textContent.includes('目标或运行构建已变化')")
        assert not any(call == ('POST', '/api/update/apply') for call in f.requests)
        assert not f.page.evaluate("sessionStorage.getItem('loomgate.unconfirmedUpdate')")
        assert f.page.locator('#dialog[open]').count() == 0
    finally:
        f.close()

def lost_update_response_is_not_replayed(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update/apply', 'POST')
        f.update['status'].update(state='handover', auto_restart=True, staged_version='v1.0.2')
        f.hold('/api/update')
        f.release('/api/update/apply', {'ok': False, 'error': 'fixture_lost_response'}, 'POST', status=503)
        f.wait_held('/api/update')
        f.release('/api/update', {'ok': False, 'error': 'fixture_restarting'}, status=503)
        f.page.wait_for_function("document.querySelector('[data-update-action-status]')?.textContent.includes('自动')")
        assert f.page.locator('[data-action=update-apply]').is_hidden()
        assert f.page.evaluate("Boolean(sessionStorage.getItem('loomgate.unconfirmedUpdate'))")
        f.update['status'].update(current='v1.0.2', state='idle', update_ready=False, staged_version='', restart_required=False, healthy=True)
        f.page.wait_for_function("document.querySelector('[data-update-action-status]')?.textContent.includes('运行版本已核验')")
        assert not f.page.evaluate("sessionStorage.getItem('loomgate.unconfirmedUpdate')")
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
    finally:
        f.close()

def late_update_acceptance_preserves_password_draft(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update/apply', 'POST')
        open_password_draft(f)
        f.update['status']['state'] = 'downloading'
        f.release('/api/update/apply', {'ok': True, 'status': f.update['status']}, 'POST')
        f.page.wait_for_timeout(70)
        assert_password_draft(f, 'late update acceptance closed or replaced the password draft')
        assert f.page.evaluate("window.fixtureLoopCard===document.querySelector('#setting-loop')&&window.fixtureUpdateCard===document.querySelector('#setting-update')")
    finally:
        f.close()

def update_zero_time_is_unchecked(browser, base):
    f = Fixture(browser, base)
    try:
        f.update = {'ok': True, 'status': {'enabled': True, 'state': 'idle', 'current': 'v1.0.0',
            'signature_verification': True, 'checked_at': '0001-01-01T00:00:00Z'}}
        f.page.goto(base+'/#settings')
        f.page.locator('#setting-update').wait_for()
        assert '已是最新' not in f.page.locator('#setting-update').inner_text(), 'zero Go timestamp was presented as a completed check'
        assert '未检查' in f.page.locator('#setting-update').inner_text()
    finally:
        f.close()


def update_completion_requires_running_version(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update/apply', 'POST')
        f.update['status'].update(state='handover', staged_version='v1.0.2', auto_restart=True)
        f.release('/api/update/apply', {'ok': True, 'status': f.update['status']}, 'POST')
        f.page.wait_for_function("document.querySelector('#setting-update').textContent.includes('自动切换中')")
        assert '运行版本已核验' not in f.page.locator('[data-update-action-status]').inner_text()
        f.update['status'].update(current='v1.0.2', commit='fixture-new-commit', state='idle', update_ready=False, healthy=False)
        f.page.wait_for_timeout(2100)
        assert '运行版本已核验' not in f.page.locator('[data-update-action-status]').inner_text(), 'unhealthy target was reported as complete'
        f.update['status']['healthy'] = True
        f.page.wait_for_function("document.querySelector('[data-update-action-status]')?.textContent.includes('运行版本已核验')")
        assert not f.page.evaluate("sessionStorage.getItem('loomgate.unconfirmedUpdate')")
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
        assert '构建时间' in f.page.locator('#setting-update .update-facts').inner_text()
        assert not any(label.inner_text() == '部署时间' for label in f.page.locator('#setting-update .update-facts dt').all())
    finally:
        f.close()

def unsigned_update_has_no_apply_action(browser, base):
    f = Fixture(browser, base)
    try:
        f.update = {'ok': True, 'status': {'enabled': True, 'state': 'idle', 'current': 'v1.0.0',
            'signature_verification': False, 'latest_tag': 'v1.0.2', 'repo': 'fixture-owner/fixture-product',
            'asset_name': 'fixture.zip', 'update_ready': True}}
        f.page.goto(base+'/#settings')
        f.page.locator('#setting-update').wait_for()
        assert f.page.locator('[data-action=update-apply]:visible').count() == 0
        assert not any(call == ('POST', '/api/update/apply') for call in f.requests)
    finally:
        f.close()


def update_read_order_keeps_newest_status(browser, base, failed=False):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update')
        f.page.locator('[data-action=update-status]').click()
        f.wait_held('/api/update')
        f.page.locator('[data-action=update-status]').click()
        for _ in range(100):
            if len(f.pending[('GET', '/api/update')]) == 2:
                break
            f.page.wait_for_timeout(10)
        assert len(f.pending[('GET', '/api/update')]) == 2
        newer = f.pending[('GET', '/api/update')].pop(1)
        newest = dict(f.update, status=dict(f.update['status'], current='v1.0.9', update_ready=False))
        answer(newer, newest)
        f.page.wait_for_function("document.querySelector('#setting-update .update-facts')?.textContent.includes('v1.0.9')")
        f.release('/api/update', {'ok': False, 'error': 'fixture_old_read_failed'} if failed else f.update, status=503 if failed else 200)
        f.page.wait_for_timeout(70)
        assert 'v1.0.9' in f.page.locator('#setting-update .update-facts').inner_text(), 'older update read overwrote the newest running version'
        assert '读取失败' not in f.page.locator('#setting-update .update-notice').inner_text(), 'obsolete update read failure replaced newer state'
    finally:
        f.close()


def malformed_update_intent_recovers_without_manual_review(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.page.evaluate("sessionStorage.setItem('loomgate.unconfirmedUpdate','{broken-fixture')")
        f.page.locator('[data-action=update-status]').click()
        f.page.wait_for_timeout(70)
        assert not f.page.evaluate("sessionStorage.getItem('loomgate.unconfirmedUpdate')")
        assert f.page.locator('[data-action=update-review]').count() == 0
        assert f.page.locator('[data-action=update-apply]:visible').count() == 1
        assert not any(call == ('POST', '/api/update/apply') for call in f.requests)
    finally:
        f.close()

def update_storage_failure_uses_in_memory_single_submission(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.page.evaluate("window.fixtureStorageSet=sessionStorage.setItem.bind(sessionStorage);sessionStorage.setItem=function(name,value){if(name==='loomgate.unconfirmedUpdate')throw new Error('fixture blocked storage');return window.fixtureStorageSet(name,value)}")
        f.hold('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').click()
        f.wait_held('/api/update/apply', 'POST')
        f.page.locator('[data-action=update-apply]').evaluate("button=>button.dispatchEvent(new MouseEvent('click',{bubbles:true}))")
        f.page.wait_for_timeout(40)
        assert sum(call == ('POST', '/api/update/apply') for call in f.requests) == 1
        f.update['status']['state']='handover'
        f.release('/api/update/apply',{'ok':True},'POST')
        f.page.wait_for_function("document.querySelector('#setting-update').textContent.includes('自动切换中')")
        assert f.page.locator('[data-action=update-apply]').is_hidden()
        assert f.page.locator('#dialog[open]').count()==0
    finally:
        f.close()

def update_check_is_scoped_and_readback_is_truthful(browser, base):
    f = Fixture(browser, base)
    try:
        ready_update(f)
        f.hold('/api/update/check', 'POST')
        f.page.locator('[data-action=update-check]').click()
        request = f.wait_held('/api/update/check', 'POST').request.post_data_json
        assert request == {}
        f.release('/api/update/check', f.update, 'POST')
        f.page.wait_for_function("document.querySelector('#setting-update .update-notice')?.textContent.includes('检测到更新')")
        assert f.page.locator('[data-action=update-apply]:visible').count() == 1
        assert sum(call == ('POST', '/api/update/check') for call in f.requests) == 1
        assert f.page.evaluate("window.fixtureLoopCard===document.querySelector('#setting-loop')&&window.fixtureUpdateCard===document.querySelector('#setting-update')")
    finally:
        f.close()


def main():
    global PANEL
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', required=True)
    parser.add_argument('--assets', type=Path, default=PANEL)
    parser.add_argument('--case', action='append', default=[])
    args = parser.parse_args()
    PANEL = args.assets.resolve()
    if not all((PANEL / asset[0]).is_file() for asset in ASSETS.values()):
        raise SystemExit('synthetic fixture assets are incomplete')
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    server = assets_server()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    report = {'cases': [], 'external_network': False}
    try:
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            tests = [
                ('old_step_success_cannot_unlock_new_login', lambda: stale_connection_step(browser, base, False)),
                ('old_step_failure_cannot_pollute_new_login', lambda: stale_connection_step(browser, base, True)),
                ('late_plan_save_preserves_second_draft', lambda: late_plan_save(browser, base)),
                ('reversed_account_responses_preserve_scope', lambda: scope_switch(browser, base)),
                ('verified_flow_commits_once', lambda: commit_once(browser, base, False)),
                ('lost_commit_response_is_not_replayed', lambda: commit_once(browser, base, True)),
                ('key_toggle_preserves_endpoint_and_row', lambda: key_toggle(browser, base)),
                ('canceled_key_limits_do_not_replace_tasks', lambda: canceled_page_reads(browser, base, 'keys', '/api/key-limits')),
                ('canceled_migration_does_not_replace_tasks', lambda: canceled_page_reads(browser, base, 'migration', '/api/migration')),
                ('responsive_pages_keep_single_heading', lambda: responsive_pages(browser, base)),
                ('legacy_account_confirmation_keeps_intent', lambda: captured_account_intent(browser, base, 'workbuddy')),
                ('vault_account_confirmation_keeps_intent', lambda: captured_account_intent(browser, base, 'autoclaw')),
                ('stale_refresh_cannot_revert_saved_connection', lambda: commit_refresh_overlap(browser, base, True)),
                ('refresh_before_commit_keeps_save_receipt', lambda: commit_refresh_overlap(browser, base, False)),
                ('same_log_old_success_cannot_replace_new_output', lambda: same_log_overlap(browser, base, False)),
                ('same_log_old_failure_cannot_replace_new_output', lambda: same_log_overlap(browser, base, True)),
                ('late_key_result_preserves_new_draft', lambda: late_key_result(browser, base)),
                ('task_log_request_includes_provider_scope', lambda: provider_scoped_task_log(browser, base)),
                ('sequence_plan_run_preserves_selector_and_scope', lambda: sequence_plan_run_preserves_selector_and_scope(browser, base)),
                ('default_sequence_run_sends_only_visible_accounts', lambda: sequence_plan_run_preserves_selector_and_scope(browser, base, True)),
                ('sequence_switch_and_timing_preserve_selectors', lambda: sequence_plan_save_keeps_selectors(browser, base)),
                ('unread_sequence_scope_never_submits_all_accounts', lambda: sequence_plan_missing_scope_never_runs(browser, base)),
                ('late_key_edit_read_preserves_new_draft', lambda: late_key_edit_read(browser, base)),
                ('late_key_edit_read_failure_is_obsolete', lambda: late_key_edit_read(browser, base, True)),
                ('late_key_edit_save_preserves_new_draft_and_row', lambda: late_key_edit_save(browser, base)),
                ('key_edit_verifies_note_readback', lambda: mismatched_key_edit(browser, base, 'note')),
                ('key_edit_verifies_limits_readback', lambda: mismatched_key_edit(browser, base, 'limits')),
                ('late_key_delete_preserves_draft_and_endpoint', lambda: late_key_delete(browser, base)),
                ('late_key_copy_never_replaces_new_draft', lambda: late_key_copy(browser, base)),
                ('late_password_save_preserves_new_draft', lambda: late_password_save(browser, base)),
                ('late_lan_read_preserves_new_password_draft', lambda: late_lan_read(browser, base)),
                ('late_lan_save_preserves_new_password_draft', lambda: late_lan_save(browser, base)),
                ('current_key_copy_is_dialog_scoped', lambda: current_key_copy_is_displayed_only_in_its_dialog(browser, base)),
                ('same_key_mutations_are_serialized', lambda: key_mutations_on_same_record_are_serialized(browser, base)),
                ('password_session_mismatch_stays_unconfirmed', lambda: password_session_mismatch_stays_unconfirmed(browser, base)),
                ('lan_disable_is_local_and_clears_old_cidrs', lambda: lan_disabling_clears_cidrs_and_updates_only_its_card(browser, base)),
                ('key_creation_ack_preserves_existing_content', lambda: key_creation_ack_is_local(browser, base)),
                ('first_key_creation_preserves_endpoint_on_mobile', lambda: key_creation_ack_is_local(browser, base, True)),
                ('late_key_creation_sync_preserves_new_draft', lambda: late_key_creation_sync_keeps_draft(browser, base)),
                ('missing_created_key_readback_keeps_one_time_result', lambda: unconfirmed_key_creation_keeps_result(browser, base, 'missing')),
                ('mismatched_created_key_readback_stays_unconfirmed', lambda: unconfirmed_key_creation_keeps_result(browser, base, 'mismatch')),
                ('creation_readback_after_ack_preserves_new_draft', lambda: creation_readback_after_ack_keeps_new_draft(browser, base)),
                ('creation_readback_failure_keeps_one_time_result', lambda: creation_readback_failure_keeps_result(browser, base)),
                ('creation_readback_requires_returned_id_not_same_name', lambda: creation_requires_returned_identity(browser, base)),
                ('invalid_creation_receipt_preserves_result_without_false_verification', lambda: creation_requires_returned_identity(browser, base, True)),
                ('old_creation_readback_cannot_revert_revisited_page', lambda: creation_old_readback_cannot_revert_revisited_page(browser, base)),
                ('update_apply_is_one_click_and_scoped', lambda: update_apply_is_one_click_and_scoped(browser, base)),
                ('stale_idle_update_intent_recovers_automatically', lambda: stale_update_intent_recovers_automatically(browser, base)),
                ('stale_failed_update_intent_recovers_automatically', lambda: stale_update_intent_recovers_automatically(browser, base, True)),
                ('one_click_update_has_no_confirmation_form', lambda: one_click_update_has_no_confirmation_form(browser, base)),
                ('accepted_idle_update_reconnects_without_replay', lambda: accepted_idle_update_reconnects_without_replay(browser, base)),
                ('revisited_update_recovers_automatically', lambda: revisited_update_recovers_automatically(browser, base)),
                ('abandoned_update_preflight_never_sends', lambda: abandoned_update_preflight_never_sends(browser, base)),
                ('late_update_read_preserves_password_draft', lambda: late_update_read_keeps_password_draft(browser, base)),
                ('changed_update_target_is_not_applied_silently', lambda: update_target_change_is_not_applied_silently(browser, base)),
                ('lost_update_response_is_not_replayed', lambda: lost_update_response_is_not_replayed(browser, base)),
                ('late_update_acceptance_preserves_password_draft', lambda: late_update_acceptance_preserves_password_draft(browser, base)),
                ('zero_update_time_is_not_latest', lambda: update_zero_time_is_unchecked(browser, base)),
                ('update_completion_requires_running_version', lambda: update_completion_requires_running_version(browser, base)),
                ('unsigned_update_never_offers_apply', lambda: unsigned_update_has_no_apply_action(browser, base)),
                ('old_update_read_cannot_revert_newest_status', lambda: update_read_order_keeps_newest_status(browser, base)),
                ('old_update_read_error_cannot_replace_newest_status', lambda: update_read_order_keeps_newest_status(browser, base, True)),
                ('malformed_update_intent_recovers_without_manual_review', lambda: malformed_update_intent_recovers_without_manual_review(browser, base)),
                ('update_storage_failure_uses_in_memory_single_submission', lambda: update_storage_failure_uses_in_memory_single_submission(browser, base)),
                ('update_check_is_scoped_and_readback_truthful', lambda: update_check_is_scoped_and_readback_is_truthful(browser, base)),
            ]
            base = 'http://127.0.0.1:'+str(server.server_port)
            if args.case:
                available = {name for name, _ in tests}
                if set(args.case)-available:
                    raise ValueError('unknown requested browser case')
                tests = [(name, run) for name, run in tests if name in args.case]
            report['requested_cases'] = args.case or 'all'
            for name, run in tests:
                try:
                    run()
                    report['cases'].append({'name': name, 'passed': True})
                    print('PASS '+name, flush=True)
                except Exception as error:
                    report['cases'].append({'name': name, 'passed': False, 'error': str(error)[:1500]})
                    print('FAIL '+name+': '+type(error).__name__+': '+str(error)[:180], flush=True)
            browser.close()
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
        (output/'result.json').write_text(json.dumps(report, indent=2, ensure_ascii=False), encoding='utf-8')
    return 0 if report['cases'] and all(case['passed'] for case in report['cases']) else 1


if __name__ == '__main__':
    raise SystemExit(main())
