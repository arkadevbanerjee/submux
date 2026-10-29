#!/usr/bin/env python3
"""Run one Claude session through an isolated local slim-tools reverse proxy."""
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path


def main():
    if len(sys.argv) < 3:
        print('usage: slim-submux-runner.py RELAY_PORT COMMAND [ARGS...]', file=sys.stderr)
        return 64
    relay_port = int(sys.argv[1])
    executable = shutil.which('mitmdump')
    if not executable or not shutil.which('lsof'):
        print('submux-claude: slimming requires mitmdump and lsof', file=sys.stderr)
        return 1

    root = Path(os.environ.get('SLIM_SUBMUX_STATE_ROOT', Path.home() / '.local/state/slim-tools/submux'))
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    state = Path(tempfile.mkdtemp(prefix='sc-', dir=root))
    env = dict(os.environ, SLIM_STATE=str(state), SLIM_TOOLS='1')
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    addon = Path(__file__).resolve().with_name('slim-submux-addon.py')
    log = state / 'proxy.log'
    with log.open('w') as output:
        proxy = subprocess.Popen([executable, '--quiet', '--mode', f'reverse:http://127.0.0.1:{relay_port}',
                                  '--listen-host', '127.0.0.1', '--listen-port', str(port),
                                  '--set', f'confdir={state / "mitm"}', '-s', str(addon)],
                                 env=dict(env, PYTHONDONTWRITEBYTECODE='1'),
                                 stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        child = None

        def stop(signum, _frame):
            raise SystemExit(128 + signum)

        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGHUP, stop)
        try:
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if proxy.poll() is not None:
                    raise RuntimeError(f'proxy exited with status {proxy.returncode}')
                check = subprocess.run(['lsof', '-nP', '-a', '-p', str(proxy.pid), '-iTCP:' + str(port),
                                        '-sTCP:LISTEN', '-t'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                if check.returncode == 0 and (state / 'addon-ready').is_file():
                    break
                time.sleep(0.05)
            else:
                raise RuntimeError('proxy did not start within 10 seconds')
            env['ANTHROPIC_BASE_URL'] = f'http://127.0.0.1:{port}'
            child = subprocess.Popen(sys.argv[2:], env=env)
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            status = child.wait()
            return 128 - status if status < 0 else status
        except (OSError, RuntimeError) as exc:
            print(f'submux-claude: slim proxy failed: {exc}; see {log}', file=sys.stderr)
            return 1
        finally:
            if child is not None and child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
            if proxy.poll() is None:
                proxy.terminate()
                try:
                    proxy.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proxy.kill()
                    proxy.wait()


if __name__ == '__main__':
    sys.exit(main())
