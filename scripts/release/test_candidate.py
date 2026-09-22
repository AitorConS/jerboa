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

    def manifest(self):
        v='v1.0.0';asset={'url':c.BASE+'/cli/v1.0.0/a','sha256':'a'*64,'size':1}
        m={'components':{k:{'version':v,'platforms':{p:dict(asset) for p in platforms}} for k,platforms in c.REQUIRED.items()}}
        m['components']['distro']={'version':v,'kernel':'v0.2.0',**asset}
        m['components']['kernel']={'version':'v0.2.0','files':{'kernel':asset}}
        return m

    def test_incomplete_platform_blocks_promotion(self):
        m=self.manifest();c.assert_manifest(m,'v1.0.0')
        del m['components']['desktop']['platforms']['windows-amd64']
        with self.assertRaisesRegex(ValueError,'Missing'):c.assert_manifest(m,'v1.0.0')

    def test_mixed_versions_block_promotion(self):
        m=self.manifest();m['components']['desktop']['version']='v0.9.0'
        with self.assertRaisesRegex(ValueError,'Version'):c.assert_manifest(m,'v1.0.0')

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
                with patch.object(c,'verify_candidate',return_value=(inv,{})),patch.object(c,'existing',return_value=None),patch.object(c,'sign'),patch.object(c,'put',side_effect=lambda *a,**k:calls.append(a[1])),patch.object(c,'download',side_effect=RuntimeError('network failed')),patch.dict(os.environ,{'MACOS_EVIDENCE':'https://example.org/report','GITHUB_RUN_ID':'456'}):
                    with self.assertRaisesRegex(RuntimeError,'network'):c.promote()
                self.assertFalse(any(k.startswith('channels/') or k=='desktop/latest.yml' for k in calls))
            finally:os.chdir(old)

if __name__=='__main__':unittest.main()
