#!/usr/bin/env python3
"""Job-started hook for the self-hosted KVM runner: refuse jobs it must not run.

The `if:` guards and `environment: kvm` on the KVM jobs live in workflow files
that a pull request supplies itself, so a fork pull request can drop them or
add a workflow of its own naming the runner's labels. This hook runs on the
runner before any step of a job, from a copy the runner owner installed, so a
pull request cannot change it. A non-zero exit fails the job before its steps
run.

Jobs not started by a pull request (push, workflow_dispatch, schedule) wait
for no review: nightly.yml's KVM jobs have no `environment: kvm`, so that the
scheduled run does not wait for a person, and anyone who can dispatch a
workflow picks its ref. Those jobs therefore run only for refs/heads/main,
whose changes were reviewed; a branch is tested on this runner through its
pull request, which waits for the `kvm` environment's review.

Install (on the runner machine, as root):

  install -o root -g root -m 0755 scripts/release/kvm-runner-guard.py /usr/local/libexec/jerboa-kvm-runner-guard
  install -o root -g root -m 0755 scripts/release/kvm-runner-guard.sh /usr/local/libexec/jerboa-kvm-runner-guard.sh
  # In the runner's .env file (next to its config), then restart the runner service:
  ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/libexec/jerboa-kvm-runner-guard.sh
  JERBOA_KVM_REPOSITORY=AitorConS/jerboa

The runner refuses a hook whose name does not end in .sh, .ps1 or .js, so the
hook it is given is kvm-runner-guard.sh, which runs this file from the same
directory.

Install from a reviewed revision of main and re-install after changing either
file: the runner never reads them from a checkout. `kvm-runner-guard.py
check-installed`, which the release candidate workflow runs on the runner,
fails when an installed copy differs from the checkout's.

Optional settings in the same .env file:
  JERBOA_KVM_WORKFLOWS  workflow files allowed to run here (default below)
  JERBOA_KVM_EVENTS     triggering events allowed (default below)
"""
import json
import os
import sys

DEFAULT_WORKFLOWS = 'main.yml release.yml nightly.yml'
DEFAULT_EVENTS = 'push pull_request workflow_dispatch schedule'
MAIN_REF = 'refs/heads/main'
INSTALL = ('On the runner, from a reviewed checkout of main, install both files as the header of '
           'scripts/release/kvm-runner-guard.py says, restart the runner service and re-run this job')


def refusal(env):
    """Return why this job must not run on the KVM runner, or None."""
    expected = env.get('JERBOA_KVM_REPOSITORY', '')
    repo = env.get('GITHUB_REPOSITORY', '')
    if not expected:
        return 'JERBOA_KVM_REPOSITORY is not configured on this runner'
    if repo != expected:
        return f'repository {repo!r} is not {expected!r}'
    event_name = env.get('GITHUB_EVENT_NAME', '')
    if event_name not in env.get('JERBOA_KVM_EVENTS', DEFAULT_EVENTS).split():
        return f'event {event_name!r} may not use this runner'
    # owner/repo/.github/workflows/<file>@<ref>: the top-level workflow, also
    # for jobs of a reusable workflow it calls.
    workflow_ref = env.get('GITHUB_WORKFLOW_REF', '')
    path = workflow_ref.split('@', 1)[0]
    prefix = f'{expected}/.github/workflows/'
    allowed = env.get('JERBOA_KVM_WORKFLOWS', DEFAULT_WORKFLOWS).split()
    if not path.startswith(prefix) or path[len(prefix):] not in allowed:
        return f'workflow {workflow_ref!r} may not use this runner'
    try:
        with open(env['GITHUB_EVENT_PATH']) as f:
            event = json.load(f)
    except (KeyError, OSError, ValueError) as e:
        return f'cannot read the event payload: {e}'
    pr = event.get('pull_request')
    if event_name.startswith('pull_request') or pr is not None:
        head = ((pr or {}).get('head') or {}).get('repo') or {}
        # A fork's pull request runs the fork's workflow files.
        if head.get('full_name') != expected:
            return f'pull request from {head.get("full_name")!r}, not from {expected!r}'
        return None
    # Anything else runs unreviewed: only main's code and workflow files.
    ref = env.get('GITHUB_REF', '')
    workflow_at = workflow_ref.split('@', 1)[1] if '@' in workflow_ref else ''
    if ref != MAIN_REF or workflow_at != MAIN_REF:
        return (f'{event_name} jobs run here only on {MAIN_REF}, not on ref {ref!r} '
                f'(workflow {workflow_ref!r}); test a branch through its pull request')
    return None


def installed(env, current=__file__):
    """Compare the hook installed on this runner with this file.

    Returns ('ok' | 'unknown' | 'stale', message). It runs as a job step, from
    a checkout, so it is a reminder for the runner's owner and no protection:
    what protects the runner is the installed hook itself."""
    hook = env.get('ACTIONS_RUNNER_HOOK_JOB_STARTED', '')
    if not hook:
        return 'unknown', ('ACTIONS_RUNNER_HOOK_JOB_STARTED is not visible to this job, so the installed KVM runner guard '
                           'cannot be compared with this revision; check on the runner that it is installed (see the header '
                           'of scripts/release/kvm-runner-guard.py)')
    if not hook.endswith('.sh'):
        return 'stale', (f'the runner hook {hook} does not end in .sh, so the runner refuses it. ' + INSTALL)
    wrapper = os.path.join(os.path.dirname(os.path.abspath(current)), 'kvm-runner-guard.sh')
    for path, source in ((hook, wrapper), (hook[:-len('.sh')], current)):
        try:
            with open(path, 'rb') as a, open(source, 'rb') as b:
                same = a.read() == b.read()
        except OSError as e:
            return 'stale', f'cannot read the installed KVM runner guard {path}: {e}. ' + INSTALL
        if not same:
            return 'stale', (f'the KVM runner guard installed at {path} is not scripts/release/{os.path.basename(source)} '
                             'of this revision. ' + INSTALL)
    return 'ok', f'KVM runner guard: {hook} is this revision\'s'


def main():
    if sys.argv[1:] == ['check-installed']:
        state, message = installed(os.environ)
        if state == 'stale':
            print(f'::error::{message}', file=sys.stderr)
            sys.exit(1)
        print(f'::warning::{message}' if state == 'unknown' else message)
        return
    reason = refusal(os.environ)
    if reason:
        print(f'::error::KVM runner guard refused this job: {reason}', file=sys.stderr)
        sys.exit(1)
    print('KVM runner guard: job allowed')


if __name__ == '__main__':
    main()
