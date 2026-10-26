"""Offline checks: importing the recorder never calls AI Studio."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('record_aistudio',Path(__file__).with_name('record-aistudio.py'))
rec=importlib.util.module_from_spec(spec)
spec.loader.exec_module(rec)

class RecorderTests(unittest.TestCase):
    def test_terminal_and_incomplete(self):
        raw=b'data: {"type":"response.created"}\n\ndata: {"type":"response.completed","response":{"id":"r","output":[]}}\n\ndata: [DONE]\n\n'
        self.assertEqual(rec.terminal(raw)['id'],'r')
        with self.assertRaises(ValueError):rec.terminal(b'data: {"type":"response.created"}\n\n')

    def test_function_schema(self):
        f=rec.function('read')
        self.assertEqual(f['name'],'read')
        self.assertEqual(f['parameters']['properties'],{})
        self.assertFalse(f['parameters']['additionalProperties'])

    def test_recording_is_private_bounded_and_does_not_retry(self):
        class Response:
            class headers:
                @staticmethod
                def get_content_type():return 'text/event-stream'
            def __enter__(self):return self
            def __exit__(self,*args):return False
            def readline(self,size):return next(self.lines,b'')
            def __init__(self):
                self.lines=iter([b'data: {"type":"response.completed","response":{"id":"r","output":[{"type":"message"}],"usage":{}}}\n',b'\n'])
        class Opener:
            def __init__(self):self.requests=[]
            def open(self,request,timeout):
                self.requests.append(request)
                return Response()
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);key=root/'key';key.write_text('synthetic-secret')
            opener=Opener()
            with patch.object(rec.urllib.request,'build_opener',return_value=opener):
                rec.record('synthetic-folder',key,root/'recorded',1,'text')
            self.assertEqual(len(opener.requests),1)
            request=opener.requests[0]
            self.assertEqual(request.get_header('X-data-logging-enabled'),'false')
            self.assertFalse(json.loads(request.data)['store'])
            self.assertLessEqual(json.loads(request.data)['max_tool_calls'],1)
            saved=''.join(p.read_text() for p in (root/'recorded').iterdir())
            self.assertNotIn('synthetic-secret',saved)
            self.assertNotIn('synthetic-folder',saved)
            self.assertEqual(json.loads((root/'recorded/attempts.json').read_text())['attempts'],1)

if __name__=='__main__':unittest.main()
