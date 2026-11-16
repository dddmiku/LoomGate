"""User-level actions for the redesigned console. No HTTP shortcuts or submits.

Run acceptance still uses run_wait.wait_run with the accepted run ID.
"""

def reveal_filter(page, selector):
    control = page.locator(selector)
    if not control.is_visible():
        control.locator('xpath=ancestor::details').locator('summary').click()
    control.wait_for(state='visible')


def sidebar_action(page, selector, close=True):
    if not page.locator(selector).is_visible():
        page.locator('#navigation-toggle').click()
    page.locator(selector).click()
    if close and page.locator('#sidebar').get_attribute('aria-modal') == 'true':
        page.locator('#navigation-close').click()


def select_task(page, key, action=None, first=False):
    row = page.locator('[data-task="' + key + '"]')
    if first:
        row = row.first
    if not row.is_visible() and page.locator('[data-action=task-back]').is_visible():
        page.locator('[data-action=task-back]').click()
    row.click()
    page.locator('#task-detail').wait_for(state='visible')
    if action:
        page.locator('#task-detail [data-action="' + action + '"]').click()


def plan_timing(page):
    page.locator('#plan-next:not([disabled])').wait_for()
    page.locator('#plan-next').click()
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
    page.locator('#plan-next').click()
    page.locator('[data-plan-step="2"]').wait_for(state='visible')
    page.locator('#plan-save').wait_for(state='visible')
