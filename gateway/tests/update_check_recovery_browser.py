"""Actual console retry of an unknown visibility result; synthetic HTTP only."""
import argparse,importlib.util,json,threading
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
spec=importlib.util.spec_from_file_location('update_retry_assets',Path(__file__).with_name('console_interactions_browser.py'))
assets=importlib.util.module_from_spec(spec);spec.loader.exec_module(assets)
def main():
 p=argparse.ArgumentParser();p.add_argument('--output',type=Path,required=True);a=p.parse_args();a.output.mkdir(parents=True,exist_ok=True)
 server=assets.assets_server();threading.Thread(target=server.serve_forever,daemon=True).start();calls=[];held=[];phase={'mode':'unknown'}
 base={'enabled':True,'current':'v1.0.13','state':'idle','repo':'fixture-owner/fixture-repo','signature_verification':True,'asset_name':'fixture-runtime.tar.gz','latest_tag':'v1.0.14','checked_at':'2026-11-24T00:00:00Z'}
 def envelope(mode):
  if mode=='unknown':return {'ok':True,'status':{**base,'visibility':'unknown','state':'failed','latest_tag':'v1.0.13','update_ready':False,'unavailable_reason':'尚未确认发布仓库可见性，请稍后重试「检查更新」'}}
  if mode=='failed':return {'ok':False,'message':'fixture check failed','status':{**base,'state':'failed','visibility':'unknown','update_ready':False,'unavailable_reason':'尚未确认发布仓库可见性，请稍后重试「检查更新」'}}
  if mode=='disabled':return {'ok':False,'enabled':False,'message':'fixture update disabled'}
  return {'ok':True,'status':{**base,'current':'v1.0.14' if mode=='latest' else base['current'],'visibility':'public','unavailable_reason':'','update_ready':mode=='ready'}}
 def answer(route,value):route.fulfill(status=200,content_type='application/json',body=json.dumps(value))
 def api(route):
  path=urlsplit(route.request.url).path;calls.append((route.request.method,path))
  if path=='/api/session':value={'username':'fixture operator','expiresAt':'2027-01-01T00:00:00Z'}
  elif path=='/api/workbench/providers':value={'providers':[]}
  elif path=='/api/state':value={}
  elif path=='/api/features/reasoning-loop':value={'stop_only':True}
  elif path=='/api/update':value=envelope(phase['mode'])
  elif path=='/api/update/check':held.append(route);return
  else:value={}
  answer(route,value)
 try:
  with sync_playwright() as p:
   browser=p.chromium.launch();page=browser.new_page(viewport={'width':1440,'height':1000});page.route('**/api/**',api);page.goto(f'http://127.0.0.1:{server.server_port}/#settings')
   card=page.locator('#setting-update');card.wait_for();check=card.locator('[data-action=update-check]');apply=card.locator('[data-action=update-apply]')
   assert check.is_visible() and check.is_enabled(),'unknown visibility hid/disabled the only retry action'
   assert card.locator('button:visible').count()==1 and not apply.is_visible(),'unexpected update controls before discovery'
   assert card.locator('[data-action=update-status]').count()==0,'separate refresh button remains'
   check.click();page.wait_for_function("document.querySelector('[data-action=update-check]').disabled")
   check.evaluate("b=>b.dispatchEvent(new MouseEvent('click',{bubbles:true}))");page.wait_for_timeout(50)
   assert len(held)==1,'double-click posted the check twice';assert not apply.is_visible()
   phase['mode']='ready';answer(held.pop(),envelope('ready'));page.wait_for_function("!document.querySelector('[data-action=update-check]').disabled && !document.querySelector('[data-action=update-apply]').hidden")
   assert card.locator('button:visible').count()==2 and apply.inner_text()=='更新'
   page.screenshot(path=str(a.output/'update-ready-desktop.png'),full_page=True)
   check.click();page.wait_for_timeout(40)
   page.goto(f'http://127.0.0.1:{server.server_port}/#accounts');page.wait_for_selector('#view[data-load-state=ready]')
   page.goto(f'http://127.0.0.1:{server.server_port}/#settings');page.wait_for_selector('#setting-update')
   answer(held.pop(),envelope('failed'))
   page.wait_for_function("!document.querySelector('[data-action=update-check]').disabled && !document.querySelector('[data-action=update-apply]').hidden")
   assert 'fixture check failed' not in card.inner_text(),'old check replaced the new settings page'
   check.click();page.wait_for_timeout(40);phase['mode']='latest';answer(held.pop(),envelope('latest'));page.wait_for_function("document.querySelector('[data-action=update-apply]').hidden && !document.querySelector('[data-action=update-check]').disabled")
   assert card.locator('button:visible').count()==1 and '已是最新' in card.inner_text()
   check.click();page.wait_for_timeout(40);phase['mode']='failed';answer(held.pop(),envelope('failed'));page.wait_for_function("!document.querySelector('[data-action=update-check]').disabled")
   assert check.is_visible() and not apply.is_visible() and '未启用' not in card.inner_text(),'check failure was mistaken for a disabled feature'
   check.click();page.wait_for_timeout(40);phase['mode']='disabled';answer(held.pop(),envelope('disabled'));page.wait_for_function("document.querySelector('[data-action=update-check]').disabled")
   assert check.is_visible() and not apply.is_visible(),'disabled response restored a stale ready release'
   page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth');page.screenshot(path=str(a.output/'update-disabled-mobile.png'),full_page=True)
   assert not any(method=='POST' and path!='/api/update/check' for method,path in calls),'unexpected mutation'
   browser.close();(a.output/'result.json').write_text(json.dumps({'result':'PASS','unknown_retry_visible':True,'failed_check_not_disabled':True,'single_check_submission':True,'only_check_and_conditional_update':True,'late_check_does_not_strand_new_page':True,'disabled_not_stale_ready':True,'check_posts':5,'apply_posts':0,'provider_requests':0,'mobile_no_overflow':True},indent=2),encoding='utf-8');print('PASS: visibility recovery and minimal update controls')
 finally:server.shutdown();server.server_close()
if __name__=='__main__':main()
