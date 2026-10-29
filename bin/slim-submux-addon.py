"""Apply the user's slim-tools transformations to the local submux relay."""
import hashlib
import importlib.util
import json
import logging
import os
import tempfile
from pathlib import Path

from mitmproxy import http

source = Path.home() / '.agents/slim-tools/slim_tools.py'
spec = importlib.util.spec_from_file_location('slim_tools', source)
slim = importlib.util.module_from_spec(spec)
spec.loader.exec_module(slim)


def stable_manuals(body):
    mapping = {}
    for name in slim.MANUALS:
        local = slim.STATE / f'{name}.md'
        if not local.is_file():
            continue
        content = local.read_bytes()
        digest = hashlib.sha256(content).hexdigest()
        target = slim.STATE.parent / 'manuals' / digest / local.name
        target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        fd, tmp = tempfile.mkstemp(dir=target.parent)
        try:
            with os.fdopen(fd, 'wb') as file:
                file.write(content)
            try:
                os.link(tmp, target)
            except FileExistsError:
                pass
        finally:
            os.unlink(tmp)
        if target.read_bytes() != content:
            raise ValueError('manual cache content mismatch')
        mapping[name] = str(target)
    for tool in body.get('tools', []):
        name = slim.manual_for(tool.get('name', ''))
        if name in mapping:
            tool['description'] = tool.get('description', '').replace(
                str(slim.STATE / f'{name}.md'), mapping[name])
    (slim.STATE / 'manual-map.json').write_text(json.dumps(mapping))


def request(flow):
    req = flow.request
    if not req.path.startswith('/v1/messages'):
        return
    try:
        body = json.loads(req.content)
    except (ValueError, TypeError):
        return
    try:
        hits = slim.rewrite_body(body, slim.load_rewrites())
        slim._record(hits)
        slimmed = slim.slim_body(body)
        if slimmed:
            stable_manuals(body)
        if slimmed or any(hits.values()):
            req.content = json.dumps(body, ensure_ascii=False, separators=(',', ':')).encode()
    except Exception:
        logging.exception('submux slim-tools transformation failed')
        flow.response = http.Response.make(502, b'submux: slim-tools transformation failed')


def responseheaders(flow):
    slim.responseheaders(flow)


(slim.STATE / 'addon-ready').touch()
