#!/usr/bin/env python3
"""Select package recipes independently from product releases.

Each recipe builds for every architecture in its "arches" list (default: amd64
and arm64). A recipe that downloads a prebuilt binary gives the other
architecture's source in a key named after it, e.g. "arm64": {"source_url",
"sha256"}; the top-level source is used otherwise.
"""
import json,os,subprocess
from pathlib import Path
ARCHES=('amd64','arm64')
RUNNERS={'amd64':'ubuntu-24.04','arm64':'ubuntu-24.04-arm'}
def expand(recipe):
    for arch in recipe.get('arches',ARCHES):
        if arch not in ARCHES:raise SystemExit(f"Unknown arch {arch} for {recipe['package']}")
        item={k:v for k,v in recipe.items() if k not in ARCHES and k!='arches'}
        item.update(recipe.get(arch,{}))
        item['arch']=arch
        item['runner']=RUNNERS[arch]
        yield item
def main():
    recipes=json.loads(Path('scripts/release/packages.json').read_text())
    name=os.environ.get('PACKAGE','')
    if name:
        if name!='all' and name not in {r['package'] for r in recipes}:raise SystemExit('Unknown package: '+name)
        selected=[r for r in recipes if name=='all' or r['package']==name]
    else:
        before=os.environ.get('BEFORE','')
        if not before or set(before)=={'0'}:changed={'scripts/release/packages.json'}
        else:changed=set(subprocess.check_output(['git','diff','--name-only',before,'HEAD'],text=True).splitlines())
        all_changed=bool(changed & {'scripts/release/packages.json','.github/workflows/packages.yml','scripts/release/archive-package.py'})
        selected=[r for r in recipes if all_changed or r['build_script'] in changed]
    matrix=[item for r in selected for item in expand(r)]
    with open(os.environ['GITHUB_OUTPUT'],'a') as f:
        f.write('matrix='+json.dumps({'include':matrix})+'\n')
        f.write('any='+str(bool(matrix)).lower()+'\n')
if __name__=='__main__':main()
