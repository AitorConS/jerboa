#!/usr/bin/env python3
"""Job-started hook for the self-hosted KVM runner: refuse jobs it must not run.

The `if:` guards and `environment: kvm` on the KVM jobs live in workflow files
that a pull request supplies itself, so a fork pull request can drop them or
add a workflow of its own naming the runner's labels. This hook runs on the
runner before any step of a job, from a copy the runner owner installed, so a
pull request cannot change it. A non-zero exit fails the job before its steps
run.

Install (on the runner machine, as root):

  install -o root -g root -m 0755 scripts/release/kvm-runner-guard.py /usr/local/libexec/jerboa-kvm-runner-guard
  # In the runner's .env file (next to its config), then restart the runner service:
  ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/libexec/jerboa-kvm-runner-guard
  JERBOA_KVM_REPOSITORY=AitorConS/jerboa

Install from a reviewed revision of main and re-install after changing this
file: the runner never reads it from a checkout.

Optional settings in the same .env file:
  JERBOA_KVM_WORKFLOWS  workflow files allowed to run here (default below)
  JERBOA_KVM_EVENTS     triggering events allowed (default below)
"""
import json
import os
import sys

DEFAULT_WORKFLOWS = 'main.yml release.yml nightly.yml'
DEFAULT_EVENTS = 'push pull_request workflow_dispatch schedule'


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


def main():
    reason = refusal(os.environ)
    if reason:
        print(f'::error::KVM runner guard refused this job: {reason}', file=sys.stderr)
        sys.exit(1)
    print('KVM runner guard: job allowed')


if __name__ == '__main__':
    main()
