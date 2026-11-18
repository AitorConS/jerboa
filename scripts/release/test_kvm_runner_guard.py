"""The KVM runner's job-started hook refuses fork and unlisted jobs."""
import importlib.util,json,re,tempfile,unittest
from pathlib import Path
HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location('guard',HERE/'kvm-runner-guard.py')
g=importlib.util.module_from_spec(spec);spec.loader.exec_module(g)
REPO='AitorConS/jerboa'

class GuardTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)

    def env(self,event_name='pull_request',event=None,workflow='main.yml',**extra):
        path=Path(self.tmp.name)/'event.json'
        path.write_text(json.dumps(event if event is not None else {}))
        return {'JERBOA_KVM_REPOSITORY':REPO,'GITHUB_REPOSITORY':REPO,'GITHUB_EVENT_NAME':event_name,
                'GITHUB_WORKFLOW_REF':f'{REPO}/.github/workflows/{workflow}@refs/heads/x','GITHUB_EVENT_PATH':str(path),**extra}

    def pr(self,head):
        return {'pull_request':{'head':{'repo':{'full_name':head} if head else None},'base':{'repo':{'full_name':REPO}}}}

    def test_same_repository_jobs_run(self):
        self.assertIsNone(g.refusal(self.env(event=self.pr(REPO))))
        for event_name,workflow in [('push','main.yml'),('workflow_dispatch','release.yml'),('schedule','nightly.yml')]:
            with self.subTest(event=event_name):
                self.assertIsNone(g.refusal(self.env(event_name,{},workflow)))

    def test_fork_pull_requests_are_refused(self):
        for head in ['someone/jerboa',None]:
            with self.subTest(head=head):
                self.assertRegex(g.refusal(self.env(event=self.pr(head))),'pull request from')
        # Review events of a fork's pull request run its workflow files too.
        self.assertRegex(g.refusal(self.env('pull_request_review',self.pr('someone/jerboa'),JERBOA_KVM_EVENTS='pull_request_review')),'pull request from')

    def test_unlisted_workflows_events_and_repositories_are_refused(self):
        self.assertRegex(g.refusal(self.env(event=self.pr(REPO),workflow='added-by-pr.yml')),'workflow')
        env=self.env(event=self.pr(REPO));env['GITHUB_WORKFLOW_REF']='someone/jerboa/.github/workflows/main.yml@refs/heads/x'
        self.assertRegex(g.refusal(env),'workflow')
        for event_name in ['pull_request_target','issue_comment','workflow_run']:
            with self.subTest(event=event_name):
                self.assertRegex(g.refusal(self.env(event_name,{})),'event')
        self.assertRegex(g.refusal(self.env(GITHUB_REPOSITORY='someone/jerboa')),'repository')
        self.assertRegex(g.refusal(self.env(JERBOA_KVM_REPOSITORY='')),'not configured')

    def test_unreadable_payload_fails_closed(self):
        env=self.env(event=self.pr(REPO));Path(env['GITHUB_EVENT_PATH']).write_text('{')
        self.assertRegex(g.refusal(env),'payload')
        del env['GITHUB_EVENT_PATH']
        self.assertRegex(g.refusal(env),'payload')

    def install(self):
        """Both files as the runner holds them: the guard and its .sh launcher."""
        guard=Path(self.tmp.name)/'hook';guard.write_bytes((HERE/'kvm-runner-guard.py').read_bytes())
        launcher=Path(self.tmp.name)/'hook.sh';launcher.write_bytes((HERE/'kvm-runner-guard.sh').read_bytes())
        return guard,launcher

    def test_installed_copy_is_compared_with_the_checkout(self):
        current=HERE/'kvm-runner-guard.py'
        guard,launcher=self.install()
        env={'ACTIONS_RUNNER_HOOK_JOB_STARTED':str(launcher)}
        self.assertEqual(g.installed(env,current)[0],'ok')
        guard.write_bytes(current.read_bytes()+b'# older\n')
        state,message=g.installed(env,current)
        self.assertEqual(state,'stale');self.assertIn(str(guard),message);self.assertIn('kvm-runner-guard.py',message)
        guard,launcher=self.install();launcher.write_bytes(b'#!/bin/sh\n')
        state,message=g.installed(env,current)
        self.assertEqual(state,'stale');self.assertIn('kvm-runner-guard.sh',message)
        guard,launcher=self.install();guard.unlink()
        self.assertEqual(g.installed(env,current)[0],'stale')
        # The runner refuses a hook without one of its extensions.
        guard,launcher=self.install()
        state,message=g.installed({'ACTIONS_RUNNER_HOOK_JOB_STARTED':str(guard)},current)
        self.assertEqual(state,'stale');self.assertIn('.sh',message)
        # A runner that does not show the variable to steps cannot be checked.
        state,message=g.installed({},current)
        self.assertEqual(state,'unknown');self.assertIn('ACTIONS_RUNNER_HOOK_JOB_STARTED',message)

    def test_check_installed_fails_only_on_a_stale_copy(self):
        import os,subprocess,sys
        current=HERE/'kvm-runner-guard.py'
        def run(**extra):
            env={k:v for k,v in os.environ.items() if k!='ACTIONS_RUNNER_HOOK_JOB_STARTED'}
            return subprocess.run([sys.executable,str(current),'check-installed'],capture_output=True,text=True,env={**env,**extra})
        r=run();self.assertEqual(r.returncode,0);self.assertIn('::warning::',r.stdout)
        guard,launcher=self.install()
        r=run(ACTIONS_RUNNER_HOOK_JOB_STARTED=str(launcher));self.assertEqual(r.returncode,0);self.assertNotIn('::warning::',r.stdout)
        guard.write_bytes(b'#!/bin/sh\n')
        r=run(ACTIONS_RUNNER_HOOK_JOB_STARTED=str(launcher));self.assertEqual(r.returncode,1);self.assertIn('::error::',r.stderr)

    def test_launcher_runs_the_guard_next_to_it_the_way_the_runner_does(self):
        import os,subprocess
        guard,launcher=self.install()
        # The runner starts a .sh hook with bash and the path as its argument.
        env={k:v for k,v in os.environ.items() if not k.startswith(('GITHUB_','JERBOA_KVM_'))}
        r=subprocess.run(['bash','-e',str(launcher)],capture_output=True,text=True,env=env)
        self.assertEqual(r.returncode,1);self.assertIn('JERBOA_KVM_REPOSITORY is not configured',r.stderr)
        r=subprocess.run(['bash','-e',str(launcher)],capture_output=True,text=True,env={**env,**self.env(event_name='push')})
        self.assertEqual(r.returncode,0,r.stderr);self.assertIn('job allowed',r.stdout)

    def test_default_workflows_cover_every_kvm_workflow(self):
        # Callers of a reusable workflow are what GITHUB_WORKFLOW_REF names.
        workflows=HERE.parents[1]/'.github/workflows'
        kvm={p.name for p in workflows.glob('*.yml') if 'self-hosted' in p.read_text() and re.search(r'(?m)^\s*- kvm\s*$|\[self-hosted, linux, kvm\]',p.read_text())}
        callers={p.name for p in workflows.glob('*.yml') if any(f'uses: ./.github/workflows/{k}' in p.read_text() for k in kvm)}
        top={k for k in kvm if 'workflow_call' not in (workflows/k).read_text().split('jobs:',1)[0]}
        self.assertEqual(top|callers,set(g.DEFAULT_WORKFLOWS.split()))

if __name__=='__main__':unittest.main()
