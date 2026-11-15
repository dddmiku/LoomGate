'use strict';
document.getElementById('login-form').addEventListener('submit', async event => {
  event.preventDefault();
  const button = document.getElementById('login-submit');
  if (button.disabled) return;
  const error = document.getElementById('login-error');
  button.disabled = true; error.hidden = true;
  const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 15000);
  try {
    const response = await fetch('api/auth/login', {method:'POST', credentials:'same-origin', signal:controller.signal, headers:{'Content-Type':'application/json','X-Admin-Request':'1'}, body:JSON.stringify({username:document.getElementById('username').value, password:document.getElementById('password').value})});
    const result = await response.json();
    if (!response.ok || result.ok !== true) throw new Error(response.status === 429 ? '请稍后再试，登录请求过于频繁。' : '登录未成功，请检查账号和密码。');
    document.getElementById('password').value = ''; location.replace('./');
  } catch (e) {
    error.textContent = e.name === 'AbortError' ? '连接超时，请检查工作区是否在线。' : e instanceof TypeError ? '暂时无法连接工作区。' : e.message;
    error.hidden = false;
  } finally { clearTimeout(timer); button.disabled = false; }
});
