#!/usr/bin/env python3
"""Tests for the local submux prompt-slimming adapter."""
import importlib.util
import json
import os
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

HERE = Path(__file__).resolve().parent


class AddonTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.state = Path(self.temp.name) / 'session-one'
        self.state.mkdir()
        self.env = patch.dict(os.environ, SLIM_STATE=str(self.state))
        self.env.start()
        self.addCleanup(self.env.stop)
        http = types.SimpleNamespace(HTTPFlow=object, Response=types.SimpleNamespace(make=lambda *args: args))
        self.mitm = patch.dict('sys.modules', {'mitmproxy': types.SimpleNamespace(http=http)})
        self.mitm.start()
        self.addCleanup(self.mitm.stop)
        spec = importlib.util.spec_from_file_location('sc_slim_addon', HERE / 'slim-submux-addon.py')
        self.addon = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.addon)

    def test_rewrites_messages_and_preserves_manual(self):
        body = {'model': 'test', 'tools': [{'name': 'Artifact', 'description': 'FULL MANUAL',
                  'input_schema': {'type': 'object', 'properties': {'action': {'type': 'string', 'description': 'what to do'}}}}]}
        req = types.SimpleNamespace(path='/v1/messages', content=json.dumps(body).encode())
        self.addon.request(types.SimpleNamespace(request=req))
        result = json.loads(req.content)
        self.assertIn('Before your first Artifact call', result['tools'][0]['description'])
        self.assertIn('FULL MANUAL', (self.state / 'Artifact.md').read_text())
        self.assertIn('what to do', (self.state / 'Artifact.md').read_text())

    def test_identical_manuals_have_identical_prompt_paths(self):
        body = {'tools': [{'name': 'Artifact', 'description': 'FULL MANUAL', 'input_schema': {'type': 'object'}}]}
        def run():
            req = types.SimpleNamespace(path='/v1/messages', content=json.dumps(body).encode())
            self.addon.request(types.SimpleNamespace(request=req, response=None))
            return json.loads(req.content)['tools'][0]['description']
        first = run()
        self.addon.slim.STATE = Path(self.temp.name) / 'session-two'
        self.addon.slim.STATE.mkdir()
        second = run()
        self.assertEqual(first, second)
        mapping = json.loads((self.addon.slim.STATE / 'manual-map.json').read_text())
        self.assertIn(mapping['Artifact'], second)
        self.assertIn('FULL MANUAL', Path(mapping['Artifact']).read_text())

    def test_slimming_failure_blocks_request(self):
        req = types.SimpleNamespace(path='/v1/messages', content=b'{"tools":[]}')
        flow = types.SimpleNamespace(request=req, response=None)
        with patch.object(self.addon.slim, 'load_rewrites', side_effect=OSError('failure')):
            with self.assertLogs(level='ERROR'):
                self.addon.request(flow)
        self.assertIsNotNone(flow.response)

    def test_other_paths_unchanged_and_response_streamed(self):
        content = b'{"tools":[]}'
        req = types.SimpleNamespace(path='/v1/models', content=content)
        self.addon.request(types.SimpleNamespace(request=req))
        self.assertEqual(req.content, content)
        response = types.SimpleNamespace(stream=False)
        self.addon.responseheaders(types.SimpleNamespace(response=response))
        self.assertTrue(response.stream)


if __name__ == '__main__':
    unittest.main()
