"""Real HTTP and Playwright regression for run terminal verification."""
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from playwright.sync_api import sync_playwright
from run_wait import wait_run, RunWaitTimeout

class RunPollingTests(unittest.TestCase):
    def setUp(self):
        self.calls = 0
        self.snapshots = []
        self.stopped = threading.Event()
        parent = self
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                parent.calls += 1
                data = parent.snapshots[min(parent.calls - 1, len(parent.snapshots) - 1)]
                if callable(data):
                    data = data()
                raw = json.dumps({'runs': data}).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            def log_message(self, *_):
                pass
        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = 'http://127.0.0.1:%d/workbench' % self.server.server_port
        self.pw = sync_playwright().start()
        self.request = self.pw.request.new_context()

    def tearDown(self):
        self.request.dispose()
        self.pw.stop()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(2)

    @staticmethod
    def row(identity, status, finished=None, marker=None):
        return {'id': identity, 'task_key': 'same-task', 'status': status,
                'finished_at': finished, 'marker': marker}

    def test_old_same_key_and_unfinished_success_do_not_satisfy_new_run(self):
        old = self.row('old', 'succeeded', 'done')
        self.snapshots = [[old, self.row('new', 'running')],
                          [old, self.row('new', 'succeeded')],
                          [old, self.row('new', 'succeeded', 'done', 'verified-snapshot')],
                          [self.row('new', 'failed', 'later')]]
        result = wait_run(self.request, self.url, 'new', 'succeeded', timeout=2)
        self.assertEqual(self.calls, 3)
        self.assertEqual(result['marker'], 'verified-snapshot')

    def test_only_history_and_current_running_time_out(self):
        self.snapshots = [[self.row('old', 'succeeded', 'done'), self.row('new', 'running')]]
        with self.assertRaises(RunWaitTimeout):
            wait_run(self.request, self.url, 'new', 'succeeded', timeout=.25)
        self.assertGreaterEqual(self.calls, 2)

    def test_canceled_terminal_matches_current_id(self):
        self.snapshots = [[self.row('old', 'canceled', 'done'), self.row('new', 'running')],
                          [self.row('new', 'canceled', 'done')]]
        result = wait_run(self.request, self.url, 'new', 'canceled', timeout=2)
        self.assertEqual(result['id'], 'new')
        self.assertEqual(self.calls, 2)

    def test_unexpected_current_terminal_is_a_failure(self):
        self.snapshots = [[self.row('new', 'failed', 'done')]]
        with self.assertRaisesRegex(AssertionError, 'unexpected_run_terminal_status'):
            wait_run(self.request, self.url, 'new', 'succeeded', timeout=2)

    def test_cancel_during_http_read_cannot_return_success(self):
        def cancel_and_respond():
            self.stopped.set()
            return [self.row('new', 'succeeded', 'done')]
        self.snapshots = [cancel_and_respond]
        with self.assertRaises(InterruptedError):
            wait_run(self.request, self.url, 'new', 'succeeded', cancel=self.stopped)
        self.assertEqual(self.calls, 1)

if __name__ == '__main__':
    unittest.main()
