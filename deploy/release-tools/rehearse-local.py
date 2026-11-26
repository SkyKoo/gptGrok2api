#!/usr/bin/env python3
"""Exercise actual CFM processes on private localhost ports and disposable data.

Usage: python3 rehearse-local.py /path/to/built/gptgrok2api
No Docker, real credentials, remote services or production paths are used.
"""
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1',0)); return sock.getsockname()[1]


def request(url, body=None):
    req=urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None,
        headers={'Authorization':'Bearer fixture-admin','Content-Type':'application/json'})
    with urllib.request.urlopen(req,timeout=15) as res:
        return json.load(res) if res.headers.get_content_type()=='application/json' else res.read()


def wait(fn):
    end=time.monotonic()+15
    while time.monotonic()<end:
        try:
            value=fn()
            if value: return value
        except (OSError, ValueError): pass
        time.sleep(.02)
    raise AssertionError('local process failed to converge')


def manifest(root):
    return {str(p.relative_to(root)):hashlib.sha256(p.read_bytes()).hexdigest() for p in root.rglob('*')
        if p.is_file() and not p.name.startswith('.cfm-')}


def main(binary):
    procs={}; streams=[]
    with tempfile.TemporaryDirectory(prefix='cfm-local-handoff-') as temporary:
        root=Path(temporary); data=root/'data'; data.mkdir(); (root/'logs').mkdir()
        addresses={name:('http://127.0.0.1:'+str(port()),'http://127.0.0.1:'+str(port())) for name in ('front','blue','green')}
        def start(name):
            public, control=addresses[name]
            env=dict(os.environ, GROK_ROOT_DIR=str(root), GROK_DATA_DIR=str(data),
                GO_CONFIG_PATH=str(data/'config.json'), GO_ACCOUNTS_PATH=str(data/'accounts.json'),
                GO_AUTH_KEYS_PATH=str(data/'auth_keys.json'), GO_QUEUE_BACKEND='json', GO_DATABASE_URL='',
                GO_VERSION='test', CHATGPT2API_ADMIN_KEY='fixture-admin', CHATGPT2API_AUTH_KEY='fixture-api',
                CFM_ROLLING_CONTROL_ADDR=control.removeprefix('http://'), GO_LISTEN_ADDR=public.removeprefix('http://'),
                CFM_WRITER_LEASE_FILE=str(data/'.cfm-writer.lock'),
                CFM_ROLLING_STATE_FILE=str(root/(name+'.json')),
                CFM_ROLLING_BOOTSTRAP_ACTIVE=str(name=='front').lower(),
                CFM_GATEWAY_STATE_FILE=str(data/'.cfm-gateway.json') if name=='front' else '',
                CFM_GATEWAY_SLOT_URLS=','.join(addresses[n][0] for n in ('blue','green')))
            log=(root/(name+'.log')).open('ab'); streams.append(log)
            procs[name]=subprocess.Popen([binary],env=env,cwd=root,stdout=log,stderr=log)
            wait(lambda:request(control+'/status'))
        def ctl(name,action=None):
            return request(addresses[name][1]+('/'+action if action else '/status'),{} if action else None)
        def gate(action=None,target=''):
            state=request(addresses['front'][1]+'/gateway/status')
            if action: return request(addresses['front'][1]+'/gateway/'+action,dict(epoch=state['route']['epoch'],upstream=target))
            return state
        def stop(name):
            procs[name].terminate(); procs[name].wait(timeout=20)
        try:
            start('front'); wait(lambda:ctl('front').get('phase')=='active')
            front=addresses['front'][0]
            request(front+'/api/settings',{'rehearsal_before':'persisted before handoff'})
            start('blue'); start('green')
            assert ctl('blue')['phase']=='standby' and ctl('green')['phase']=='standby'
            # A standby cannot open even configuration while another writer exists.
            try: ctl('blue','activate'); raise AssertionError('two writers admitted')
            except urllib.error.HTTPError as err: assert err.code==409
            with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
                for old,new,bootstrap in [('front','blue',True),('blue','green',False)]:
                    ctl(old,'pause'); wait(lambda:ctl(old)['active_requests']==0 and all(v==0 for v in ctl(old)['jobs'].values()))
                    if bootstrap: ctl('front','gateway/init')
                    else: gate('hold')
                    ctl(old,'freeze'); assert not ctl(old)['writer_lease']
                    queued=[pool.submit(request, front+'/api/settings',{'rehearsal_'+new+'_'+str(i):'queued update'}) for i in range(12)]
                    wait(lambda:gate()['queued_requests']==12)
                    before=manifest(data)
                    ctl(new,'activate'); assert ctl(new)['writer_lease'] and ctl(new)['paused']
                    assert manifest(data)==before, 'initialization rewrote business files'
                    gate('route',addresses[new][0]); gate('release'); ctl(new,'resume')
                    for future in queued: future.result()
                    assert not gate()['route']['held']
                    if old!='front': stop(old)
            stop('front'); start('front')
            assert ctl('front')['phase']=='gateway' and not ctl('front')['writer_lease']
            stop('green'); start('green'); wait(lambda:ctl('green')['phase']=='active')
            rows=request(front+'/api/settings')['config']
            expected={'rehearsal_before'}|{'rehearsal_'+name+'_'+str(i) for name in ('blue','green') for i in range(12)}
            assert expected <= rows.keys(), sorted(expected-rows.keys())
            assert len([key for key in rows if key.startswith('rehearsal_')])==25
            print(json.dumps({'success':True,'handoffs':2,'queued_updates':24,'updates_preserved':25,
                'single_writer':True,'initialization_data_unchanged':True,'front_restart':True,'active_restart':True}))
        finally:
            for proc in procs.values():
                if proc.poll() is None:
                    proc.terminate()
                    try: proc.wait(timeout=20)
                    except subprocess.TimeoutExpired:
                        # Disposable local fixture only, never production.
                        proc.kill(); proc.wait()
            for log in streams: log.close()

if __name__=='__main__': main(str(Path(sys.argv[1]).resolve()))
