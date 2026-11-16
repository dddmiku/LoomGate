"""Owned official-client lifecycle. Private JSON pipes, no credentials exported.

Login happens in the official window. The journal is written before opening it,
so retries and gateway restarts only inspect an existing login attempt.
"""
import json
import os
import re
import socket
import subprocess
import sys
import time
from contextlib import contextmanager
from pathlib import Path
from urllib.parse import urlsplit


class Failure(Exception):
    pass


def atomic(path, value):
    pending = path.with_suffix('.pending')
    with pending.open('w', encoding='utf-8') as f:
        json.dump(value, f)
        f.flush()
        os.fsync(f.fileno())
    pending.replace(path)


@contextmanager
def exclusive(root):
    import msvcrt
    with (root/'client.lock').open('a+b') as f:
        if f.tell() == 0:
            f.write(b'0')
            f.flush()
        f.seek(0)
        try:
            msvcrt.locking(f.fileno(), msvcrt.LK_NBLCK, 1)
        except OSError:
            raise Failure('desktop_busy') from None
        try:
            yield
        finally:
            f.seek(0)
            msvcrt.locking(f.fileno(), msvcrt.LK_UNLCK, 1)


def load(root):
    path = root/'client.json'
    if not path.exists():
        return None
    if path.stat().st_size > 32768:
        raise Failure('desktop_management_invalid')
    try:
        data = json.loads(path.read_text(encoding='utf-8'))
        if data['version'] != 1 or not 1 <= data['port'] <= 65535:
            raise ValueError()
        if Path(data['profile']) != root/'profile' or not Path(data['executable']).is_absolute():
            raise ValueError()
        return data
    except (KeyError, ValueError, TypeError):
        raise Failure('desktop_management_invalid') from None


def find_owned(state):
    import psutil
    matches = []
    def owned(p):
        if Path(p.exe()).resolve()!=Path(state['executable']).resolve():return False
        args=p.cmdline()
        if '--user-data-dir='+state['profile'] not in args or any(a.startswith('--type=') for a in args):return False
        if '--remote-debugging-port='+str(state['port']) not in args or '--remote-debugging-address=127.0.0.1' not in args:raise Failure('desktop_management_conflict')
        return True
    if state.get('pid') and state.get('created'):
        try:
            cached=psutil.Process(state['pid'])
            if cached.create_time()==state['created'] and owned(cached):return cached
        except (psutil.NoSuchProcess,psutil.AccessDenied):pass
    # Query expensive command lines only for matching executable names. Reading
    # every process command line can exceed the whole connection deadline.
    for p in psutil.process_iter(['pid', 'name']):
        try:
            if (p.info['name'] or '').lower()!=Path(state['executable']).name.lower():continue
            if owned(p):matches.append(p)
        except (psutil.NoSuchProcess, psutil.AccessDenied):
            continue
    if len(matches) > 1:
        raise Failure('desktop_management_conflict')
    return matches[0] if matches else None


def port_owner(state, process):
    import psutil
    try:
        listeners = [c for c in psutil.net_connections(kind='tcp') if c.status == psutil.CONN_LISTEN and c.laddr.port == state['port']]
        if not listeners:
            return False
        if any(c.pid != process.pid or c.laddr.ip != '127.0.0.1' for c in listeners):
            raise Failure('desktop_management_conflict')
        return True
    except psutil.AccessDenied:
        raise Failure('desktop_management_unavailable') from None


def discover_executable(explicit):
    candidate = Path(explicit) if explicit else Path(os.environ.get('LOCALAPPDATA', ''))/'Doubao/Application/app/Doubao.exe'
    if not candidate.is_absolute() or not candidate.is_file() or candidate.is_symlink() or candidate.name.lower() != 'doubao.exe':
        raise Failure('desktop_not_installed')
    return str(candidate.resolve())


def ensure(root, explicit, start):
    state = load(root)
    if state is None:
        if not start:
            raise Failure('desktop_unavailable')
        exe = discover_executable(explicit)
        with socket.socket() as probe:
            probe.bind(('127.0.0.1', 0))
            port = probe.getsockname()[1]
        profile = root/'profile'
        if profile.exists() and any(profile.iterdir()):
            raise Failure('desktop_management_conflict')
        profile.mkdir(exist_ok=True)
        state = {'version':1, 'executable':exe, 'profile':str(profile), 'port':port, 'launch':'prepared'}
        atomic(root/'client.json', state)
    elif explicit and Path(explicit).resolve() != Path(state['executable']).resolve():
        raise Failure('desktop_management_conflict')
    process = find_owned(state)
    if process is None:
        if not start:
            raise Failure('desktop_unavailable')
        with socket.socket() as probe:
            if probe.connect_ex(('127.0.0.1', state['port'])) == 0:
                raise Failure('desktop_management_conflict')
        discover_executable(state['executable'])
        state['launch'] = 'prepared'
        atomic(root/'client.json', state)
        subprocess.Popen([state['executable'], '--user-data-dir='+state['profile'],
                          '--remote-debugging-address=127.0.0.1', '--remote-debugging-port='+str(state['port']),
                          '--no-first-run', '--no-default-browser-check'],
                         cwd=str(Path(state['executable']).parent), stdin=subprocess.DEVNULL,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=subprocess.CREATE_NO_WINDOW)
    deadline = time.monotonic() + (16 if start else 1)
    while time.monotonic() < deadline:
        process = find_owned(state)
        if process and port_owner(state, process):
            state.update(pid=process.pid, created=process.create_time(), launch='running')
            atomic(root/'client.json', state)
            return state
        time.sleep(.2)
    raise Failure('desktop_timeout')


def login_shape(page):
    # Return booleans only. Input values, QR content and displayed private text
    # never leave the official renderer.
    return page.evaluate('''()=>{const text=document.body?.innerText||'';const visible=e=>{const r=e.getBoundingClientRect(),s=getComputedStyle(e);return r.width>0&&r.height>0&&s.display!=='none'&&s.visibility!=='hidden'};return {
      sms:!!document.querySelector('input[type=tel],input[autocomplete=tel],input[placeholder*="手机号"]'),
      code:!!document.querySelector('input[placeholder*="验证码"]')||/重新获取|重新发送|秒后/.test(text),
      expired:/二维码已过期|二维码失效/.test(text),
      scanned:/扫码成功|请在手机上确认|已扫码/.test(text),
      manual:/安全验证|滑动验证|拖动滑块|完成验证|身份验证/.test(text)||[...document.querySelectorAll('[data-testid="captcha"],#captcha_container,[class*="captcha_verify_container"]')].some(visible),
      qr:[...document.querySelectorAll('img,canvas,svg')].some(e=>{const b=e.getBoundingClientRect();return b.width>=120&&b.height>=120}),
      error:/验证码错误|验证码不正确|验证码已失效|验证码已过期/.test(text)
    }}''')


def login(root, state, action, nonce, method):
    from playwright.sync_api import sync_playwright
    entry = state.get('login')
    if entry and time.time()>=entry.get('deadline',0) and entry['status'] not in ('verified','canceled'):
        entry['status']='expired'
        atomic(root/'client.json',state)
    if entry and entry['nonce'] != nonce and entry['status'] not in ('canceled','expired','verified'):
        raise Failure('desktop_busy')
    # An explicit resume may finish a start that failed before the durable
    # opening marker. A present marker always forbids reopening the login.
    starting = action == 'login_start' or action == 'login_resume' and entry is None
    if not starting and (not entry or entry['nonce'] != nonce):
        raise Failure('desktop_login_attempt_missing')
    if entry and entry['nonce']==nonce:
        if entry['method']!=method:raise Failure('desktop_login_attempt_conflict')
        if action not in ('login_cancel',) and entry['status'] in ('canceled','expired'):
            return {'state':entry['status']}
    with sync_playwright() as pw:
        browser = pw.chromium.connect_over_cdp('http://127.0.0.1:'+str(state['port']), timeout=4000)
        pages = [p for c in browser.contexts for p in c.pages]
        chat = next((p for p in pages if urlsplit(p.url).hostname == 'doubao-chat'), None)
        if not chat:
            raise Failure('desktop_unavailable')
        if starting:
            # CDP is ready before the official renderer/SDK on a cold launch.
            # This waits for a read-only readiness condition, not for success.
            chat.wait_for_function("document.readyState!=='loading' && !!window['chunk_@flow-web/desktop'] && !!document.body",timeout=5000)
        windows = [p for p in pages if urlsplit(p.url).hostname == 'doubao-login']
        if action == 'login_cancel':
            # Closing only the official login window stops its own polling. It
            # does not log out an account or undo an already accepted login.
            entry['status'] = 'canceled'
            atomic(root/'client.json', state)
            for page in windows:
                page.close(run_before_unload=False)
            return {'state':'canceled'}
        if starting and (not entry or entry['nonce'] != nonce):
            if windows:
                raise Failure('desktop_login_attempt_conflict')
            identity = chat.evaluate(Path(__file__).with_name('runtime.js').read_text(encoding='utf-8'), {'operation':'identity'})
            if identity.get('ok'):
                return {'state':'already_authenticated'}
            if identity.get('error') != 'desktop_login_required':
                raise Failure(identity.get('error','desktop_protocol_invalid'))
            entry = {'nonce':nonce,'method':method,'status':'opening','deadline':time.time()+600}
            state['login'] = entry
            atomic(root/'client.json', state)  # prior to any UI/network side effect
            button = chat.get_by_text('登录',exact=True)
            if button.count()!=1:
                raise Failure('desktop_version_unsupported')
            button.click(timeout=4000)
            deadline = time.monotonic()+6
            while time.monotonic()<deadline:
                windows = [p for c in browser.contexts for p in c.pages if urlsplit(p.url).hostname=='doubao-login']
                if windows:
                    break
                chat.wait_for_timeout(100)
            if not windows:
                raise Failure('desktop_login_open_unconfirmed')
        if action in ('login_start','login_resume') and windows and entry and entry['nonce']==nonce:
            windows[0].wait_for_load_state('domcontentloaded',timeout=5000)
            windows[0].locator('body').wait_for(state='attached',timeout=3000)
            if method=='sms' and not login_shape(windows[0])['sms']:
                # Selecting the already-open official SMS tab never sends an
                # SMS. Recovery may repeat this idempotent navigation only.
                windows[0].get_by_text('手机号登录',exact=True).click(timeout=4000)
                windows[0].locator('input').first.wait_for(state='visible',timeout=4000)
            windows[0].bring_to_front()
            entry['status']='waiting';atomic(root/'client.json',state)
        if not entry or entry['nonce']!=nonce:
            raise Failure('desktop_login_attempt_missing')
        if entry['status']=='canceled':
            return {'state':'canceled'}
        if time.time()>=entry['deadline']:
            entry['status']='expired';atomic(root/'client.json',state)
            return {'state':'expired'}
        windows = [p for c in browser.contexts for p in c.pages if urlsplit(p.url).hostname=='doubao-login']
        identity = chat.evaluate(Path(__file__).with_name('runtime.js').read_text(encoding='utf-8'), {'operation':'identity'})
        if identity.get('ok'):
            entry['status']='verified';atomic(root/'client.json',state)
            return {'state':'verified'}
        if identity.get('error')!='desktop_login_required':
            raise Failure(identity.get('error','desktop_protocol_invalid'))
        if len(windows)!=1:
            return {'state':'manual_action','reason':'official_login_window_closed'}
        shape = login_shape(windows[0])
        if shape['expired']:
            entry['status']='expired';atomic(root/'client.json',state)
            return {'state':'expired'}
        if shape['manual']:
            return {'state':'manual_action','reason':'official_verification_required'}
        if shape['error']:
            return {'state':'manual_action','reason':'official_code_rejected'}
        if shape['scanned']:
            return {'state':'scanned'}
        if entry['method']=='sms':
            if not shape['sms'] and not shape['code']:
                return {'state':'manual_action','reason':'official_login_method_pending'}
            return {'state':'waiting_code' if shape['code'] else 'waiting_phone'}
        return {'state':'waiting_scan' if shape['qr'] else 'manual_action'}


def execute(request):
    if os.name != 'nt':
        raise Failure('desktop_management_platform_unsupported')
    if not isinstance(request,dict) or set(request)-{'action','root','executable','nonce','method'}:
        raise Failure('desktop_management_invalid')
    action=request.get('action')
    if action not in ('ensure','status','login_start','login_resume','login_poll','login_cancel','close'):
        raise Failure('desktop_management_invalid')
    root=Path(request.get('root',''))
    # Go creates and protects this dedicated directory before invoking us.
    if not root.is_absolute() or not root.is_dir() or root.is_symlink() or root==Path(root.anchor):
        raise Failure('desktop_management_invalid')
    root=root.resolve()
    if action.startswith('login_') and (not re.fullmatch('[a-f0-9]{32}',request.get('nonce','')) or request.get('method') not in ('sms','qr')):
        raise Failure('desktop_management_invalid')
    with exclusive(root):
        state=ensure(root,request.get('executable',''),action in ('ensure','login_start'))
        if action=='close':
            from playwright.sync_api import sync_playwright
            with sync_playwright() as pw:
                browser=pw.chromium.connect_over_cdp('http://127.0.0.1:'+str(state['port']),timeout=4000)
                session=browser.new_browser_cdp_session()
                try:session.send('Browser.close')
                except Exception:pass
            # A close response alone is not proof that the process exited.
            deadline=time.monotonic()+5
            while time.monotonic()<deadline:
                if not find_owned(state):return {'ok':True,'data':{'state':'disconnected'}}
                time.sleep(.1)
            raise Failure('desktop_close_unconfirmed')
        if action.startswith('login_'):
            return {'ok':True,'data':login(root,state,action,request['nonce'],request['method'])}
        return {'ok':True,'data':{'state':'running','port':state['port']}}


def main():
    try:
        data=sys.stdin.buffer.readline(32769)
        if len(data)>32768:raise Failure('desktop_management_invalid')
        result=execute(json.loads(data))
    except Failure as e:
        result={'ok':False,'error':str(e)}
    except Exception:
        result={'ok':False,'error':'desktop_management_unavailable'}
    sys.stdout.write(json.dumps(result))


if __name__=='__main__':main()
