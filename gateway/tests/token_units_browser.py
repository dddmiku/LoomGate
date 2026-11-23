"""Render both consoles with synthetic large Token counts on loopback only."""
import argparse
import json
from pathlib import Path
import sys
import threading
from urllib.parse import urlsplit

from playwright.sync_api import expect, sync_playwright

sys.path.insert(0, str(Path(__file__).resolve().parent))
from console_interactions_browser import ASSETS, Fixture, answer, assets_server

TOTALS = dict(requests=12345, prompt_tokens=1234567890, completion_tokens=999999,
              cached_tokens=234567890, total_tokens=1235567889, credit=12345.67,
              failed_requests=0, unreported_requests=0)
USAGE = dict(ok=True, since='2026-11-01T00:00:00Z', totals=TOTALS, days=[], keys=[
    dict(key_id='fixture-key', name='合成调用方', totals=TOTALS, days=[],
         models=[dict(model='fixture:large', totals=TOTALS)])])
REQUEST = dict(request_id='fixture-large', started_at='2026-11-21T01:00:00Z',
               model='fixture:large', protocol='responses', key_name='合成调用方',
               status='success', http_status=200, input_tokens=1234567890,
               output_tokens=1234567, cached_tokens=999999, reasoning_tokens=0,
               duration_ms=1000, ttfb_ms=100, queue_ms=0, credit=12345.67,
               upstream_started=True, usage_state='complete', attempt_count=0)


class TokenFixture(Fixture):
    def __init__(self, browser, base):
        super().__init__(browser, base)
        self.key.update(name='合成调用方', total_tokens=1234567890)

    def route(self, route):
        path=urlsplit(route.request.url).path
        if path=='/api/usage':
            answer(route, USAGE)
        elif path=='/api/keys' and route.request.method=='GET':
            zero=dict(self.key, id='fixture-zero', name='零值调用方', total_tokens=0)
            missing={k:v for k,v in dict(self.key, id='fixture-missing', name='未知调用方').items() if k!='total_tokens'}
            answer(route, dict(ok=True, keys=[self.key,zero,missing]))
        elif path=='/api/requests':
            zero=dict(REQUEST, request_id='fixture-zero',input_tokens=0,cached_tokens=0,output_tokens=None)
            missing=dict(REQUEST, request_id='fixture-missing',input_tokens=None,cached_tokens=None,output_tokens=0)
            answer(route, dict(ok=True,total=3,offset=0,limit=20,items=[REQUEST,zero,missing]))
        elif path=='/api/requests/facets':
            answer(route,dict(ok=True,keys=[],models=[]))
        elif path=='/api/logs':
            answer(route,dict(ok=True,rows=[dict(seq=1,time='01:00:00',model='fixture:large',mode='stream',
                status='ok',key='合成调用方',uid='fixture',ttfb='0.1s',
                **{'in':'1234567890','hit':'999999','cache':'prev=1234567890','tok':'1234567','rate':'1234567','total':'1s'})]))
        elif path=='/api/tasks':
            answer(route,dict(ok=True,groups=[]))
        else:
            super().route(route)


def token(node, visible, precise):
    expect(node).to_have_text(visible)
    expect(node.locator('[title]')).to_have_attribute('title',f'{precise:,} tokens')


def exercise(browser,base,output):
    f=TokenFixture(browser,base)
    page=f.page
    cases=[]
    try:
        page.goto(base+'/#overview')
        expect(page.locator('[data-workspace-overview]')).to_be_visible()
        assert page.locator('[data-workspace-overview] [title$=" tokens"]').count()==0
        cases.append('overview_has_no_token_metric_or_changed_credit_semantics')
        page.screenshot(path=str(output/'token-overview.png'),full_page=True)

        page.locator('#navigation a[href="#usage"]').click()
        tile=page.locator('article.usage-tile').filter(has=page.locator('span',has_text='总 Token'))
        token(tile.locator('strong'),'1.24B',1235567889)
        expect(tile.locator('p')).to_have_text('输入 1.23B / 输出 1M / 缓存 234.57M')
        expect(page.locator('[data-total-requests]')).to_have_text('12,345')
        expect(page.locator('article.usage-tile').filter(has=page.locator('span',has_text='已报告消耗')).locator('strong')).to_have_text('12,345.67')
        token(page.locator('.donut-legend tbody td').nth(1),'1.24B',1235567889)
        details=page.locator('section.usage-card').filter(has=page.get_by_role('heading',name='按模型明细'))
        token(details.locator('tbody td').nth(2),'1.23B',1234567890)
        token(details.locator('tbody td').nth(3),'1M',999999)
        token(details.locator('tbody td').nth(4),'234.57M',234567890)
        callers=page.locator('section.usage-card').filter(has=page.get_by_role('heading',name='调用方',exact=True))
        token(callers.locator('tbody td').nth(2),'1.24B',1235567889)
        cases.append('usage_tiles_model_caller_and_detail_counts_keep_exact_titles')
        page.screenshot(path=str(output/'token-usage.png'),full_page=True)

        page.locator('#navigation a[href="#logs"]').click()
        row=page.locator('[data-request-row="0"]')
        token(row.locator('td').nth(6),'1.23B',1234567890)
        token(row.locator('td').nth(7),'1M',999999)
        token(row.locator('td').nth(8),'1.23M',1234567)
        # Throughput excludes the 100ms first-frame wait from the 1000ms total;
        # it stays an exact numeric rate, never a compact token-count label.
        expect(row.locator('td').nth(9)).to_have_text('1371741.1 tok/s')
        token(page.locator('[data-request-row="1"] td').nth(6),'0',0)
        expect(page.locator('[data-request-row="1"] td').nth(8)).to_have_text('—')
        expect(page.locator('[data-request-row="2"] td').nth(6)).to_have_text('—')
        token(page.locator('[data-request-row="2"] td').nth(8),'0',0)
        row.locator('[data-action=request-expand]').click()
        expect(page.locator('.log-detail')).to_be_visible()
        cases.append('request_rows_keep_zero_missing_rate_and_expand_behavior')
        page.screenshot(path=str(output/'token-logs.png'),full_page=True)

        page.locator('#navigation a[href="#keys"]').click()
        token(page.locator('tbody tr').filter(has=page.locator('[data-key-row="fixture-key"]')).locator('td').nth(5),'1.23B',1234567890)
        token(page.locator('tbody tr').filter(has=page.locator('[data-key-row="fixture-zero"]')).locator('td').nth(5),'0',0)
        expect(page.locator('tbody tr').filter(has=page.locator('[data-key-row="fixture-missing"]')).locator('td').nth(5)).to_have_text('—')
        cases.append('key_counts_keep_exact_titles_zero_and_missing')
        page.screenshot(path=str(output/'token-keys.png'),full_page=True)

        page.goto(base+'/legacy#usage')
        legacy_total=page.locator('#usageTiles .usage-tile').filter(has=page.locator('.k',has_text='合计 tokens')).locator('.v')
        expect(legacy_total).to_have_text('1.24B')
        expect(legacy_total).to_have_attribute('title','1,235,567,889 tokens')
        token(page.locator('#usageRows [data-l="输入"]'),'1.23B',1234567890)
        token(page.locator('#usageRows [data-l="合计"]'),'1.24B',1235567889)
        page.locator('[data-usage-toggle]').click()
        model_tokens=page.locator('.usage-models [title$=" tokens"]')
        expect(model_tokens).to_have_text('1.24B')
        expect(model_tokens).to_have_attribute('title','1,235,567,889 tokens')
        cases.append('legacy_usage_tiles_rows_and_model_breakdown')
        page.screenshot(path=str(output/'token-legacy-usage.png'),full_page=True)

        page.locator('button[data-view="keys"]').first.click()
        token(page.locator('.key-tokens .key-usage b').first,'1.23B',1234567890)
        cases.append('legacy_key_counts')

        page.locator('button[data-view="logs"]').first.click()
        log=page.locator('#logRows tr').first
        expect(log.locator('td').nth(8)).to_have_text('1.23B')
        expect(log.locator('td').nth(8)).to_have_attribute('title','输入 tokens（每轮重发整段上下文）：1,234,567,890')
        expect(log.locator('td').nth(9)).to_have_text('1M')
        token(log.locator('td').nth(11),'1.23M',1234567)
        expect(log.locator('td').nth(12)).to_have_text('1234567')
        cases.append('legacy_logs_input_cache_output_with_rate_unchanged')

        page.locator('button[data-view="requests"]').first.click()
        request=page.locator('#requestRows [data-request-id="fixture-large"]')
        token(request.locator('td').nth(4).locator('.requests-metric'),'1.23B',1234567890)
        token(request.locator('td').nth(5).locator('.requests-metric'),'1.23M',1234567)
        cases.append('legacy_request_metrics')
        page.screenshot(path=str(output/'token-legacy-requests.png'),full_page=True)

        assert not f.external, 'external_request'
        assert not f.errors, f.errors
        return dict(result='PASS',cases=cases,fixture_only=True,page_errors=f.errors,external_requests=f.external)
    finally:
        f.close()


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--output',required=True)
    args=parser.parse_args()
    output=Path(args.output).resolve()
    if output.drive.upper()!='D:':
        raise SystemExit('D_drive_evidence_required')
    output.mkdir(parents=True,exist_ok=True)
    ASSETS['/legacy']=('index.html','text/html')
    server=assets_server()
    thread=threading.Thread(target=server.serve_forever,daemon=True)
    thread.start()
    try:
        with sync_playwright() as pw:
            browser=pw.chromium.launch(headless=True)
            try:
                report=exercise(browser,f'http://127.0.0.1:{server.server_port}',output)
            finally:
                browser.close()
        (output/'token-units-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
        print(json.dumps(dict(result=report['result'],cases=len(report['cases']),page_errors=len(report['page_errors']),external_requests=len(report['external_requests']))))
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


if __name__=='__main__':
    main()
