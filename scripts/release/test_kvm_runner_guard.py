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

    def test_default_workflows_cover_every_kvm_workflow(self):
        # Callers of a reusable workflow are what GITHUB_WORKFLOW_REF names.
        workflows=HERE.parents[1]/'.github/workflows'
        kvm={p.name for p in workflows.glob('*.yml') if 'self-hosted' in p.read_text() and re.search(r'(?m)^\s*- kvm\s*$|\[self-hosted, linux, kvm\]',p.read_text())}
        callers={p.name for p in workflows.glob('*.yml') if any(f'uses: ./.github/workflows/{k}' in p.read_text() for k in kvm)}
        top={k for k in kvm if 'workflow_call' not in (workflows/k).read_text().split('jobs:',1)[0]}
        self.assertEqual(top|callers,set(g.DEFAULT_WORKFLOWS.split()))

if __name__=='__main__':unittest.main()
