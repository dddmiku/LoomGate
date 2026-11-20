"""Login drawer regressions using the real console and synthetic loopback APIs.

No real authentication, SMS or account storage is used.
"""
import argparse
import importlib.util
import json
from pathlib import Path
import threading
from urllib.parse import urlsplit

from playwright.sync_api import sync_playwright

_spec = importlib.util.spec_from_file_location("console_login_fixture", Path(__file__).with_name("console_interactions_browser.py"))
_fixture = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_fixture)
Fixture, answer, assets_server = _fixture.Fixture, _fixture.answer, _fixture.assets_server


class LoginFixture(Fixture):
    def __init__(self, browser, base):
        super().__init__(browser, base)
        self.login_requests = []
        self.reject_sms = False
        self.drop_sms_response = False
        self.providers = [dict(id=provider, methods=[dict(id=method, name=method, available=True)])
                          for provider, method in [('workbuddy', 'qr'), ('trae', 'callback'),
                                                   ('doubao', 'qr'), ('autoclaw', 'sms')]]

    def route(self, route):
        request = route.request
        path = urlsplit(request.url).path
        if path == '/api/connections/providers':
            answer(route, {'providers': self.providers})
        elif path == '/api/connections/start':
            self.starts += 1
            body = request.post_data_json
            self.login_requests.append(body)
            self.latest_flow = dict(self.flow('fixture-flow-'+str(self.starts)), **body)
            self.latest_flow['status'] = self.start_status
            if self.start_status == 'waiting_phone':
                self.latest_flow.update(input_mode='phone', can_verify=False)
            answer(route, {'flow': self.latest_flow})
        elif path == '/api/connections/step' and request.post_data_json.get('action') == 'send_code':
            body = request.post_data_json
            self.login_requests.append(body)
            if self.drop_sms_response:
                route.abort()
            elif self.reject_sms:
                answer(route, {'error': 'authentication_busy'}, 409)
            else:
                self.latest_flow.update(status='waiting_code', input_mode='code')
                answer(route, {'flow': self.latest_flow})
        else:
            super().route(route)


def add_another(browser, base, provider):
    f = LoginFixture(browser, base)
    try:
        f.start_status = 'verified'
        f.open_accounts()
        f.open_connection()
        f.page.locator('#connection-provider input[value="'+provider+'"]').check()
        if provider == 'workbuddy':
            f.page.locator('#connection-realm input[value=global]').check()
        f.page.locator('#connection-begin').click()
        f.page.locator('#connection-done').wait_for()
        another = f.page.locator('#connection-add-another')
        assert another.count() == 1, 'successful login must offer a repeat-add action in its drawer'
        assert another.inner_text() == '再添加一个'
        f.start_status = 'waiting_phone'
        another.click()
        f.page.locator('#connection-begin').wait_for()
        assert f.page.locator('#dialog').is_visible()
        assert f.page.locator('#connection-provider input:checked').input_value() == provider
        assert f.page.locator('#connection-method input:checked').input_value() == f.login_requests[0]['method']
        if provider == 'workbuddy':
            assert f.page.locator('#connection-realm input:checked').input_value() == 'global'
        assert f.starts == 1, 'repeat-add should wait for an explicit new login submission'
        f.page.locator('#connection-begin').click()
        f.page.locator('#connection-sensitive').wait_for()
        assert f.starts == 2
        assert not f.login_requests[-1].get('account_id'), 'repeat-add must create a new flow without a relogin target'
    finally:
        f.close()


def empty_invite(browser, base, rejected=False, dropped=False):
    f = LoginFixture(browser, base)
    try:
        f.start_status = 'waiting_phone'
        f.reject_sms = rejected
        f.drop_sms_response = dropped
        f.open_accounts()
        f.open_connection()
        f.page.locator('#connection-provider input[value=autoclaw]').check()
        f.page.locator('#connection-begin').click()
        phone = f.page.locator('#connection-sensitive input[name=phone]')
        synthetic_phone = '1' + '0' * 10
        phone.fill(synthetic_phone)
        f.page.locator('#connection-sensitive input[name=invite_code]').focus()
        f.page.locator('#connection-sensitive button').click()
        if rejected or dropped:
            f.page.locator('#connection-error:not([hidden])').wait_for()
            assert phone.input_value() == synthetic_phone, 'a rejected SMS request must keep the phone draft for correction'
            if rejected:
                assert '未受理' in f.page.locator('#connection-error').inner_text(), 'a definite server rejection must not be described as an unknown operation'
            else:
                assert '未确认' in f.page.locator('#connection-error').inner_text(), 'a lost response must not claim no SMS was sent'
        else:
            f.page.locator('#connection-sensitive input[name=code]').wait_for()
            assert f.latest_flow['status'] == 'waiting_code'
        sends = [body for body in f.login_requests if body.get('action') == 'send_code']
        assert len(sends) == 1, 'focus on an optional empty field must not duplicate SMS requests'
        assert sends[0]['input']['phone'] == synthetic_phone
        assert 'invite_code' not in sends[0]['input'], 'empty optional invite fields should be omitted from the request'
    finally:
        f.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    if output.is_relative_to(Path(__file__).resolve().parents[2]):
        parser.error('evidence must be outside the source tree')
    output.mkdir(parents=True, exist_ok=True)
    server = assets_server()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    results = []
    try:
        with sync_playwright() as pw:
            browser = pw.chromium.launch(headless=True)
            base = 'http://127.0.0.1:'+str(server.server_port)
            cases = [('repeat_add_'+provider, lambda p=provider: add_another(browser, base, p))
                     for provider in ['workbuddy', 'trae', 'doubao', 'autoclaw']]
            cases += [('optional_empty_invite', lambda: empty_invite(browser, base)),
                      ('sms_rejection_preserves_draft', lambda: empty_invite(browser, base, True)),
                      ('sms_unknown_preserves_draft_without_replay', lambda: empty_invite(browser, base, dropped=True))]
            for name, run in cases:
                try:
                    run()
                    results.append(dict(name=name, passed=True))
                    print('PASS '+name, flush=True)
                except Exception as error:
                    results.append(dict(name=name, passed=False, error=type(error).__name__))
                    print('FAIL '+name+': '+type(error).__name__, flush=True)
            browser.close()
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
        (output/'result.json').write_text(json.dumps(results, indent=2), encoding='utf-8')
    return 0 if results and all(result['passed'] for result in results) else 1


if __name__ == '__main__':
    raise SystemExit(main())
