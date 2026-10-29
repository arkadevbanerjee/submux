#!/usr/bin/env python3
"""Exercise the per-session slimming relay without contacting a provider."""
import json
import os
import signal
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent


class Handler(BaseHTTPRequestHandler):
    body = None

    def do_POST(self):
        type(self).body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')

    def log_message(self, *args):
        pass


class RunnerTest(unittest.TestCase):
    def test_term_stops_proxy_and_child(self):
        with tempfile.TemporaryDirectory() as d:
            fake = Path(d) / 'wait.py'
            fake.write_text('import os, time\nprint(os.getpid(), os.environ["ANTHROPIC_BASE_URL"], flush=True)\ntime.sleep(30)\n')
            env = dict(os.environ, SLIM_SUBMUX_STATE_ROOT=d)
            runner = subprocess.Popen([sys.executable, str(HERE / 'slim-submux-runner.py'), '9', sys.executable, str(fake)],
                                      env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            proxy_pid = child_pid = None
            try:
                line = runner.stdout.readline()
                child, url = line.split()
                child_pid = int(child)
                port = url.rsplit(':', 1)[1]
                proxy_pid = int(subprocess.check_output(['lsof', '-nP', '-t', '-iTCP:' + port, '-sTCP:LISTEN'], text=True).strip())
                runner.terminate()
                runner.wait(timeout=10)
                for pid in (proxy_pid, child_pid):
                    with self.assertRaises(ProcessLookupError):
                        os.kill(pid, 0)
            finally:
                if runner.poll() is None:
                    runner.kill()
                    runner.wait()
                for pid in (proxy_pid, child_pid):
                    if pid is not None:
                        try:
                            os.kill(pid, signal.SIGTERM)
                        except ProcessLookupError:
                            pass
                runner.stdout.close()
                runner.stderr.close()

    def test_forwards_slimmed_body_and_keeps_manual(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            fake = root / 'fake.py'
            fake.write_text('''import json, os, urllib.request\nbody = {"model":"test","tools":[{"name":"Artifact","description":"FULL MANUAL","input_schema":{"type":"object"}}]}\nreq = urllib.request.Request(os.environ["ANTHROPIC_BASE_URL"] + "/v1/messages", json.dumps(body).encode(), {"Content-Type":"application/json"})\nassert urllib.request.urlopen(req).read() == b"ok"\nprint(os.environ["SLIM_STATE"])\n''')
            server = HTTPServer(('127.0.0.1', 0), Handler)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                env = dict(os.environ, SLIM_SUBMUX_STATE_ROOT=d)
                result = subprocess.run([sys.executable, str(HERE / 'slim-submux-runner.py'), str(server.server_port), sys.executable, str(fake)],
                                        env=env, capture_output=True, text=True, timeout=25)
            finally:
                server.shutdown()
                server.server_close()
                thread.join()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('Before your first Artifact call', Handler.body['tools'][0]['description'])
            state = Path(result.stdout.strip())
            self.assertEqual(state.parent, root)
            self.assertIn('FULL MANUAL', (state / 'Artifact.md').read_text())


if __name__ == '__main__':
    unittest.main()
