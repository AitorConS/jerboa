"""Contract tests for release ownership without third-party YAML dependencies."""
import hashlib,importlib.util,json,os,subprocess,sys,tempfile,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
class WorkflowContracts(unittest.TestCase):
 def test_only_promoter_advances_stable(self):
  workflows=ROOT/'.github/workflows'
  # The main CI and builder must not contain upload commands or invoke promote.
  for name in ['main.yml','validate.yml','release.yml']:
   text=(workflows/name).read_text()
   self.assertNotIn('candidate.py promote',text)
   self.assertNotIn('aws s3 cp',text)
   self.assertNotIn('publish-packages:',text)
  self.assertIn('cancel-in-progress: false',(workflows/'promote.yml').read_text())
  self.assertIn('environment: release',(workflows/'promote.yml').read_text())
 def test_selected_kernel_is_boot_gated_and_shared_by_packagers(self):
  text=(ROOT/'.github/workflows/release.yml').read_text()
  stage=text.split('  kernel-stage:',1)[1].split('  kernel-test:',1)[0]
  boot=text.split('  kernel-test:',1)[1].split('  build-release:',1)[0]
  self.assertIn('name: kernel-linux',stage)
  self.assertIn('candidate.py kernel',stage)
  self.assertIn('environment: kvm',boot)
  self.assertIn('validate-kernel.py',boot)
  self.assertIn('name: validated-kernel',boot)
  for job,next_job in [('build-distro','build-macos'),('assemble',None)]:
   part=text.split('  '+job+':',1)[1]
   if next_job:part=part.split('  '+next_job+':',1)[0]
   self.assertIn('    - kernel-test',part)
   self.assertIn('name: validated-kernel',part)
  self.assertIn('JERBOA_KERNEL_TOOLSET: kernel-candidate',text)
  self.assertNotIn('default: v0.2.0',text)
 def test_scope_handles_docs_and_kernel(self):
  spec=importlib.util.spec_from_file_location('scope',Path(__file__).with_name('ci-select.py'))
  scope=importlib.util.module_from_spec(spec);spec.loader.exec_module(scope)
  self.assertEqual(scope.classify(['docs/foo.md']),(False,False))
  self.assertEqual(scope.classify(['internal/api/client.go']),(True,False))
  self.assertEqual(scope.classify(['kernel/platform/pc/main.c']),(True,True))
  self.assertEqual(scope.classify(['tests/integration/lifecycle_test.go']),(True,True))
  self.assertEqual(scope.classify(['tests/e2e/build_test.go']),(True,True))
  self.assertEqual(scope.classify(['.github/workflows/release.yml']),(True,True))
 def test_package_matrix_builds_each_arch(self):
  with tempfile.TemporaryDirectory() as d:
   out=Path(d)/'out'
   subprocess.run([sys.executable,'scripts/release/select-packages.py'],cwd=ROOT,check=True,env={**os.environ,'PACKAGE':'node','GITHUB_OUTPUT':str(out)})
   matrix=json.loads(out.read_text().splitlines()[0].split('=',1)[1])['include']
  by_arch={(m['version'],m['arch']):m for m in matrix}
  self.assertEqual(by_arch[('20.11.0','arm64')]['runner'],'ubuntu-24.04-arm')
  self.assertIn('linux-arm64',by_arch[('20.11.0','arm64')]['source_url'])
  self.assertIn('linux-x64',by_arch[('20.11.0','amd64')]['source_url'])
  self.assertTrue(all('arm64' not in m for m in matrix))
  text=(ROOT/'.github/workflows/packages.yml').read_text()
  self.assertIn('image: ubuntu@sha256:',text)
  self.assertIn('runs-on: ${{ matrix.runner }}',text)
  self.assertNotIn("-name '*.tar.gz' -exec gh release upload",text)
  publish=text[text.index('  publish-index:'):text.index('  select:')]
  self.assertIn('environment: release',publish)
  self.assertIn('bin/jerboa-sign -in dist/publish/packages.json',publish)
  self.assertIn('packages.json.minisig',publish)
 def test_package_index_is_immutable(self):
  script=ROOT/'scripts/package-index.py'
  def stage(root,data,content):
   d=Path(root)/'pkg'/'linux-amd64';d.mkdir(parents=True)
   (d/'files.tar.gz').write_bytes(data)
   (d/'meta.json').write_text(json.dumps({'name':'pkg','version':'1.0.0','platform':'linux/amd64','sha256':hashlib.sha256(data).hexdigest(),'size':len(data),'content_digest':content}))
  def publish(src,dst):
   return subprocess.run([sys.executable,str(script),src,dst,'o/r'],capture_output=True,text=True)
  with tempfile.TemporaryDirectory() as a,tempfile.TemporaryDirectory() as b,tempfile.TemporaryDirectory() as c,tempfile.TemporaryDirectory() as dst:
   stage(a,b'first','sha256:'+'1'*64)
   r=publish(a,dst);self.assertEqual(r.returncode,0,r.stderr)
   self.assertEqual(r.stdout.split(),['pkg-1.0.0-linux-amd64.tar.gz'])
   before=(Path(dst)/'packages.json').read_text()
   # Same content in a different archive: kept, nothing new to upload.
   stage(b,b'repacked','sha256:'+'1'*64)
   r=publish(b,dst);self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(r.stdout,'')
   self.assertEqual((Path(dst)/'packages.json').read_text(),before)
   # Different content under the same version: refused, index untouched.
   stage(c,b'changed','sha256:'+'2'*64)
   r=publish(c,dst);self.assertNotEqual(r.returncode,0);self.assertIn('Refusing to republish',r.stderr)
   self.assertEqual((Path(dst)/'packages.json').read_text(),before)
if __name__=='__main__':unittest.main()
