#!/usr/bin/env python3
"""Record synthetic Responses dialogs. Opt-in only; never called by CI."""
import argparse
import datetime
import json
import os
from pathlib import Path
import ssl
import urllib.error
import urllib.request

ENDPOINT = 'https://ai.api.cloud.yandex.net/v1/responses'
INSTRUCTIONS = ('This is a synthetic protocol test, with no personal data. '
                'Follow the requested function calls exactly. Keep text under 30 words. '
                'Use web search only when explicitly requested, at most twice in this entire dialogue.')

def function(name):
    return {'type': 'function', 'name': name, 'description': 'Synthetic test function '+name,
            'parameters': {'type': 'object', 'properties': {}, 'additionalProperties': False}, 'strict': True}


def terminal(raw):
    for block in raw.decode('utf-8').split('\n\n'):
        lines = [x[5:].lstrip() for x in block.splitlines() if x.startswith('data:')]
        if not lines:
            continue
        value = '\n'.join(lines)
        if value == '[DONE]':
            continue
        event = json.loads(value)
        if event.get('type') == 'response.completed':
            return event['response']
    raise ValueError('recorded stream has no completed response')


def record(folder, key_path, output, max_requests=8, only=None, normalize_call_ids=False):
    # Credentials never enter fixtures, URLs, command arguments or printed errors.
    key = Path(key_path).read_text().strip()
    if not key or len(key)>8192 or any(ord(c)<33 or ord(c)>126 for c in key):
        raise ValueError('invalid credential file')
    if not folder or any(not(c.isascii() and (c.isalnum() or c in '-_')) for c in folder):
        raise ValueError('invalid folder')
    output.mkdir(parents=True, exist_ok=False)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=ssl.create_default_context()))
    dialogs = [
        ('text', 'Briefly explain what a personal budget is.', []),
        ('functions', 'Call test_overview and test_totals in parallel. After both outputs, call test_details. After its output, give a short conclusion.', [function(n) for n in ['test_overview','test_totals','test_details']]),
        ('repeat', 'Call test_repeat twice in parallel with slot a and slot b. After both results, call test_repeat once more with slot c. Do not answer with text before these calls.', [dict(function('test_repeat'),parameters={'type':'object','properties':{'slot':{'type':'string','enum':['a','b','c']}},'required':['slot'],'additionalProperties':False})]),
        ('correlation', 'First call test_repeat in parallel with slot a and slot b. After both results call test_repeat with slot c. After that call test_present using result_id equal to the call_id for slot b visible in the input, and marker copied from that result. After test_present give a short final answer.', [dict(function('test_repeat'),parameters={'type':'object','properties':{'slot':{'type':'string','enum':['a','b','c']}},'required':['slot'],'additionalProperties':False}),dict(function('test_present'),parameters={'type':'object','properties':{'result_id':{'type':'string'},'marker':{'type':'string'}},'required':['result_id','marker'],'additionalProperties':False})]),
        ('search', 'Use web search once for the official Bank of Russia website and give one short fact with a URL citation.', [{'type':'web_search'}]),
    ]
    count, searches = 0, 0
    manifest = {'recorded_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'endpoint':ENDPOINT,'model':'aliceai-llm-flash','synthetic':True,'normalized_call_ids':normalize_call_ids,'entries':[]}
    for name, question, tools in dialogs:
        if (only and name != only) or (not only and name in ['repeat','correlation']):
            continue
        history = [{'role':'user','content':question}]
        result_ids = {}
        for step in range(4):
            if count >= max_requests:
                raise ValueError('request budget exhausted; recordings retained')
            body = {'model':'gpt://'+folder+'/aliceai-llm-flash','instructions':INSTRUCTIONS,'input':history,
                    'tools':tools,'stream':True,'store':False,'parallel_tool_calls':True,'truncation':'disabled',
                    'max_output_tokens':512,'max_tool_calls':max(0,1-searches)}
            if name=='correlation' and 'b' in result_ids:
                selected=json.loads(json.dumps(tools[1]))
                selected['parameters']['properties']['result_id']['enum']=[result_ids['b']]
                selected['parameters']['properties']['marker']['enum']=['marker-b']
                body['tools']=[tools[0],selected]
            request = urllib.request.Request(ENDPOINT,data=json.dumps(body).encode(),method='POST',headers={
                'Authorization':'Bearer '+key,'OpenAI-Project':folder,'Content-Type':'application/json',
                'Accept':'text/event-stream','x-data-logging-enabled':'false'})
            count += 1
            (output/'attempts.json').write_text(json.dumps({'attempts':count})+'\n')
            try:
                with opener.open(request,timeout=180) as response:
                    if response.headers.get_content_type()!='text/event-stream':
                        raise ValueError('unexpected response format')
                    chunks, size, completed_line = [], 0, False
                    while True:
                        line=response.readline(4*1024*1024+1)
                        if not line:
                            break
                        chunks.append(line)
                        size+=len(line)
                        if size>4*1024*1024:
                            raise ValueError('response too large')
                        if b'response.completed' in line or b'response.failed' in line or b'response.incomplete' in line:
                            completed_line=True
                        if completed_line and not line.strip():
                            break
                    raw=b''.join(chunks)
            except urllib.error.HTTPError as error:
                raise ValueError('AI Studio HTTP '+str(error.code)+'; no response body printed') from None
            except urllib.error.URLError:
                raise ValueError('AI Studio transport failed; no automatic retry') from None
            stem = name+'-'+str(step)
            # Folder identity is immaterial to parsing; replace it in both fixtures.
            safe = raw.replace(folder.encode(),b'folder')
            if key.encode() in safe:
                raise ValueError('credential appeared in provider response; refusing fixture')
            (output/(stem+'.sse')).write_bytes(safe)
            (output/(stem+'.request.json')).write_text(json.dumps(body,ensure_ascii=False,indent=2).replace(folder,'folder')+'\n')
            completed = terminal(raw)
            items = completed.get('output',[])
            searches += sum(i.get('type')=='web_search_call' for i in items)
            manifest['entries'].append({'name':stem,'usage':completed.get('usage',{}),'types':[i.get('type') for i in items]})
            (output/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
            calls = [i for i in items if i.get('type')=='function_call']
            if not calls or (name=='repeat' and step==1):
                break
            history.extend([dict(i,call_id=i['id']) if normalize_call_ids and i.get('type')=='function_call' else i for i in items])
            for call in calls:
                allowed = call['name'] in ['test_overview','test_totals','test_details'] and json.loads(call['arguments'])=={}
                if name in ['repeat','correlation']:
                    allowed = call['name']=='test_repeat' and json.loads(call['arguments']) in [{'slot':s} for s in ['a','b','c']]
                if name=='correlation' and call['name']=='test_present':
                    args=json.loads(call['arguments'])
                    allowed=set(args)=={'result_id','marker'} and isinstance(args['result_id'],str) and isinstance(args['marker'],str)
                if not allowed:
                    raise ValueError('unexpected function request; no execution')
                value={'synthetic':True,'total_minor':12345,'currency':'RUB'}
                if name=='correlation':
                    args=json.loads(call['arguments'])
                    value={'marker':'marker-'+args['slot']} if call['name']=='test_repeat' else {'presented':True}
                    if call['name']=='test_repeat':
                        result_ids[args['slot']]=call['id'] if normalize_call_ids else call['call_id']
                    elif args!={'result_id':result_ids['b'],'marker':'marker-b'}:
                        raise ValueError('correlation did not preserve the selected result')
                history.append({'type':'function_call_output','call_id':call['id'] if normalize_call_ids else call['call_id'],'output':json.dumps(value)})
        else:
            raise ValueError('four-step dialogue limit reached')
    print('Recorded '+str(count)+' HTTP responses and '+str(searches)+' searches; inspect manifest and run replay tests.')


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--folder',required=True)
    p.add_argument('--key-file',required=True)
    p.add_argument('--output',type=Path,required=True)
    p.add_argument('--dialog',choices=['text','functions','search','repeat','correlation'])
    p.add_argument('--normalize-call-ids',action='store_true')
    p.add_argument('--max-requests',type=int,choices=range(1,9),default=8)
    p.add_argument('--allow-paid-requests',action='store_true',required=True)
    a=p.parse_args()
    try:
        record(a.folder,a.key_file,a.output,a.max_requests,a.dialog,a.normalize_call_ids)
    except ValueError as error:
        raise SystemExit(str(error)) from None
    except (OSError,TimeoutError):
        # Do not print arbitrary exception text: it can contain a URL or secret path.
        raise SystemExit('Recording failed. No automatic retry. Inspect retained fixtures and request budget.') from None

if __name__=='__main__':main()
