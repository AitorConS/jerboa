#!/usr/bin/env python3
"""Package the recipe's executable, rather than passing a directory to pkg create."""
import os,subprocess
from pathlib import Path
name=os.environ['PACKAGE_NAME'];version=os.environ['PACKAGE_VERSION']
binaries={'python':'python3','redis':'redis-server','sqlite':'sqlite3'}
root=Path('dist/pkg')/name/version
binary=root/binaries.get(name,name)
if not binary.is_file():raise SystemExit('Missing built executable: '+str(binary))
args=['./dist/jerboa','pkg','create',name+':'+version,str(binary),'--description',f'{name} {version} runtime','--runtime',name]
# Recipes can stage complete interpreter resources at their final guest paths.
resources=root/'rootfs'
if resources.exists():
    args+=['--program-path','/usr/local/bin/'+binary.name]
    for p in sorted(resources.rglob('*')):
        if p.is_file():args+=['--map',str(p)+'=/'+str(p.relative_to(resources))]
subprocess.run(args,check=True)
