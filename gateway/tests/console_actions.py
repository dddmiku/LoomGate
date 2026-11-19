"""User-level actions for the redesigned console. No HTTP shortcuts or submits.

Run acceptance still uses run_wait.wait_run with the accepted run ID.
"""

def reveal_filter(page, selector):
    control = page.locator(selector)
    if not control.is_visible():
        control.locator('xpath=ancestor::details').locator('summary').click()
    control.wait_for(state='visible')


def choose_provider(page, selector, value):
    page.locator(selector + ' [data-provider-choice="' + value + '"]').click()
    page.locator(selector + ' [data-provider-choice="' + value + '"][aria-pressed=true]').wait_for()


def choose_account(page, selector, value):
    page.locator(selector + ':not([disabled])').click()
    page.locator('#scope-search').fill(value)
    page.locator('[data-account-choice="' + value + '"]').click()
    page.locator('#dialog').wait_for(state='hidden')
    # The trigger label is the verified nickname when the provider supplies one;
    # only the account id or the "all accounts" label can be asserted exactly.
    if not value:
        label = '所有账号（分页）' if selector == '#model-account' else '全部已启用账号'
        page.wait_for_function('([selector,label])=>document.querySelector(selector+" strong")?.textContent===label', arg=[selector,label])
    else:
        page.wait_for_function('(selector)=>{const el=document.querySelector(selector);const text=el?.querySelector("strong")?.textContent||"";return el&&!el.disabled&&text&&text!=="正在读取账号…"}', arg=selector)


def choose_capability(page, value):
    page.locator('#model-capability [data-capability="' + value + '"]').click()


def sidebar_action(page, selector, close=True):
    if not page.locator(selector).is_visible():
        page.locator('#navigation-toggle').click()
    page.locator(selector).click()
    if close and page.locator('#sidebar').get_attribute('aria-modal') == 'true':
        page.locator('#navigation-close').click()


def select_task(page, key, action=None, first=False):
    """Trigger a row action in the task center.

    The task page renders one row per shipped plan (data-task = plan task key)
    plus catalog-only rows for discovered tasks without a plan. Actions live on
    the row itself: 运行 = run-plan, 定时 = edit-plan-time, 日志 = task-log.
    Historical action names from the old detail-pane layout are mapped here so
    existing acceptance scripts keep their intent.
    """
    row = page.locator('[data-task="' + key + '"]')
    if first:
        row = row.first
    row.wait_for(state='visible')
    if action:
        mapped = {
            'task-schedule': 'edit-plan-time',
            'run-task': 'run-plan',
            'manual-task': 'manual-plan',
        }.get(action, action)
        row.locator('[data-action="' + mapped + '"]').first.click()


def plan_timing(page):
    # Fixed-work editors (opened from a task or an existing plan) start on the
    # timing step and expose only 保存计划; the old three-step flow is gone.
    advance = page.locator('#plan-next')
    if advance.count() and advance.is_visible():
        page.locator('#plan-next:not([disabled])').wait_for()
        advance.click()
    page.locator('[data-plan-step="1"]').wait_for(state='visible')


def plan_times(page, times, zone=None):
    controls = page.locator('input[name=plan-time]')
    while controls.count() < len(times):
        page.locator('#plan-add-time').click()
    while controls.count() > len(times):
        page.locator('[data-remove-time]').last.click()
    for index, value in enumerate(times):
        controls.nth(index).fill(value)
    if zone is not None:
        reveal_filter(page, '#plan-timezone')
        page.locator('#plan-timezone').fill(zone)


def plan_review(page):
    advance = page.locator('#plan-next')
    if advance.count() and advance.is_visible():
        advance.click()
        page.locator('[data-plan-step="2"]').wait_for(state='visible')
    page.locator('#plan-save').wait_for(state='visible')
