"""Artifacts are bound to the digest their producing job recorded."""
import importlib.util,os,re,subprocess,sys,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
HERE=Path(__file__).resolve().parent
ROOT=HERE.parents[1]
spec=importlib.util.spec_from_file_location('artifact_digest',HERE/'artifact-digest.py')
d=importlib.util.module_from_spec(spec);spec.loader.exec_module(d)
cspec=importlib.util.spec_from_file_location('candidate',HERE/'candidate.py')
c=importlib.util.module_from_spec(cspec);cspec.loader.exec_module(c)

def jobs(text):
    body=text.split('\njobs:\n',1)[1]
    return {part.split(':',1)[0]:part for part in re.split(r'\n  (?=[\w-]+:\n)','\n'+body) if part.strip()}

class ArtifactDigestTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name)

    def test_upload_and_download_layouts_agree(self):
        src=self.root/'src';src.mkdir();(src/'a').write_bytes(b'a');(src/'b').write_bytes(b'b')
        down=self.root/'down';down.mkdir();(down/'a').write_bytes(b'a');(down/'b').write_bytes(b'b')
        # Files of one directory upload as their basenames; a directory as its tree.
        self.assertEqual(d.tree_digest([str(src/'a'),str(src/'b')]),d.tree_digest([str(down)]))
        self.assertEqual(d.tree_digest([str(src)]),d.tree_digest([str(down)]))
        (src/'sub').mkdir();(src/'sub'/'c').write_bytes(b'c')
        self.assertNotEqual(d.tree_digest([str(src)]),d.tree_digest([str(down)]))

    def test_record_then_verify_and_refuse_changes(self):
        f=self.root/'release-lock.json';f.write_text('{}')
        out=self.root/'out'
        with patch.dict(os.environ,{'GITHUB_OUTPUT':str(out)}):d.record('release_lock_sha256',[str(f)])
        name,value=out.read_text().strip().split('=')
        self.assertEqual(name,'release_lock_sha256')
        d.verify(value,[str(f)])
        f.write_text('{"x":1}')
        with self.assertRaisesRegex(ValueError,'differs'):d.verify(value,[str(f)])
        # A producer output that never arrived fails closed.
        for expected in ['','0'*63,'Z'*64]:
            with self.assertRaisesRegex(ValueError,'expected'):d.verify(expected,[str(f)])

    def test_added_file_symlink_missing_and_empty_are_refused(self):
        a=self.root/'art';a.mkdir();(a/'x').write_bytes(b'x')
        value=d.tree_digest([str(a)])
        (a/'extra').write_bytes(b'')
        with self.assertRaisesRegex(ValueError,'differs'):d.verify(value,[str(a)])
        (a/'extra').unlink();(a/'link').symlink_to(a/'x')
        with self.assertRaisesRegex(ValueError,'Unexpected'):d.tree_digest([str(a)])
        with self.assertRaisesRegex(ValueError,'missing'):d.tree_digest([str(self.root/'none')])
        (self.root/'empty').mkdir()
        with self.assertRaisesRegex(ValueError,'empty'):d.tree_digest([str(self.root/'empty')])

    def test_cli_exits_nonzero_on_mismatch(self):
        f=self.root/'f';f.write_bytes(b'f')
        r=subprocess.run([sys.executable,str(HERE/'artifact-digest.py'),'verify','0'*64,str(f)],capture_output=True,text=True)
        self.assertNotEqual(r.returncode,0);self.assertIn('differs',r.stderr)

    def test_executed_release_binaries_never_see_the_signing_seed(self):
        with patch.dict(os.environ,{'JERBOA_MINISIGN_SEED':'seed','GH_TOKEN':'t','AWS_SECRET_ACCESS_KEY':'k','KEY_ID':'id','PATH':'/bin'}):
            env=c.built_binary_env()
        for name in ['JERBOA_MINISIGN_SEED','GH_TOKEN','AWS_SECRET_ACCESS_KEY']:
            self.assertNotIn(name,env)
        self.assertEqual(env['PATH'],'/bin')

class WorkflowBindsArtifacts(unittest.TestCase):
    def test_trusted_consumers_verify_downloads_before_use(self):
        text=(ROOT/'.github/workflows/release.yml').read_text()
        # A pattern download would also take an artifact another job added.
        self.assertNotRegex(text,r'(?m)^\s+pattern:')
        j=jobs(text)
        for name in ['kernel-stage','kernel-test','build-distro','windows','assemble']:
            with self.subTest(job=name):
                job=j[name]
                self.assertIn('artifact-digest.py verify',job)
                uses=[i for i in [job.find(m) for m in ['candidate.py','stage-candidate.mjs','validate-kernel.py','tar -xzf']] if i>=0]
                self.assertLess(job.index('artifact-digest.py verify'),min(uses))

    def test_producers_export_the_digest_of_what_they_upload(self):
        text=(ROOT/'.github/workflows/release.yml').read_text()
        j=jobs(text)
        # validated-kernel is the KVM runner's own upload: assembly compares it
        # with the verified selected toolset instead of trusting it.
        for name in ['inputs','kernel-stage','build-release','build-distro','build-macos','kernel-tools-arm64','windows']:
            with self.subTest(job=name):
                self.assertIn('artifact-digest.py record',j[name])
                self.assertRegex(j[name],r'\n    outputs:\n(      .*\n)*      \w+: \$\{\{ steps\.digest\.outputs\.')
        validate=(ROOT/'.github/workflows/validate.yml').read_text()
        self.assertIn('kernel_linux_sha256:\n        value: ${{ jobs.kernel-build.outputs.kernel_linux_sha256 }}',validate)
        self.assertEqual(validate.count('artifact-digest.py verify "$KERNEL_LINUX" kernel-linux.tar.gz'),2)

if __name__=='__main__':unittest.main()
