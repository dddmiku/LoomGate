"""Authoritative, bounded polling of one accepted run; no browser JS promises."""
import threading
import time

TERMINAL = {'succeeded', 'failed', 'canceled', 'uncertain', 'skipped', 'manual_action'}

class RunWaitTimeout(TimeoutError):
    pass

def wait_run(request, endpoint, run_id, expected, timeout=15, cancel=None):
    if not run_id or expected not in TERMINAL or timeout <= 0:
        raise ValueError('invalid_run_wait')
    deadline = time.monotonic() + timeout
    delay = .05
    cancellation = cancel or threading.Event()
    while True:
        if cancellation.is_set():
            raise InterruptedError('run_wait_canceled')
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RunWaitTimeout('run_terminal_timeout')
        response = request.get(endpoint, timeout=max(1, min(2000, int(remaining * 1000))))
        try:
            if response.status != 200:
                raise AssertionError('run_status_request_failed')
            snapshot = response.json()
        finally:
            response.dispose()
        if cancellation.is_set():
            raise InterruptedError('run_wait_canceled')
        if time.monotonic() >= deadline:
            raise RunWaitTimeout('run_terminal_timeout')
        matches = [row for row in snapshot.get('runs', []) if row.get('id') == run_id]
        if len(matches) > 1:
            raise AssertionError('duplicate_run_identity')
        if matches:
            run = matches[0]
            if run.get('finished_at') and run.get('status') in TERMINAL:
                if run['status'] != expected:
                    raise AssertionError('unexpected_run_terminal_status')
                # Return this exact verified snapshot, not a later independent read.
                return run
        cancellation.wait(min(delay, max(0, deadline - time.monotonic())))
        delay = min(.5, delay * 1.5)
