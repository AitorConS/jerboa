#!/usr/bin/env python3
"""Select package recipes independently from product releases."""
import json,os,subprocess
from pathlib import Path
recipes=json.loads(Path('scripts/release/packages.json').read_text())
name=os.environ.get('PACKAGE','')
if name:
    if name!='all' and name not in {r['package'] for r in recipes}:raise SystemExit('Unknown package: '+name)
    selected=[r for r in recipes if name=='all' or r['package']==name]
else:
    before=os.environ.get('BEFORE','')
    if not before or set(before)=={'0'}:changed={'scripts/release/packages.json'}
    else:changed=set(subprocess.check_output(['git','diff','--name-only',before,'HEAD'],text=True).splitlines())
    all_changed=bool(changed & {'scripts/release/packages.json','.github/workflows/packages.yml'})
    selected=[r for r in recipes if all_changed or r['build_script'] in changed]
with open(os.environ['GITHUB_OUTPUT'],'a') as f:
    f.write('matrix='+json.dumps({'include':selected})+'\n')
    f.write('any='+str(bool(selected)).lower()+'\n')
