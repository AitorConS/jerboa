import importlib.util,json,os,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
spec=importlib.util.spec_from_file_location('candidate',Path(__file__).with_name('candidate.py'))
c=importlib.util.module_from_spec(spec);spec.loader.exec_module(c)

class ProtocolTests(unittest.TestCase):
    def test_version_and_paths_reject_ambiguous_input(self):
        for value in ['latest','0.54.2','v1.2.3/evil','v1.2.3-rc1']:
            with self.assertRaises(ValueError):c.version(value)
        for value in ['../secret','/etc/passwd','x/../../a','x\\a','x//a']:
            with self.assertRaises(ValueError):c.safe_path(value)
        self.assertLess(c.version_tuple('v0.9.0'),c.version_tuple('v0.10.0'))

    def test_published_version_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as td:
            p=Path(td)/'file';p.write_bytes(b'new');old=Path(td)/'old';old.write_bytes(b'old')
            with patch.object(c,'existing',return_value=old),patch.object(c,'aws') as aws:
                with self.assertRaisesRegex(ValueError,'overwrite'):c.put(p,'cli/v1.0.0/file')
                aws.assert_not_called()

    def test_retry_reuses_identical_immutable_artifact(self):
        with tempfile.TemporaryDirectory() as td:
            p=Path(td)/'file';p.write_bytes(b'same')
            with patch.object(c,'existing',return_value=p),patch.object(c,'aws') as aws:
                c.put(p,'cli/v1.0.0/file');aws.assert_not_called()

    def test_missing_object_is_uploaded(self):
        with patch.object(c,'existing',return_value=None),patch.object(c,'aws') as aws,patch.dict(os.environ,{'BUCKET':'test'}):
            c.put('file','cli/v1.0.0/file');self.assertEqual(aws.call_count,1)

    def test_auth_errors_do_not_authorize_overwrites(self):
        import subprocess
        with patch.dict(os.environ,{'BUCKET':'test','ENDPOINT':'https://example.invalid'}),patch.object(c.subprocess,'run',return_value=subprocess.CompletedProcess([],1,'','AccessDenied')):
            with self.assertRaisesRegex(RuntimeError,'inspect'):c.existing('key','temp')

    def test_installer_alias_uses_candidate_and_checks_public_bytes(self):
        with tempfile.TemporaryDirectory() as td:
            root=Path(td);key='releases/v1.0.0/install.sh'
            source=root/'payload'/key;source.parent.mkdir(parents=True)
            source.write_bytes(b'#!/bin/sh\n# candidate installer\n')
            inventory={'version':'v1.0.0','assets':{key:c.digest(source)}}
            def download(url,target):
                self.assertEqual(url,c.BASE+'/install.sh')
                Path(target).write_bytes(source.read_bytes())
            with patch.object(c,'put') as put,patch.object(c,'download',side_effect=download):
                c.publish_installer(root,inventory)
                put.assert_called_once_with(source,'install.sh',False,'text/plain')
            with patch.object(c,'put'),patch.object(c,'download',side_effect=lambda url,p:Path(p).write_bytes(b'stale')):
                with self.assertRaisesRegex(ValueError,'Public installer mismatch'):
                    c.publish_installer(root,inventory)
            source.write_bytes(b'changed after signing')
            with patch.object(c,'put') as put:
                with self.assertRaisesRegex(ValueError,'Candidate installer bytes changed'):
                    c.publish_installer(root,inventory)
                put.assert_not_called()

    def test_legacy_candidate_does_not_publish_checkout_installer(self):
        with patch.object(c,'put') as put:
            c.publish_installer(Path('unused'),{'version':'v1.0.0','assets':{}})
            put.assert_not_called()

    def manifest(self):
        v='v1.0.0';asset={'url':c.BASE+'/cli/v1.0.0/a','sha256':'a'*64,'size':1}
        m={'components':{k:{'version':v,'platforms':{p:dict(asset) for p in platforms}} for k,platforms in c.REQUIRED.items()}}
        m['components']['distro']={'version':v,'kernel':'v0.2.0',**asset}
        m['components']['kernel']={'version':'v0.2.0','files':{'kernel':asset}}
        return m

    def test_installer_is_pinned_to_its_release_manifest(self):
        with tempfile.TemporaryDirectory() as td:
            source=Path(td)/'install.sh'; target=Path(td)/'pinned.sh'
            source.write_text('#!/bin/sh\nMIN_GENERATED=""\necho\n')
            c.pin_installer(source,target,'2026-10-26T10:00:00Z')
            self.assertEqual(target.read_text(),'#!/bin/sh\nMIN_GENERATED="2026-10-26T10:00:00Z"\necho\n')
            source.write_text('#!/bin/sh\n')
            with self.assertRaisesRegex(ValueError,'MIN_GENERATED'):
                c.pin_installer(source,target,'2026-10-26T10:00:00Z')

    def test_incomplete_platform_blocks_promotion(self):
        m=self.manifest();c.assert_manifest(m,'v1.0.0')
        del m['components']['desktop']['platforms']['windows-amd64']
        with self.assertRaisesRegex(ValueError,'Missing'):c.assert_manifest(m,'v1.0.0')

    def test_mixed_versions_block_promotion(self):
        m=self.manifest();m['components']['desktop']['version']='v0.9.0'
        with self.assertRaisesRegex(ValueError,'Version'):c.assert_manifest(m,'v1.0.0')

    def test_promotion_refuses_an_older_manifest(self):
        old={'generated':'2026-10-21T10:00:00Z'}
        c.check_generation({}, {})
        c.check_generation({}, {'generated':'2026-10-01T10:00:00Z'})
        c.check_generation(old, {'generated':'2026-10-21T10:00:00Z'})
        c.check_generation(old, {'generated':'2026-10-22T10:00:00Z'})
        for new in [{}, {'generated':'2026-10-20T10:00:00Z'}]:
            with self.assertRaisesRegex(ValueError,'generated before'):c.check_generation(old,new)
        self.assertRegex(c.generated_now(), r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$')

    def test_promotion_rejects_failed_or_untrusted_run(self):
        with patch.dict(os.environ,{'CANDIDATE_RUN':'1','GITHUB_REPOSITORY':'AitorConS/jerboa','MACOS_EVIDENCE':'https://example.org/report'}):
            for field,value in [('conclusion','failure'),('head_branch','feature'),('event','pull_request'),('path','.github/workflows/main.yml')]:
                result={'path':'.github/workflows/release.yml','event':'workflow_dispatch','conclusion':'success','head_branch':'main'};result[field]=value
                with patch.object(c,'api',return_value=result),self.assertRaises(ValueError):c.authorize()

    def test_promote_stages_and_verifies_before_updating_pointers(self):
        inv={'version':'v1.0.0','run_id':'123','engine_sha':'a'*40,'assets':{'cli/v1.0.0/a':{'sha256':'a'*64,'size':1}}}
        calls=[]
        with tempfile.TemporaryDirectory() as td:
            old=os.getcwd();os.chdir(td)
            try:
                Path('authorized-run.json').write_text(json.dumps({'run_id':'123','engine_sha':'a'*40}))
                with patch.object(c,'verify_candidate',return_value=(inv,{'channel':'candidate'})),patch.object(c,'macos_evidence'),patch.object(c,'existing',return_value=None),patch.object(c,'sign'),patch.object(c,'put',side_effect=lambda *a,**k:calls.append(a[1])),patch.object(c,'download',side_effect=RuntimeError('network failed')),patch.dict(os.environ,{'MACOS_EVIDENCE':'https://example.org/report','GITHUB_RUN_ID':'456'}):
                    with self.assertRaisesRegex(RuntimeError,'network'):c.promote()
                self.assertFalse(any(k.startswith('channels/') or k=='desktop/latest.yml' for k in calls))
            finally:os.chdir(old)

    def test_resume_after_manifest_pointer_failure_reuses_candidate(self):
        # Exercise the real inventory checks and immutable writes against an
        # in-memory object store, including a failure between detached pointers.
        with tempfile.TemporaryDirectory() as td:
            old=os.getcwd();os.chdir(td)
            try:
                root=Path('candidate');payload=root/'payload/cli/v1.0.0/a'
                payload.parent.mkdir(parents=True);payload.write_bytes(b'verified release bytes')
                m=self.manifest();m['channel']='candidate'
                for asset in c.assets(m):asset.update(c.digest(payload))
                c.write_json(root/'manifest.json',m)
                (root/'latest.yml').write_text('version: 1.0.0\n')
                m['generated']='2026-10-26T10:00:00Z';c.write_json(root/'manifest.json',m)
                notes=root/'payload/releases/v1.0.0/notes.md';notes.parent.mkdir(parents=True);notes.write_text('### Fixed\n\n- A thing.\n')
                inv={'version':'v1.0.0','run_id':'123','engine_sha':'a'*40,'pipeline':c.PIPELINE,
                     # Older than the candidate's own manifest: a retry that meets
                     # that manifest as the live one must still go through.
                     'min_stable_generated':'2026-10-20T10:00:00Z','native_macos_app':{'sha256':'b'*64,'size':1},
                     'assets':{'cli/v1.0.0/a':c.digest(payload),'releases/v1.0.0/notes.md':c.digest(notes)},
                     'manifest':c.digest(root/'manifest.json'),'feed':c.digest(root/'latest.yml')}
                c.write_json(root/'inventory.json',inv)
                c.write_json('docs/releases/v1.0.0/native-macos.json',{'candidate_run':'123','app_sha256':'b'*64,'version':'v1.0.0','result':'pass'})
                for name in ['manifest.json','inventory.json']:(root/(name+'.minisig')).write_bytes(b'signature')
                c.write_json('authorized-run.json',{'run_id':'123','engine_sha':'a'*40})
                store={};writes=[];fail_once=[True]
                def existing(key,target):
                    if key not in store:return None
                    Path(target).write_bytes(store[key]);return Path(target)
                def aws(command,*args):
                    self.assertEqual(command,'put-object')
                    key=args[args.index('--key')+1]
                    if key=='channels/stable.json.minisig' and fail_once[0]:
                        fail_once[0]=False;raise RuntimeError('interrupted pointer update')
                    store[key]=Path(args[args.index('--body')+1]).read_bytes();writes.append(key)
                def download(url,target):Path(target).write_bytes(store[url[len(c.BASE)+1:]])
                def sign(path,comment):Path(str(path)+'.minisig').write_bytes(b'signed:'+Path(path).read_bytes())
                with patch.object(c,'existing',side_effect=existing),patch.object(c,'aws',side_effect=aws),patch.object(c,'download',side_effect=download),patch.object(c,'sign',side_effect=sign),patch.object(c,'verify_signature'),patch.dict(os.environ,{'BUCKET':'test','MACOS_EVIDENCE':'https://example.org/report','GITHUB_RUN_ID':'456'}):
                    with self.assertRaisesRegex(RuntimeError,'interrupted'):c.promote()
                    self.assertIn('channels/stable.json',store)
                    self.assertNotIn('desktop/latest.yml',store)
                    c.promote()
                    # Stable is the candidate re-signed for its channel at promotion.
                    stable=json.loads(store['channels/stable.json'])
                    self.assertEqual(stable,{**m,'channel':'stable'})
                    self.assertEqual(store['channels/stable.json.minisig'],b'signed:'+store['channels/stable.json'])
                    self.assertEqual(store['releases/v1.0.0/manifest.json'],store['channels/stable.json'])
                    self.assertEqual(store['releases/v1.0.0/candidate-manifest.json'],(root/'manifest.json').read_bytes())
                    self.assertEqual(store['desktop/latest.yml'],(root/'latest.yml').read_bytes())
                    self.assertEqual(store['releases/v1.0.0/notes.md'],notes.read_bytes())
                    published=store['releases/v1.0.0/published.json']
                    self.assertEqual(json.loads(published)['manifest_sha256'],c.digest(Path('stable-manifest.json'))['sha256'])
                    c.promote()
                    self.assertEqual(store['releases/v1.0.0/published.json'],published)
                    self.assertEqual(writes.count('cli/v1.0.0/a'),1)
            finally:os.chdir(old)

    def test_stable_floor_comes_from_the_live_manifest(self):
        with tempfile.TemporaryDirectory() as td:
            source=Path(td)/'freshness.go'
            with patch.object(c,'FLOOR_SOURCE',source):
                source.write_text('package release\n\nvar MinStableGenerated = "2026-11-13T18:27:12Z"\n')
                self.assertEqual(c.stable_floor({'generated':'2026-11-13T19:07:13Z'}),'2026-11-13T19:07:13Z')
                self.assertEqual(c.stable_floor({'generated':'2026-11-13T18:27:12Z'}),'2026-11-13T18:27:12Z')
                # A committed value beyond the live manifest would make source builds refuse stable.
                with self.assertRaisesRegex(ValueError,'later than the live stable manifest.*Set it to 2026-11-13T18:00:00Z or earlier'):
                    c.stable_floor({'generated':'2026-11-13T18:00:00Z'})
                for bad in [{},{'generated':'2026-11-13'},{'generated':'2026-11-13T19:07:13+00:00'}]:
                    with self.assertRaisesRegex(ValueError,'Expected a UTC time'):c.stable_floor(bad)
                source.write_text('package release\n')
                with self.assertRaisesRegex(ValueError,'no MinStableGenerated'):c.stable_floor({'generated':'2026-11-13T19:07:13Z'})
        # The repository's own declaration is the one lock reads.
        with patch.object(c,'FLOOR_SOURCE',Path(__file__).resolve().parents[2]/c.FLOOR_SOURCE):
            self.assertRegex(c.committed_floor(),r'^\d{4}-')

    def lock(self,stable,runtime=None,notes=None,**env):
        compare,checks=runtime or ({'status':'ahead'},{'check_runs':[{'name':'tests','status':'completed','conclusion':'success'},{'name':'ab','status':'completed','conclusion':'skipped'}]})
        def api(path):return compare if '/compare/' in path else checks
        def download(url,target):
            if url.endswith('.minisig'):Path(target).write_bytes(b'signature')
            else:c.write_json(target,stable)
        old=os.getcwd();root=Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as td:
            os.chdir(td)
            try:
                Path('VERSION.md').write_text('1.0.0\n');Path('kernel').mkdir();Path('kernel/VERSION').write_text('0.2.1\n')
                values={'VERSION':'v1.0.0','KERNEL_SOURCE':'build','KERNEL_VERSION':'v0.2.1','GITHUB_SHA':'a'*40,'DESKTOP_SHA':'b'*40,
                        'RUNTIME_SHA':'c'*40,'GITHUB_RUN_ID':'7','GITHUB_REPOSITORY':'AitorConS/jerboa','GITHUB_OUTPUT':'out',**env}
                with patch.dict(os.environ,values),patch.object(c,'FLOOR_SOURCE',root/c.FLOOR_SOURCE),patch.object(c,'api',side_effect=api) as called,patch.object(c,'release_notes',side_effect=notes) as noted,\
                        patch.object(c,'download',side_effect=download),patch.object(c,'verify_signature') as verify:
                    c.lock()
                    verify.assert_called_once();noted.assert_called_once_with('v1.0.0')
                    self.assertIn(f'repos/{c.RUNTIME_REPOSITORY}/compare/{"c"*40}...main',[a.args[0] for a in called.call_args_list])
                return json.loads(Path('release-lock.json').read_text()),Path('out').read_text()
            finally:os.chdir(old)

    def stable(self,generated='2026-11-13T19:07:13Z',channel='stable'):
        file={'url':'u','sha256':'a'*64,'size':3}
        return {'channel':channel,'generated':generated,'components':{'cli':{'version':'v0.9.0'},
                'kernel':{'version':'v0.2.0','files':{name:dict(file) for name in c.KERNEL_FILES}}}}

    def test_lock_records_the_floor_and_the_published_kernel(self):
        lock,out=self.lock(self.stable())
        self.assertEqual(lock['min_stable_generated'],'2026-11-13T19:07:13Z')
        self.assertIn('min_stable_generated=2026-11-13T19:07:13Z\n',out)
        self.assertEqual(lock['stable_kernel'],{'version':'v0.2.0','files':{f:{'sha256':'a'*64,'size':3} for f in c.KERNEL_FILES.values()}})
        # Only the signed stable manifest sets the floor, and it must be dated.
        with self.assertRaisesRegex(ValueError,'stable channel'):self.lock(self.stable(channel='candidate'))
        with self.assertRaisesRegex(ValueError,'Expected a UTC time'):self.lock(self.stable(generated=None))
        with self.assertRaisesRegex(ValueError,'later than the live stable manifest'):self.lock(self.stable(generated='2020-01-01T00:00:00Z'))

    def test_lock_refuses_a_version_without_a_changelog_entry(self):
        def missing(v):raise ValueError('CHANGELOG.md has no usable entry for 1.0.0')
        with self.assertRaisesRegex(ValueError,'no usable entry'):self.lock(self.stable(),notes=missing)

    @unittest.skipUnless(c.CHANGELOG_SCRIPT.exists(),'scripts/release/changelog.py is not in this checkout')
    def test_release_notes_are_the_changelog_entry(self):
        link='https://example.org/'
        text=f'# Changelog\n\n## [Unreleased]\n\n## [1.0.0] - 2026-02-10\n\n### Fixed\n\n- A thing (#1).\n\n[Unreleased]: {link}\n[1.0.0]: {link}\n'
        with tempfile.TemporaryDirectory() as td:
            log=Path(td)/'CHANGELOG.md';log.write_text(text)
            self.assertEqual(c.release_notes('v1.0.0',log),'### Fixed\n\n- A thing (#1).\n')
            # No entry, an empty one, a malformed file and no file all stop the
            # release and name the command that writes the entry.
            for version,content in [('v1.1.0',text),('v1.0.0',text.replace('### Fixed\n\n- A thing (#1).\n\n','')),('v1.0.0',text.replace('# Changelog','# Notes')),('v1.0.0',None)]:
                if content is None:log.unlink()
                else:log.write_text(content)
                with self.subTest(version=version),self.assertRaisesRegex(ValueError,r'python3 scripts/release/prepare-release\.py '+version[1:].replace('.','\\.')):
                    c.release_notes(version,log)

    def test_lock_requires_a_validated_runtime_commit(self):
        ok={'name':'tests','status':'completed','conclusion':'success'}
        for compare,runs,message in [
                ({'status':'diverged'},[ok],'not on the main branch'),({'status':'behind'},[ok],'not on the main branch'),
                ({'status':'identical'},[],'no successful check run'),
                ({'status':'ahead'},[{'name':'skip','status':'completed','conclusion':'skipped'}],'no successful check run'),
                ({'status':'ahead'},[ok,{'name':'macOS','status':'completed','conclusion':'failure'}],'macOS: failure'),
                ({'status':'ahead'},[ok,{'name':'macOS','status':'in_progress','conclusion':None}],'macOS: in_progress')]:
            with self.subTest(message=message),self.assertRaisesRegex(ValueError,message):
                self.lock(self.stable(),(compare,{'check_runs':runs}))
        self.lock(self.stable(),({'status':'identical'},{'check_runs':[ok,{'name':'n','status':'completed','conclusion':'neutral'}]}))

    def test_promotion_refuses_a_floor_a_later_release_overtook(self):
        inv={'min_stable_generated':'2026-11-13T19:07:13Z'}
        c.check_floor({},inv)
        c.check_floor({'generated':'2026-11-13T19:07:13Z'},inv)
        c.check_floor({'generated':'2026-11-01T00:00:00Z'},inv)
        with self.assertRaisesRegex(ValueError,'2026-11-14T00:00:00Z was promoted after this candidate was built.*rebuild the candidate'):
            c.check_floor({'generated':'2026-11-14T00:00:00Z'},inv)

    def test_floor_is_looked_for_in_every_released_binary(self):
        with tempfile.TemporaryDirectory() as td:
            payload=Path(td);m=self.manifest();m['generated']='2026-11-14T00:00:00Z';floor='2026-11-13T19:07:13Z'
            for name in ['cli','daemon']:
                for platform,asset in m['components'][name]['platforms'].items():
                    asset['url']=f'{c.BASE}/{name}/v1.0.0/{platform}'
                    p=payload/name/'v1.0.0'/platform;p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(b'\x7fELF'+floor.encode()+b'\0')
            c.verify_floor_embedded(m,payload,floor)
            (payload/'cli/v1.0.0/darwin-arm64').write_bytes(b'built without the flag 2026-11-12T20:06:52Z')
            with self.assertRaisesRegex(ValueError,'cli darwin-arm64 was not built with the stable floor.*MinStableGenerated='+floor):
                c.verify_floor_embedded(m,payload,floor)

    def test_candidate_is_never_signed_as_stable(self):
        with tempfile.TemporaryDirectory() as td:
            m=self.manifest();m['channel']='candidate';m['generated']='2026-10-26T10:00:00Z'
            stable=c.stable_manifest(m,Path(td)/'stable.json')
            self.assertEqual(json.loads(stable.read_text()),{**m,'channel':'stable'})
            # Deterministic: a retried promotion re-creates the same bytes.
            again=c.stable_manifest(m,Path(td)/'again.json')
            self.assertEqual(stable.read_bytes(),again.read_bytes())
            for channel in ['stable',None,'beta']:
                with self.subTest(channel=channel),self.assertRaisesRegex(ValueError,'candidate manifest'):
                    c.stable_manifest({**m,'channel':channel},Path(td)/'x.json')

if __name__=='__main__':unittest.main()
