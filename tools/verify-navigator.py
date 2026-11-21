#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Real unmodified Navigator Work API -> bridge -> native Go gRPC verification."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
import hashlib
import urllib.request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--navigator-root', type=Path, required=True)
parser.add_argument('--plugin', type=Path, required=True)
args = parser.parse_args()
sys.path.insert(0, str(args.navigator_root.absolute() / 'src'))
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'integrations/navigator'))
from pilot_bridge import attach_pilot
from cyrene_navigator.persistence.api import create_persistence_app
from cyrene_navigator.persistence.store import PersistencePrincipal
from fastapi.testclient import TestClient


class NavigatorIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix='pilot-navigator-')
        cls.root = Path(cls.temp.name)
        data = b'{"instruction":"Call 13800138000","output":"ok"}\n'
        (cls.root / 'data.jsonl').write_bytes(data)
        (cls.root / 'operator.token').write_text('operator-012345678901234567890123456789')
        (cls.root / 'operator.token').chmod(0o600)
        cls.cfg = {'httpListen':'127.0.0.1:0','adminTokenFile':str(cls.root/'operator.token'),
                   'automation':{'datasets':{'data':{'path':str(cls.root/'data.jsonl'),
                       'digest':'sha256:'+hashlib.sha256(data).hexdigest()}},'models':{},'dataGuard':{}}}
        (cls.root/'config.json').write_text(json.dumps(cls.cfg))
        (cls.root/'state').mkdir(mode=0o700)
        cls.log = (cls.root/'plugin.log').open('w+')
        cls.child = subprocess.Popen([str(args.plugin.absolute()),'--capability','tool.provider.v1',
            '--interface-version','1','--listen','127.0.0.1:0','--config',str(cls.root/'config.json')],
            env={**os.environ,'CYRENE_PLUGIN_STORAGE_PATH':str(cls.root/'state'),
                 'CYRENE_CAPABILITY_BINDING_ID':'pilot'},stdout=subprocess.PIPE,stderr=cls.log,text=True)
        cls.addClassCleanup(lambda: cls.child.poll() is None and (cls.child.terminate(), cls.child.wait(timeout=10)))
        cls.ref = json.loads(cls.child.stdout.readline())['connection_ref']
        cls.bindings = [{'organization_id':'org','workspace_id':'workspace','binding_id':'pilot',
                         'connection_ref_env':'PILOT_REF'}]
        cls.principals = {
            'writer':PersistencePrincipal(actor_id='writer',organization_id='org',workspace_ids=frozenset({'workspace'}),can_takeover=True),
            'reader':PersistencePrincipal(actor_id='reader',organization_id='org',workspace_ids=frozenset({'workspace'})),
            'foreign':PersistencePrincipal(actor_id='foreign',organization_id='other',workspace_ids=frozenset({'workspace'}),can_takeover=True)}
        factory = attach_pilot(create_persistence_app,cls.bindings,{'PILOT_REF':cls.ref})
        cls.app = factory(cls.root/'navigator.db',cls.principals)
        cls.client = TestClient(cls.app)
        cls.client.__enter__()
        cls.base = '/api/v1/workspaces/workspace/work/tool-providers'

    @classmethod
    def tearDownClass(cls):
        cls.client.__exit__(None,None,None)
        cls.child.terminate()
        cls.child.wait(timeout=10)
        cls.child.stdout.close()
        cls.log.close()
        cls.temp.cleanup()

    def call(self, tool, payload, token='writer'):
        return self.client.post(self.base+'/pilot/tools/'+tool+'/call',
            headers={'Authorization':'Bearer '+token},json={'arguments':payload})

    def test_catalog_and_real_native_workflow(self):
        headers={'Authorization':'Bearer writer'}
        providers=self.client.get(self.base,headers=headers)
        self.assertEqual(providers.status_code,200,providers.text)
        self.assertIn('pilot',[p['bindingId'] for p in providers.json()['items']])
        catalog=self.client.get(self.base+'/pilot/tools',headers=headers)
        self.assertEqual(catalog.status_code,200,catalog.text)
        tools={t['id']:t for t in catalog.json()['tools']}
        self.assertFalse(tools['data_guard_start']['readOnly'])
        self.assertTrue(tools['pilot_job_status']['readOnly'])
        response=self.call('data_guard_start',{'datasetId':'data','idempotencyKey':'navigator-e2e'})
        self.assertEqual(response.status_code,200,response.text)
        job=response.json()['structuredContent']
        for _ in range(50):
            response=self.call('pilot_job_status',{'jobId':job['id']})
            self.assertEqual(response.status_code,200,response.text)
            job=response.json()['structuredContent']
            if job['state'] not in ('QUEUED','RUNNING'): break
            time.sleep(0.1)
        self.assertEqual(job['state'],'SUCCEEDED',job)
        self.assertEqual(job['summary']['findingCount'],1)
        self.assertEqual(job['summary']['reviewState'],'PENDING')

    def test_authorization_is_preserved(self):
        self.assertEqual(self.client.get(self.base).status_code,401)
        self.assertEqual(self.call('data_guard_start',{},'reader').status_code,403)
        self.assertEqual(self.call('data_guard_start',{},'foreign').status_code,404)
        self.assertEqual(self.call('not_allowed',{}).status_code,502)
        foreign=self.client.get(self.base,headers={'Authorization':'Bearer foreign'})
        self.assertEqual(foreign.json()['items'],[])

    def test_actual_launcher_and_http_discovery(self):
        launcher=Path(__file__).resolve().parents[1]/'integrations/navigator/serve.py'
        bindings=self.root/'bindings.json'
        bindings.write_text(json.dumps({'pilot_bindings':self.bindings}))
        principals=self.root/'principals.json'
        principals.write_text(json.dumps({'principals':[{'token_env':'NAV_TEST_TOKEN','actor_id':'writer',
            'organization_id':'org','workspace_ids':['workspace'],'can_takeover':True}]}))
        with (self.root/'launcher.log').open('w+') as log:
            child=subprocess.Popen([sys.executable,str(launcher),'--navigator-root',str(args.navigator_root),
                '--pilot-bindings',str(bindings),'--database',str(self.root/'launcher.db'),
                '--principal-config',str(principals)],env={**os.environ,'NAV_TEST_TOKEN':'test-writer-token',
                'PILOT_REF':self.ref},stdout=subprocess.PIPE,stderr=log,text=True)
            try:
                ready=json.loads(child.stdout.readline())
                url='http://127.0.0.1:'+str(ready['port'])+self.base+'/pilot/tools'
                for attempt in range(50):
                    try:
                        with urllib.request.urlopen(urllib.request.Request(url,headers={
                            'Authorization':'Bearer test-writer-token'}),timeout=2) as response:
                            catalog=json.load(response)
                        break
                    except OSError:
                        if attempt==49:raise
                        time.sleep(0.1)
                self.assertIn('data_guard_start',[t['id'] for t in catalog['tools']])
            finally:
                child.terminate();child.wait(timeout=10);child.stdout.close()

    def test_host_scope_and_tool_allowlists(self):
        duplicate=[*self.bindings,{**self.bindings[0],'workspace_id':'other-workspace'}]
        with self.assertRaises(ValueError):attach_pilot(create_persistence_app,duplicate,{'PILOT_REF':self.ref})
        with self.assertRaises(ValueError):attach_pilot(create_persistence_app,[{**self.bindings[0],'allowed_tools':['shell']}],{'PILOT_REF':self.ref})(self.root/'bad.db',self.principals)
        factory=attach_pilot(create_persistence_app,self.bindings,{'PILOT_REF':self.ref})
        with self.assertRaises(ValueError):factory(self.root/'reader.db',{'reader':self.principals['reader']})


unittest.main(argv=[sys.argv[0]],verbosity=2)
