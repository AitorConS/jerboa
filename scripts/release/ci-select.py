#!/usr/bin/env python3
"""Classify changes; uncertainty always selects validation."""
import os,subprocess

# What only the end-to-end suite (tests/e2e) checks against a real daemon and
# real VMs: the CLI and daemon, and the packages that build, run, connect and
# store. A pull request that changes them runs the KVM jobs before the merge,
# not after it.
E2E_COVERED=('cmd/jerboa/','cmd/jerboad/','internal/apiserver/','internal/builder/','internal/compose/',
             'internal/dnsserver/','internal/network/','internal/vm/','internal/volume/','scripts/run-e2e.sh','scripts/run-kvm-tests.sh')

def classify(paths):
    enabled=any(not (p.startswith('docs/') or p.endswith('.md')) for p in paths)
    # VERSION.md is metadata, not permission to publish. Kernel/toolchain changes
    # require the expensive suite even on a PR; every main push validates fully.
    # The installer and what the upgrade test drives decide whether an
    # installed machine survives the next release.
    full=any(p.startswith(('kernel/','distro/','.github/','scripts/release/','tests/integration/','tests/e2e/','tests/upgrade/','scripts/install.sh')+E2E_COVERED) for p in paths)
    return enabled,full

if __name__=='__main__':
    event=os.environ.get('EVENT','');base=os.environ.get('BASE','');head=os.environ.get('HEAD','HEAD')
    try:
        if not base or set(base)=={'0'}:raise ValueError('Unknown base')
        paths=subprocess.check_output(['git','diff','--name-only',base,head],text=True).splitlines()
        enabled,full=classify(paths)
        if not paths:enabled,full=True,True
    except (ValueError,subprocess.CalledProcessError):enabled,full=True,True
    if event in ('push','workflow_dispatch'):full=True
    with open(os.environ['GITHUB_OUTPUT'],'a') as out:
        out.write(f'enabled={str(enabled).lower()}\nfull={str(full).lower()}\n')
