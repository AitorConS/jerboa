"""Contract tests for release ownership without third-party YAML dependencies."""
import hashlib,importlib.util,json,os,re,subprocess,sys,tempfile,unittest
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
  # A release names the hypervisor commit it ships: a default goes stale.
  self.assertNotIn('default:',text.split('      runtime_sha:',1)[1].split('      kernel_version:',1)[0])
 def test_self_hosted_jobs_never_run_fork_code(self):
  workflows=ROOT/'.github/workflows'
  fork="github.event.pull_request.head.repo.full_name == github.repository"
  for path in sorted(workflows.glob('*.yml')):
   text=path.read_text()
   triggers=text.split('jobs:',1)[0]
   self.assertNotIn('pull_request_target',triggers)
   for job in re.split(r'\n  (?=[\w-]+:\n)',text.split('\njobs:\n',1)[1]):
    if 'self-hosted' not in job:continue
    with self.subTest(workflow=path.name,job=job.split(':',1)[0]):
     # Jobs that pull requests can reach are gated on the kvm environment
     # and on the pull request's branch living in this repository.
     if 'pull_request' in triggers or 'workflow_call' in triggers:
      self.assertIn('environment: kvm',job)
      self.assertIn(fork,job)
  # Release assembly takes the kernel from the GitHub-hosted selection; the
  # KVM runner's upload may only add its boot report.
  text=(workflows/'release.yml').read_text()
  for job,next_job in [('build-distro','build-macos'),('assemble',None)]:
   part=text.split('  '+job+':',1)[1]
   if next_job:part=part.split('  '+next_job+':',1)[0]
   self.assertIn('name: selected-kernel',part)
  self.assertIn('candidate.py selected',text.split('  build-distro:',1)[1].split('  build-macos:',1)[0])
 def test_scope_handles_docs_and_kernel(self):
  spec=importlib.util.spec_from_file_location('scope',Path(__file__).with_name('ci-select.py'))
  scope=importlib.util.module_from_spec(spec);spec.loader.exec_module(scope)
  self.assertEqual(scope.classify(['docs/foo.md']),(False,False))
  self.assertEqual(scope.classify(['internal/api/client.go']),(True,False))
  self.assertEqual(scope.classify(['kernel/platform/pc/main.c']),(True,True))
  self.assertEqual(scope.classify(['tests/integration/lifecycle_test.go']),(True,True))
  self.assertEqual(scope.classify(['tests/e2e/build_test.go']),(True,True))
  # What the end-to-end suite covers runs it before the merge.
  for path in ['cmd/jerboa/run.go','cmd/jerboad/main.go','internal/vm/qemu.go','internal/network/guard_linux.go','internal/builder/sandbox.go']:
   self.assertEqual(scope.classify([path]),(True,True),path)
  self.assertEqual(scope.classify(['.github/workflows/release.yml']),(True,True))
  self.assertEqual(scope.classify(['tests/upgrade/guest.sh']),(True,True))
  self.assertEqual(scope.classify(['scripts/install.sh']),(True,True))
 def test_upgrade_from_stable_gates_validation_and_candidates(self):
  text=(ROOT/'.github/workflows/validate.yml').read_text()
  job=text.split('  upgrade-e2e:\n',1)[1].split('  release-tests:\n',1)[0]
  self.assertIn('tests/upgrade/run.py',job)
  self.assertIn('environment: kvm',job)
  # The installer never runs on the runner itself.
  self.assertNotIn('install.sh',job)
  self.assertNotIn('sudo ',job.replace('sudo usermod',''))
  result=text.split('  result:\n',1)[1]
  self.assertIn('    - upgrade-e2e\n',result.split('    steps:',1)[0])
  # A release candidate runs the same validation in full before anything
  # is built for it.
  release=(ROOT/'.github/workflows/release.yml').read_text()
  self.assertIn('uses: ./.github/workflows/validate.yml\n    with:\n      full: true',release)
  for name in ['build-release','kernel-stage']:
   needs=release.split('  '+name+':\n',1)[1].split('    steps:',1)[0]
   self.assertIn('validate',needs)
  # The state-compatibility tests run with the unit tests.
  self.assertIn('go test ./cmd/... ./internal/...',text)
  guest=(ROOT/'tests/upgrade/guest.sh').read_text()
  self.assertIn('https://releases.jerboa.dev/install.sh',guest)
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
  # The existing index is verified before it is extended and signed again.
  # Only a signature made for the package index counts, and the base
  # index may not be older than the last publication.
  verify=publish.index('bin/jerboa-verify -purpose packages dist/publish/packages.json')
  # An unset variable is a record of 0, not "no check".
  self.assertIn("PKG_INDEX_MIN_SERIAL: ${{ vars.PKG_INDEX_MIN_SERIAL || '0' }}",publish)
  self.assertNotIn('PKG_INDEX_MIN_SERIAL: ${{ vars.PKG_INDEX_MIN_SERIAL }}',publish)
  # The published serial is recorded by the job when it holds a token that
  # may, and otherwise the summary carries the command.
  record=publish[publish.index('    - name: Record the published serial'):]
  self.assertLess(publish.index('cmp dist/publish/packages.json dist/public/packages.json'),publish.index('    - name: Record the published serial'))
  self.assertIn('SERIAL: ${{ steps.publish.outputs.serial }}',record)
  self.assertIn('VARIABLES_TOKEN: ${{ secrets.PKG_INDEX_VARIABLES_TOKEN }}',record)
  self.assertIn('GH_TOKEN="$VARIABLES_TOKEN" gh variable set PKG_INDEX_MIN_SERIAL --body "$SERIAL"',record)
  self.assertIn('::warning::',record)
  self.assertIn('GITHUB_STEP_SUMMARY',record)
  self.assertLess(publish.index('--pattern packages.json.minisig'),verify)
  self.assertLess(verify,publish.index('scripts/package-index.py'))
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
 def test_package_index_refuses_foreign_urls(self):
  script=ROOT/'scripts/package-index.py'
  with tempfile.TemporaryDirectory() as src,tempfile.TemporaryDirectory() as dst:
   d=Path(src)/'pkg'/'linux-amd64';d.mkdir(parents=True)
   (d/'files.tar.gz').write_bytes(b'new')
   (d/'meta.json').write_text(json.dumps({'name':'pkg','version':'2.0.0','platform':'linux/amd64','sha256':hashlib.sha256(b'new').hexdigest(),'size':3}))
   good='https://github.com/o/r/releases/download/pkg-index/old-1.0.0-linux-amd64.tar.gz'
   for url,ok in [(good,True),('https://example.com/old.tar.gz',False),(good.replace('o/r','x/r'),False),(good.replace('old-1.0.0','other'),False)]:
    with self.subTest(url=url):
     (Path(dst)/'packages.json').write_text(json.dumps({'packages':{'old':[{'version':'1.0.0','platform':'linux/amd64','sha256':'0'*64,'size':1,'url':url}]}}))
     r=subprocess.run([sys.executable,str(script),src,dst,'o/r'],capture_output=True,text=True)
     self.assertEqual(r.returncode==0,ok,r.stderr)
     if not ok:self.assertIn('points outside the package index',r.stderr)
 def test_package_index_serial_and_floor(self):
  script=ROOT/'scripts/package-index.py'
  def stage(root,version):
   d=Path(root)/version;d.mkdir(parents=True)
   data=version.encode()
   (d/'files.tar.gz').write_bytes(data)
   (d/'meta.json').write_text(json.dumps({'name':'pkg','version':version,'platform':'linux/amd64','sha256':hashlib.sha256(data).hexdigest(),'size':len(data)}))
  def publish(src,dst,floor=''):
   env=dict(os.environ,PKG_INDEX_MIN_SERIAL=floor)
   return subprocess.run([sys.executable,str(script),src,dst,'o/r'],capture_output=True,text=True,env=env)
  def index(dst):return json.loads((Path(dst)/'packages.json').read_text())
  with tempfile.TemporaryDirectory() as src,tempfile.TemporaryDirectory() as dst:
   stage(src,'1.0.0')
   r=publish(src,dst);self.assertEqual(r.returncode,0,r.stderr)
   first=index(dst)
   self.assertEqual((first['purpose'],first['serial']),('jerboa-package-index',1))
   self.assertRegex(first['generated'],r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$')
   # Nothing new: the index, serial included, is unchanged.
   r=publish(src,dst,'1');self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(index(dst),first)
   stage(src,'2.0.0')
   r=publish(src,dst,'1');self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(index(dst)['serial'],2)
   # A base older than the last publication is refused, as is a legacy one
   # once a floor is set, and a document that is not a package index.
   (Path(dst)/'packages.json').write_text(json.dumps(first))
   r=publish(src,dst,'2');self.assertNotEqual(r.returncode,0);self.assertIn('older than the last publication',r.stderr)
   (Path(dst)/'packages.json').write_text(json.dumps({'packages':{}}))
   r=publish(src,dst,'2');self.assertNotEqual(r.returncode,0);self.assertIn('older than the last publication',r.stderr)
   for bad in [{'purpose':'stable','serial':3,'packages':{}},{'serial':3,'packages':{}},{'purpose':'jerboa-package-index','serial':'3','packages':{}}]:
    (Path(dst)/'packages.json').write_text(json.dumps(bad))
    r=publish(src,dst);self.assertNotEqual(r.returncode,0,bad)
 def test_package_index_floor_cannot_fall_behind(self):
  script=ROOT/'scripts/package-index.py'
  def stage(root,version):
   d=Path(root)/version;d.mkdir(parents=True)
   data=version.encode()
   (d/'files.tar.gz').write_bytes(data)
   (d/'meta.json').write_text(json.dumps({'name':'pkg','version':version,'platform':'linux/amd64','sha256':hashlib.sha256(data).hexdigest(),'size':len(data)}))
  def publish(src,dst,floor):
   env={k:v for k,v in os.environ.items() if k!='PKG_INDEX_MIN_SERIAL'}
   if floor is not None:env['PKG_INDEX_MIN_SERIAL']=floor
   return subprocess.run([sys.executable,str(script),src,dst,'o/r'],capture_output=True,text=True,env=env)
  def serial(dst):return json.loads((Path(dst)/'packages.json').read_text())['serial']
  with tempfile.TemporaryDirectory() as src,tempfile.TemporaryDirectory() as dst:
   # The workflow passes 0 while the variable does not exist: the first
   # serial is published against it.
   stage(src,'1.0.0')
   r=publish(src,dst,'0');self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(serial(dst),1)
   # The variable was not raised: the next publication stops, changes
   # nothing and says exactly what to run.
   stage(src,'2.0.0');before=(Path(dst)/'packages.json').read_text()
   for floor in ['0',' 0 ']:
    r=publish(src,dst,floor);self.assertNotEqual(r.returncode,0)
    self.assertIn('was not raised after the last publication',r.stderr)
    self.assertIn('gh variable set PKG_INDEX_MIN_SERIAL --body 1\n',r.stderr)
    self.assertIn('Never lower the variable',r.stderr)
    self.assertEqual(r.stdout,'')
    self.assertEqual((Path(dst)/'packages.json').read_text(),before)
    self.assertFalse((Path(dst)/'pkg-2.0.0-linux-amd64.tar.gz').exists())
   # Raised: the publication goes through. Without the variable in the
   # environment (local use) nothing is compared.
   r=publish(src,dst,'1');self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(serial(dst),2)
   stage(src,'3.0.0')
   r=publish(src,dst,None);self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(serial(dst),3)
 def test_release_binaries_are_built_with_the_stable_floor(self):
  # Clients refuse stable manifests older than the floor compiled into
  # them. The candidate takes it from the live manifest (lock) and every
  # jerboa and jerboad it builds must be linked with it; assembly then
  # looks for it in each binary.
  flag='-X github.com/AitorConS/jerboa/internal/release.MinStableGenerated='
  spec=importlib.util.spec_from_file_location('candidate',Path(__file__).with_name('candidate.py'))
  candidate=importlib.util.module_from_spec(spec);spec.loader.exec_module(candidate)
  self.assertEqual(candidate.FLOOR_FLAG,flag)
  text=(ROOT/'.github/workflows/release.yml').read_text()
  self.assertIn('min_stable_generated: ${{ steps.lock.outputs.min_stable_generated }}',text.split('  validate:\n',1)[0])
  builds=0
  for path in [ROOT/'.github/workflows/release.yml',ROOT/'scripts/build-macos.sh',ROOT/'distro/build.sh',ROOT/'scripts/build-macos-reproducible.py']:
   source=path.read_text()
   # A build command, with its continuation lines.
   for command in re.findall(r'(?m)^.*\bgo[", ]+build\b(?:.*\\\n)*.*$',source):
    if not re.search(r'\./cmd/(jerboa|jerboad|\{name\})(\s|"|\]|$)',command):continue
    builds+=1
    with self.subTest(file=path.name,command=command.strip()[:60]):
     self.assertTrue(flag in command or '$floor' in command or '${floor}' in command or ' flags,' in command,command)
   if path.suffix!='.yml':
    self.assertIn(flag.split('=')[0]+'=',source)
    self.assertIn('JERBOA_MIN_STABLE_GENERATED',source)
  self.assertEqual(builds,6)
  def job(name,next_job):return text.split('  '+name+':\n',1)[1].split('  '+next_job+':\n',1)[0]
  self.assertIn('MIN_STABLE_GENERATED: ${{ needs.inputs.outputs.min_stable_generated }}',job('build-release','build-distro'))
  self.assertEqual(job('build-release','build-distro').count(flag+'${MIN_STABLE_GENERATED}'),2)
  self.assertIn('JERBOA_MIN_STABLE_GENERATED: ${{ needs.inputs.outputs.min_stable_generated }}',job('build-macos','determinism'))
  # The distro bakes the daemon build-release produced.
  self.assertIn('JERBOA_DAEMON_BINARY: dist/jerboad-linux-amd64',job('build-distro','build-macos'))
 def test_candidate_checks_the_installed_runner_guard(self):
  text=(ROOT/'.github/workflows/release.yml').read_text()
  boot=text.split('  kernel-test:',1)[1].split('  build-release:',1)[0]
  self.assertLess(boot.index('kvm-runner-guard.py check-installed'),boot.index('validate-kernel.py'))
 def test_promotion_runs_from_a_checkout_that_holds_the_evidence(self):
  # promote reads docs/releases/<version>/native-macos.json from its own
  # checkout of main, never from the candidate.
  text=(ROOT/'.github/workflows/promote.yml').read_text()
  self.assertLess(text.index('actions/checkout@'),text.index('candidate.py promote'))
  self.assertNotIn('ref:',text)
 def test_release_jobs_restore_no_build_cache(self):
  # A cache entry can be written by any earlier job on main. The jobs that
  # build what ships, and the ones that build and run the signer, compile
  # from source only.
  for name in ('release.yml','promote.yml'):
   text=(ROOT/'.github/workflows'/name).read_text()
   self.assertNotIn('cache: true',text,name)
   self.assertNotIn('actions/cache@',text,name)
   self.assertEqual(text.count('actions/setup-go@'),text.count('cache: false'),name)
 def test_promotion_gives_the_bucket_keys_to_one_step(self):
  text=(ROOT/'.github/workflows/promote.yml').read_text()
  self.assertEqual(text.count('secrets.AWS_SECRET_ACCESS_KEY'),1)
  self.assertEqual(text.count('secrets.AWS_ACCESS_KEY_ID'),1)
  step=text.split('- name: Verify and promote existing bytes',1)[1].split('- name:',1)[0]
  self.assertIn('secrets.AWS_SECRET_ACCESS_KEY',step)
  self.assertIn('secrets.JERBOA_MINISIGN_SEED',step)
 def test_distro_inputs_are_pinned(self):
  # The WSL rootfs is signed into releases: its base image and Firecracker
  # are pinned and checked, Firecracker to the same digest install.sh uses.
  text=(ROOT/'distro/Dockerfile').read_text()
  self.assertRegex(text,r'(?m)^FROM ubuntu:24\.04@sha256:[0-9a-f]{64}$')
  version=re.search(r'ARG FIRECRACKER_VERSION=(\S+)',text).group(1)
  arch=re.search(r'ARG FC_ARCH=(\S+)',text).group(1)
  digest=re.search(r'ARG FIRECRACKER_SHA256=([0-9a-f]{64})',text).group(1)
  self.assertIn(f'{version}-{arch}) fc_digest={digest} ;;',(ROOT/'scripts/install.sh').read_text())
  self.assertIn('"${FIRECRACKER_SHA256}  /tmp/fc.tgz" | sha256sum -c -',text)
  self.assertLess(text.index('sha256sum -c'),text.index('tar -xzf /tmp/fc.tgz'))
 def test_signing_secrets_only_reach_packaging(self):
  text=(ROOT/'.github/workflows/release.yml').read_text()
  for secret in ['secrets.CSC_LINK','secrets.CSC_KEY_PASSWORD']:
   self.assertEqual(text.count(secret),1)
  step=text[text.rindex('    - name:',0,text.index('secrets.CSC_LINK')):text.index('secrets.CSC_LINK')]
  self.assertIn('electron-builder --win',step)
  for command in ['npm ci','npm install','npm run','node ']:
   self.assertNotIn(command,step)
 def test_windows_smoke_gates_assembly_on_a_per_run_runner(self):
  text=(ROOT/'.github/workflows/release.yml').read_text()
  def job(name,next_job):return text.split('  '+name+':\n',1)[1].split('  '+next_job+':\n',1)[0]
  up=job('windows-runner-up','windows-smoke')
  smoke=job('windows-smoke','windows-runner-down')
  down=job('windows-runner-down','assemble')
  self.assertIn('    - windows-smoke\n',text.split('  assemble:\n',1)[1].split('    steps:',1)[0])
  for part in (up,down):
   self.assertIn('environment: windows-runner\n',part)
   self.assertIn('id-token: write',part)
  # The smoke runs only on this run's machine, with no secrets or cloud access.
  self.assertIn('- ${{ needs.windows-runner-up.outputs.label }}',smoke)
  self.assertNotIn('secrets.',smoke)
  self.assertNotIn('id-token',smoke)
  self.assertNotIn('environment:',smoke)
  self.assertIn('scripts/windows-install-smoke.sh',smoke)
  self.assertIn("if: always() && needs.windows-runner-up.outputs.instance_id != ''",down)
  script=(ROOT/'scripts/release/windows-runner.sh').read_text()
  self.assertIn('echo "::add-mask::$jit"',script)
  self.assertNotIn('set -x',script)
if __name__=='__main__':unittest.main()
