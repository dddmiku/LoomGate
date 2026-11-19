"""Responsive layout acceptance against the synthetic console runtime only."""
import argparse,json,sys,traceback
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
from console_actions import choose_provider, choose_account, choose_capability, reveal_filter, select_task, plan_timing, plan_times, plan_review
p=argparse.ArgumentParser();p.add_argument('--runtime',required=True);p.add_argument('--output',required=True);a=p.parse_args()
root=Path(a.runtime).resolve();out=Path(a.output).resolve()
if root.name!='catalog-fixture' or root.drive.upper()!='D:' or out.drive.upper()!='D:':raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True,exist_ok=True)
report={'pages':0,'actions':[],'screenshots':[],'page_errors':[],'console_errors':[],'external_requests':[]};stage='start'

def main():
 global stage
 with sync_playwright() as pw:
  browser=pw.chromium.launch(headless=True);page=browser.new_page(viewport={'width':1440,'height':1000});base='http://127.0.0.1:17863'
  page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__))
  page.on('console',lambda m:report['console_errors'].append('console_error') if m.type=='error' else None)
  page.on('request',lambda r:report['external_requests'].append('external') if urlsplit(r.url).hostname not in (None,'127.0.0.1') else None)
  assert page.request.get(base+'/__fixture/stats').status==200
  page.request.post(base+'/__fixture/mode/success')
  def bounds():
   assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1'),'page_overflow'
  def shot(name):
   page.locator('h1').first.click();page.screenshot(path=str(out/name),full_page=True,animations='disabled',mask=[page.locator('input[type=password]')]);report['screenshots'].append(name)
  def nav(target_route):
   if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
   page.locator('#navigation a[href="#'+target_route+'"]').click()
   page.locator('#view > .loading').wait_for(state='detached')
   page.wait_for_function('(target_route)=>document.querySelector("#navigation a[aria-current=page]")?.hash==="#"+target_route',arg=target_route)
   if target_route=='models':page.locator('[data-action=model-refresh]:not([disabled])').wait_for()
   if target_route=='accounts':page.locator('#account-list.loading').wait_for(state='detached')
   if target_route=='tasks':page.locator('#task-list.loading').wait_for(state='detached')
  fields=dict(line.split(':',1) for line in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
  page.goto(base+'/admin/');page.locator('#login-submit').wait_for();bounds();shot('layout-login-desktop.png')
  page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-overview]').wait_for()
  for width in (1440,1024,768,390,320):
   page.set_viewport_size({'width':width,'height':1000 if width>900 else 844})
   for route in ('overview','providers','accounts','models','tasks','usage','logs','keys','migration','settings'):
    stage=f'{width}:{route}';nav(route);bounds();report['pages']+=1
    if width<=900:assert not page.locator('#navigation').is_visible()
    assert page.locator('.hero,.metrics,.card,.eyebrow').count()==0
    if route in ('tasks','models') and page.locator('.selection-row').count():
     page.locator('.selection-row').first.click();page.locator('.detail-pane:visible').wait_for();bounds()
     if width<=700:
      assert not page.locator('.selection-pane').is_visible()
      page.locator('.detail-back:visible').click();assert page.locator('.selection-pane').is_visible()
    if (width,route) in ((1440,'overview'),(1440,'models'),(390,'models'),(390,'overview'),(1024,'providers')):shot(f'layout-{width}-{route}.png')
   if width<=900:
    page.locator('#navigation-toggle').click();links=page.locator('#navigation a')
    assert links.count()==len(('overview','providers','accounts','models','tasks','usage','logs','keys','migration','settings'))
    sidebar=page.locator('#sidebar').bounding_box()
    assert sidebar['x']==0 and sidebar['height']>=page.viewport_size['height']-sidebar['y']-1
    for link in links.all():
     b=link.bounding_box();assert b and b['x']>=0 and b['x']+b['width']<=width+1 and b['height']>=35
    bounds()
    if width==390:page.screenshot(path=str(out/'layout-mobile-navigation.png'),full_page=False);report['screenshots'].append('layout-mobile-navigation.png')
    page.keyboard.press('Escape');assert not page.locator('#navigation').is_visible()
    assert page.locator('#navigation-toggle').evaluate('(e)=>document.activeElement===e')
    assert not page.locator('#main').evaluate('(e)=>e.inert')
   nav('models')
   if width<=700:
    assert page.locator('#model-account').is_visible()
    assert page.locator('#model-account').is_visible() and page.locator('#model-capability').is_visible();bounds()
   stage=f'{width}:plan-dialog';nav('tasks')
   if page.locator('#task-provider [data-provider-choice=workbuddy]').get_attribute('aria-pressed')!='true':
    choose_provider(page,'#task-provider','workbuddy')
   page.locator('[data-plan-row] [data-action=edit-plan-time]').first.wait_for()
   page.locator('[data-plan-row] [data-action=edit-plan-time]').first.click();page.locator('#plan-save').wait_for()
   plan_times(page,['08:20','18:20'])
   bounds();assert page.locator('#dialog').evaluate('(d)=>d.scrollWidth<=d.clientWidth+1')
   assert page.locator('#plan-save').is_visible();page.locator('#plan-save').scroll_into_view_if_needed()
   page.locator('#plan-enabled-quick').scroll_into_view_if_needed()
   title=page.locator('#dialog-title').bounding_box();assert title and title['y']>=0 and title['y']+title['height']<page.viewport_size['height']
   b=page.locator('#plan-save').bounding_box();assert b['x']>=0 and b['x']+b['width']<=width
   if width==390:page.screenshot(path=str(out/'layout-mobile-plan-dialog.png'),full_page=False);report['screenshots'].append('layout-mobile-plan-dialog.png')
   page.keyboard.press('Escape');assert not page.locator('#dialog').is_visible()
  report['actions'].append('all_pages_5_widths_navigation_filters_and_dialogs')
  # The drawer owns keyboard focus, survives short viewports, and never leaves
  # the content inert after a route change or desktop/mobile resize.
  stage='drawer-accessibility';page.set_viewport_size({'width':390,'height':568})
  page.locator('#navigation-toggle').click()
  assert page.locator('#sidebar').get_attribute('aria-modal')=='true'
  assert page.locator('.masthead').evaluate('(e)=>e.inert')
  page.locator('#sidebar .brand').focus();page.keyboard.press('Shift+Tab')
  assert page.locator('#logout').evaluate('(e)=>document.activeElement===e')
  page.keyboard.press('Tab');assert page.locator('#sidebar .brand').evaluate('(e)=>document.activeElement===e')
  for _ in range(20):
   page.keyboard.press('Tab')
   assert page.locator('#sidebar').evaluate('(e)=>e.contains(document.activeElement)')
  page.locator('#navigation a[href="#settings"]').click()
  assert not page.locator('#navigation').is_visible()
  assert not page.locator('#main').evaluate('(e)=>e.inert')
  page.locator('#navigation-toggle').click();page.locator('#theme').click()
  assert page.locator('html').get_attribute('data-theme')=='dark'
  assert page.locator('#theme-label').inner_text()=='浅色模式'
  page.locator('#theme').click();page.locator('#navigation-close').click()
  assert page.locator('#navigation-toggle').evaluate('(e)=>document.activeElement===e')
  page.locator('#navigation-toggle').click();page.mouse.click(370,200)
  assert not page.locator('#navigation').is_visible()
  page.locator('#navigation-toggle').click();page.locator('#navigation-close').focus()
  page.set_viewport_size({'width':1024,'height':768})
  page.wait_for_function('!document.querySelector("main").inert')
  assert page.locator('#sidebar').get_attribute('aria-modal') is None
  assert page.locator('#navigation a[aria-current=page]').evaluate('(e)=>document.activeElement===e')
  page.set_viewport_size({'width':390,'height':568})
  page.wait_for_function('document.activeElement===document.querySelector("#navigation-toggle")')
  page.locator('#navigation-toggle').click();page.keyboard.press('Escape')
  report['actions'].append('drawer_focus_theme_close_backdrop_and_breakpoint_recovery')
  stage='desktop-short-height';page.set_viewport_size({'width':1280,'height':600})
  nav('models');page.locator('#navigation a[href="#settings"]').scroll_into_view_if_needed()
  nav('accounts');page.evaluate('window.scrollTo(0,document.documentElement.scrollHeight)')
  # 侧栏在短视口下整体可见（sticky 顶部或页面不足一屏时跟随文档顶部），
  # 且底部控制按钮不能被裁掉。
  bar=page.locator('#sidebar').bounding_box()
  assert bar and abs(bar['y'])<=1 and bar['y']+bar['height']<=page.viewport_size['height']+1,bar
  assert page.locator('#theme').bounding_box()['y']>=0
  page.locator('#navigation a[href="#tasks"]').click()
  page.locator('[data-plan-row]').first.wait_for()
  bounds();report['actions'].append('desktop_sidebar_independent_scroll_and_bottom_controls')
  stage='dark-contrast';page.set_viewport_size({'width':1440,'height':1000});nav('tasks')
  def luminance(color):
   values=[int(x) / 255 for x in color[color.index('(')+1:color.index(')')].split(',')[:3]]
   values=[x/12.92 if x<=.04045 else ((x+.055)/1.055)**2.4 for x in values]
   return sum(x*y for x,y in zip(values,(.2126,.7152,.0722)))
  for theme in ('light','dark'):
   if page.locator('html').get_attribute('data-theme')!=theme:page.locator('#theme').click()
   button=page.locator('[data-plan-row] [data-action=toggle-schedule]').first;button.hover()
   colors=button.evaluate('(e)=>{const s=getComputedStyle(e);return [s.color,s.backgroundColor]}')
   lo,hi=sorted(luminance(x) for x in colors);assert (hi+.05)/(lo+.05)>=4.5
   if theme=='dark':shot('layout-tasks-dark.png')
  page.locator('#theme').click();report['actions'].append('primary_hover_contrast_light_and_dark')
  browser.close()
 assert not report['page_errors'] and not report['console_errors'] and not report['external_requests']
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',stage=stage,error_type=type(e).__name__,error_message=str(e)[:500],source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('console_layout.py')])
(out/'layout-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
print(json.dumps({k:report[k] for k in ('result','pages','stage','error_type','source_lines') if k in report}))
sys.exit(0 if report['result']=='PASS' else 1)
