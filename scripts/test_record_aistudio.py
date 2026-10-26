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


class ScriptedResponse:
    class headers:
        @staticmethod
        def get_content_type():return 'text/event-stream'
    def __init__(self,items):
        event={'type':'response.completed','response':{'id':'response','output':items,'usage':{}}}
        self.lines=iter([(b'data: '+json.dumps(event).encode()+b'\n'),b'\n'])
    def __enter__(self):return self
    def __exit__(self,*args):return False
    def readline(self,size):return next(self.lines,b'')

class ScriptedOpener:
    def __init__(self,outputs):self.outputs=iter(outputs);self.requests=[]
    def open(self,request,timeout):
        self.requests.append(json.loads(request.data))
        return ScriptedResponse(next(self.outputs))

def function_call(identity,name,args):
    return {'id':identity,'type':'function_call','call_id':name,'name':name,'arguments':json.dumps(args)}

class RecorderProbeTests(unittest.TestCase):
    def record(self,outputs,dialog,normalize=False):
        directory=tempfile.TemporaryDirectory();self.addCleanup(directory.cleanup)
        root=Path(directory.name);key=root/'key';key.write_text('synthetic-key')
        opener=ScriptedOpener(outputs)
        with patch.object(rec.urllib.request,'build_opener',return_value=opener):
            rec.record('folder',key,root/'recorded',8,dialog,normalize)
        return opener.requests

    def test_correlation_rewrites_both_sides_and_preserves_results(self):
        first=[function_call('id-a','test_repeat',{'slot':'a'}),function_call('id-b','test_repeat',{'slot':'b'})]
        second=[function_call('id-c','test_repeat',{'slot':'c'}),function_call('id-p','test_present',{'result_id':'id-b','marker':'marker-b'})]
        requests=self.record([first,second,[{'type':'message'}]],'correlation',True)
        self.assertEqual(len(requests),3)
        items=requests[1]['input'][1:]
        self.assertEqual([i['call_id'] for i in items],['id-a','id-b','id-a','id-b'])
        self.assertEqual([json.loads(i['output']) for i in items if i['type']=='function_call_output'],[{'marker':'marker-a'},{'marker':'marker-b'}])
        self.assertEqual(requests[1]['tools'][1]['parameters']['properties']['result_id']['enum'],['id-b'])
        self.assertEqual(first[0]['call_id'],'test_repeat')

    def test_unexpected_functions_and_premature_presentation_fail_cleanly(self):
        for name,args in [('unexpected',{}),('test_repeat',{'slot':'wrong'}),('test_repeat',{}),('test_present',{'result_id':'id-b','marker':'marker-b'})]:
            with self.subTest(name=name,args=args):
                with self.assertRaises(ValueError):self.record([[function_call('bad',name,args)]],'correlation',True)
        # Slot b produced in the same response is not a prior-step result.
        with self.assertRaisesRegex(ValueError,'prior step'):
            self.record([[function_call('b','test_repeat',{'slot':'b'}),function_call('p','test_present',{'result_id':'b','marker':'marker-b'})]],'correlation',True)

    def test_allowlist_and_reference_validation(self):
        valid=function_call('id','test_repeat',{'slot':'a'})
        self.assertEqual(rec.function_value('repeat',valid,{}),{'synthetic':True,'total_minor':12345,'currency':'RUB'})
        for call in [function_call('x','test_present',{'result_id':'other','marker':'marker-b'}),dict(valid,arguments='[]'),dict(valid,arguments=None),dict(valid,id='')]:
            with self.assertRaises(ValueError):rec.function_value('correlation',call,{'b':'id-b'})
        self.assertEqual(rec.function_value('functions',function_call('x','test_overview',{}),{}),{'synthetic':True,'total_minor':12345,'currency':'RUB'})

    def test_default_batch_excludes_extra_probes(self):
        requests=self.record([[{'type':'message'}]]*3,None)
        self.assertEqual(len(requests),3)
        for request in requests:
            self.assertFalse(any(tool.get('name') in ['test_repeat','test_present'] for tool in request['tools']))

    def test_repeat_without_normalization_keeps_provider_alias(self):
        requests=self.record([[function_call('a','test_repeat',{'slot':'a'})],[function_call('b','test_repeat',{'slot':'b'})]],'repeat')
        self.assertEqual(requests[1]['input'][1]['call_id'],'test_repeat')
        self.assertEqual(requests[1]['input'][2]['call_id'],'test_repeat')

if __name__=='__main__':unittest.main()
