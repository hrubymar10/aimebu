#!/bin/sh
# Smoke a real binary upgrade with a scratch Claude protocol stub.
set -eu
if [ "$#" -ne 3 ]; then
    echo "usage: $0 OLD_BINARY NEW_BINARY SCRATCH_DIR" >&2
    exit 2
fi
exec python3 - "$@" <<'PYCODE'
import json
import sys
import tempfile
import shutil
import os
import pathlib
import re
import socket
import subprocess
import time
import urllib.error
import urllib.request
import http.cookiejar

OLD = pathlib.Path(sys.argv[1]).resolve()
NEW = pathlib.Path(sys.argv[2]).resolve()
PARENT = pathlib.Path(sys.argv[3]).resolve()
if not OLD.is_file() or not NEW.is_file() or not PARENT.is_dir():
    raise SystemExit('old binary, new binary, and an existing scratch directory required')
STUB = r'''#!/usr/bin/env python3
import json
import os
import pathlib
import sys
import time
import urllib.request

if '--version' in sys.argv:
    print('2.1.0')
    sys.exit(0)

root = pathlib.Path(__file__).resolve().parent.parent
args = sys.argv
config = json.loads(args[args.index('--mcp-config') + 1])
entry = next(iter(config['mcpServers'].values()))
url = entry['env']['AIMEBU_URL'] + '/api/agent/mcp'
key = os.environ['AIMEBU_AGENT_KEY']
session = args[args.index('--session-id') + 1] if '--session-id' in args else '00000000-0000-4000-8000-000000000001'
payload = json.load(sys.stdin)
note = next((n for n in payload['notes'] if 'release-probe' in n.get('body', '')), payload['notes'][-1])
old = 'old-release-probe' in note['body']
kind = 'old' if old else 'new'
def event(msg):
    with (root / 'stub.log').open('a') as f:
        f.write(time.strftime('%Y-%m-%dT%H:%M:%SZ ', time.gmtime()) + kind + ' ' + msg + '\n')
event('started turn=' + payload['turn_id'])
if old:
    request = {'jsonrpc':'2.0','id':1,'method':'tools/call','params':{'name':'run_command','arguments':{
        'cmd': 'while [ ! -f ' + str(root / 'job.release') + ' ]; do sleep 0.2; done; echo job-finished',
        'request_key':'upgrade-job-1','room_id':note['room_id'],'label':'upgrade-job','timeout_seconds':120}}}
    http = urllib.request.Request(url, data=json.dumps(request).encode(), headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'}, method='POST')
    with urllib.request.urlopen(http, timeout=40) as response:
        result = json.load(response)
    data = result.get('result', {})
    if data.get('isError') or result.get('error'):
        event('job_failed ' + json.dumps(result))
        sys.exit(2)
    job = json.loads(data['content'][0]['text'])
    (root / 'job.id').write_text(job['id'])
    (root / 'old.turn').write_text(payload['turn_id'])
    event('job_started id=' + job['id'] + ' state=' + job['state'])
    while not (root / 'turn.release').exists():
        time.sleep(0.1)
else:
    (root / 'new.turn').write_text(payload['turn_id'])
answer = {'messages':[{'room':note['room_id'],'body':kind+'-answered','reply_to':note['message_id']}]}
print(json.dumps({'type':'system','subtype':'init','session_id':session}), flush=True)
print(json.dumps({'type':'result','subtype':'success','is_error':False,'result':json.dumps(answer),'structured_output':answer,'session_id':session,'usage':{'input_tokens':5,'output_tokens':5}}), flush=True)
event('answered')
'''

T = pathlib.Path(tempfile.mkdtemp(prefix='upgrade-', dir=PARENT))
for name in ('bin', 'home', 'codex', 'claude', 'project'):
    (T/name).mkdir()
stub = T/'bin'/'claude'
stub.write_text(STUB)
stub.chmod(0o700)

C = T/('config-'+str(os.getpid()))
for name in ('job.id','old.turn','new.turn','turn.release','job.release','stub.log'):
    (T/name).unlink(missing_ok=True)
env = os.environ.copy()
env.update(HOME=str(T/'home'), CODEX_HOME=str(T/'codex'), CLAUDE_CONFIG_DIR=str(T/'claude'),
           AIMEBU_CONFIG_DIR=str(C), AIMEBU_SENTRY_DSN='off', AIMEBU_BIND='127.0.0.1',
           AIMEBU_OWNER_LOGIN='owner', AIMEBU_OWNER_PASSWORD='correct horse battery staple',
           AIMEBU_WEB_BUNDLE='off', AIMEBU_USAGES='0', AIMEBU_ALLOW_NATIVE='1',
           AIMEBU_WARM_IDLE='0', PATH=str(T/'bin')+':'+os.environ['PATH'])
env.pop('AIMEBU_URL', None)
with socket.socket() as s:
    s.bind(('127.0.0.1', 0))
    port = s.getsockname()[1]
env['AIMEBU_PORT'] = str(port)
base = 'http://127.0.0.1:' + str(port)
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
csrf = ''
oldpid = None

def stamp():
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())

def cli(which, *args):
    p = subprocess.run([str(OLD if which == 'old' else NEW), *args], env=env, capture_output=True, text=True, timeout=120)
    print('CLI', stamp(), which, *args, 'exit', p.returncode, repr(p.stdout.strip()), repr(p.stderr.strip()), flush=True)
    if p.returncode:
        raise RuntimeError('CLI failed')
    return p.stdout

def req(method, path, data=None, key=None):
    headers = {}
    if data is not None: headers['Content-Type'] = 'application/json'
    if csrf and method != 'GET': headers['X-CSRF-Token'] = csrf
    if key: headers['Idempotency-Key'] = key
    request = urllib.request.Request(base+path, data=json.dumps(data).encode() if data is not None else None, headers=headers, method=method)
    try:
        response = opener.open(request, timeout=15)
        result = json.load(response)
        if (method != 'GET' and path != '/api/login') or path in ('/api/buildinfo','/api/machines'):
            print('HTTP', stamp(), method, path, response.status, json.dumps(result)[:700], flush=True)
        return result
    except urllib.error.HTTPError as e:
        print('HTTP_ERROR', stamp(), method, path, e.code, e.read().decode()[:700], flush=True)
        raise

def until(label, predicate, seconds=60):
    deadline = time.monotonic()+seconds
    while time.monotonic()<deadline:
        v = predicate()
        if v:
            print('PASS', stamp(), label, v, flush=True)
            return v
        time.sleep(.2)
    raise RuntimeError('timeout: '+label)

def alive(pid):
    p=subprocess.run(['ps','-o','stat=','-p',str(pid)],capture_output=True,text=True)
    return p.returncode==0 and p.stdout.strip() and not p.stdout.strip().startswith('Z')

def messages(room):
    return req('GET','/api/rooms/'+room+'/messages')

def answered(room, body):
    return any(m.get('body')==body and m.get('author_kind')=='agent' for m in messages(room))

try:
    old_version=cli('old','version').strip()
    new_version=cli('new','version').strip()
    cli('old','server','start')
    login=req('POST','/api/login',{'login':'owner','password':'correct horse battery staple'})
    csrf=login['csrf_token']
    project=req('POST','/api/projects',{'name':'upgrade-probe','folder':str(T/'project')})
    rooms=req('GET','/api/projects/'+project['id']+'/rooms')
    room=next(r['id'] for r in rooms if r['kind']=='task')
    until('old machine connected',lambda: any(m.get('connected') and m.get('supervisor_version')==old_version for m in req('GET','/api/machines')['machines']))
    machine=req('GET','/api/machines')['machines'][0]['id']
    created=req('POST','/api/machines/'+machine+'/launchers',{'tool':'claude','name':'scratch','command':str(T/'bin'/'claude'),'enabled':True,'supports_toolbox':True,'visible_roots':[str(T/'project')]})
    claude=created['config']
    req('PUT','/api/machines/'+machine+'/launchers/'+claude['id']+'/models',{'models':[{'slug':'haiku','label':'Haiku','default':True}]})
    until('scratch launcher available',lambda: any(row['config']['id']==claude['id'] and row['available'] for row in req('GET','/api/machines/'+machine+'/launchers')['launchers']))
    old=req('POST','/api/rooms/'+room+'/agents',{'name':'oldwork','role_key':'worker','tool':'claude','variant':'native','launcher_id':claude['id'],'model':'haiku'},'old-agent')
    msg=req('POST','/api/rooms/'+room+'/messages',{'body':'@oldwork old-release-probe'})
    until('old job started',lambda: (T/'job.id').exists())
    jobid=(T/'job.id').read_text()
    print('OLD_TURN', (T/'old.turn').read_text(), 'JOB',jobid,flush=True)
    oldpid=int(re.search(r'pid (\d+)',cli('old','supervisor','status')).group(1))
    assert alive(oldpid)
    cli('old','server','stop')
    assert alive(oldpid)
    cli('new','server','start')
    print('BUILD',req('GET','/api/buildinfo'),flush=True)
    until('new machine connected',lambda: any(m.get('connected') and m.get('supervisor_version')==new_version for m in req('GET','/api/machines')['machines']))
    print('OLD_ALIVE_DURING_NEW',oldpid,alive(oldpid),flush=True)
    assert alive(oldpid)
    print('JOURNALS_DURING_OVERLAP',[str(x) for x in (C/'supervisor').glob('**/*.sqlite')],flush=True)
    new=req('POST','/api/rooms/'+room+'/agents',{'name':'newwork','role_key':'reviewer','tool':'claude','variant':'native','launcher_id':claude['id'],'model':'haiku'},'new-agent')
    req('POST','/api/rooms/'+room+'/messages',{'body':'@newwork new-release-probe'})
    until('new answer',lambda: answered(room,'new-answered'))
    assert alive(oldpid)
    print('OLD_STILL_RUNNING',oldpid,alive(oldpid),flush=True)
    (T/'turn.release').touch()
    until('old answer',lambda: answered(room,'old-answered'))
    assert alive(oldpid)
    print('OLD_ALIVE_WITH_JOB',oldpid,alive(oldpid),flush=True)
    (T/'job.release').touch()
    def jobdone():
        return next((m['body'] for m in messages(room) if m.get('author_kind')=='system' and m.get('body','').startswith('Command upgrade-job: finished (exit 0')),None)
    until('job exit 0 note',jobdone)
    until('old supervisor exited',lambda: not alive(oldpid))
    until('old journal removed',lambda: not (C/'supervisor'/'journal.sqlite').exists())
    print('FINAL_MESSAGES',json.dumps(messages(room)),flush=True)
    print('FINAL_AGENTS',json.dumps(req('GET','/api/rooms/'+room+'/agents')),flush=True)
    events=req('GET','/api/system-events')['events']
    lost=[e['kind'] for e in events if 'lost' in e['kind'] or 'interrupt' in e['kind']]
    print('LOST_OR_INTERRUPTED_EVENTS',lost,flush=True)
    assert not lost
    print('FINAL_JOURNALS',[str(x) for x in (C/'supervisor').glob('**/*.sqlite')],flush=True)
    doctor=subprocess.run([str(NEW),'doctor','--json'],env=env,capture_output=True,text=True,timeout=30)
    print('DOCTOR_EXIT',doctor.returncode,flush=True)
    print('DOCTOR_SCHEMA',[x for x in json.loads(doctor.stdout)['checks'] if x['name']=='sqlite database'],flush=True)
    assert doctor.returncode==0
    print('LOG_EXCERPTS',flush=True)
    for name in ('server/aimebu.log','supervisor/aimebu.log'):
        p=C/name
        log=p.read_text(errors='replace')
        assert not any(x in log.lower() for x in ('unexpected machine frame','turn_lost','agent_keys'))
        print(name,flush=True)
        for line in log.splitlines():
            if any(x in line.lower() for x in ('supervisor','migration 14','turn answered','turn_lost','unexpected machine frame','agent_keys','command')):
                print(line[:700],flush=True)
finally:
    for which in ('new','old'):
        p=subprocess.run([str(OLD if which == 'old' else NEW),'server','stop','--with-supervisor'],env=env,capture_output=True,text=True,timeout=30)
        print('CLEANUP_CLI',which,p.returncode,repr(p.stdout.strip()),repr(p.stderr.strip()),flush=True)
    p=subprocess.run(['ps','-axo','pid,command'],capture_output=True,text=True)
    remaining=[line for line in p.stdout.splitlines() if len(line.split(maxsplit=1))==2 and line.split(maxsplit=1)[1].startswith((str(OLD),str(NEW),str(stub)))]
    print('CLEANUP_PROCESS_LINES',remaining,flush=True)
    assert not remaining
    shutil.rmtree(T)

PYCODE
