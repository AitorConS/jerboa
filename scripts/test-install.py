"""Exercise the installer offline with system commands mocked and paths isolated."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile

root_dir = Path(__file__).resolve().parents[1]
source = root_dir / ('public/install.sh' if (root_dir / 'public/install.sh').exists() else 'scripts/install.sh')
MOCK = '''#!/usr/bin/env python3
import hashlib, json, os, pathlib, shutil, sys, subprocess
name=pathlib.Path(sys.argv[0]).name; args=sys.argv[1:]; root=pathlib.Path(os.environ['FIXTURE'])
with (root/'calls').open('a') as log: log.write(name+' '+ ' '.join(args)+'\\n')
if name=='uname': print(os.environ.get('OS','Linux') if args==['-s'] else os.environ.get('ARCH','x86_64'))
elif name=='id':
    if args==['-u']: print(os.environ.get('UID_MOCK','0'))
    elif args and args[0]=='-gn': print('users')
elif name=='getent':
    if args[0]=='passwd': print('tester:x:1000:1000::'+str(root/'home')+':/bin/bash')
    else: sys.exit(2)
elif name=='curl':
    url=args[-3]; out=pathlib.Path(args[-1])
    if url.endswith(('stable.json','manifest.json')): out.write_bytes((root/'manifest').read_bytes())
    elif url.endswith('.minisig'): out.write_text('mock signature')
    elif os.environ.get('FAIL_DOWNLOAD'): sys.exit(22)
    else: out.write_bytes(b'corrupt' if os.environ.get('CORRUPT') else b'binary')
elif name=='tar':
    out=pathlib.Path(args[-1])/'release-v1.10.1-x86_64'; out.mkdir()
    (out/'firecracker-v1.10.1-x86_64').write_bytes(b'firecracker')
elif name=='minisign': sys.exit(1 if os.environ.get('BAD_SIGNATURE') else 0)
elif name=='sw_vers': print(os.environ.get('MACOS_VERSION','26.0'))
elif name in ['pkgutil','spctl']:
    sys.exit(1 if os.environ.get('BAD_PACKAGE_SIGNATURE') else 0)
elif name=='sudo':
    if args[0]=='install': sys.exit(subprocess.call(args))
    if args[0]=='installer':
        native=root/'usr/local/libexec/jerboa/bin'; native.mkdir(parents=True,exist_ok=True)
        for binary in ['jerboa','jerboad']:
            p=native/binary; p.write_text('#!/bin/sh\\nexit 0\\n'); p.chmod(0o755)
elif name in ['sha256sum','shasum']:
    digest, file=sys.stdin.read().strip().split('  ',1)
    sys.exit(0 if hashlib.sha256(pathlib.Path(file).read_bytes()).hexdigest()==digest else 1)
elif name=='install':
    mode=0o755; positional=[]; i=0
    while i<len(args):
        if args[i] in ['-o','-g','-m']:
            if args[i]=='-m': mode=int(args[i+1],8)
            i+=2
        elif args[i]=='-d': i+=1
        else: positional.append(args[i]); i+=1
    if '-d' in args:
        for p in positional: pathlib.Path(p).mkdir(parents=True,exist_ok=True)
    else: shutil.copyfile(*positional)
    pathlib.Path(positional[-1]).chmod(mode)
'''

with tempfile.TemporaryDirectory(prefix='jerboa-install-test-') as tmp:
    root=Path(tmp); bin=root/'mocks'; bin.mkdir()
    for name in ['uname','id','getent','curl','minisign','sha256sum','install','apt-get','systemctl','modprobe','useradd','usermod','groupadd','chown','tar','sw_vers','shasum','pkgutil','spctl','sudo','brew']:
        p=bin/name; p.write_text(MOCK); p.chmod(0o755)
    script=source.read_text()
    for path in ['/usr/local/bin','/usr/local/libexec/jerboa','/var/lib/jerboa','/etc/jerboa','/etc/systemd/system','/etc/modules-load.d','/run/systemd/system','/dev/kvm']:
        script=script.replace(path,str(root)+path)
    (root/'etc/systemd/system').mkdir(parents=True)
    (root/'run/systemd/system').mkdir(parents=True)
    runner=root/'install.sh'; runner.write_text(script)
    digest=hashlib.sha256(b'binary').hexdigest()
    manifest={'components':{key:{'version':'v0.51.2','platforms':{'linux-amd64':{'sha256':digest}}} for key in ['cli','daemon']}}
    (root/'manifest').write_text(json.dumps(manifest))
    env={**os.environ,'PATH':str(bin)+os.pathsep+os.environ['PATH'],'FIXTURE':str(root),'HYPERVISOR':'qemu','SUDO_USER':'tester'}
    for key in ['RELEASE_BASE','JERBOA_PORT','FC_ARCH','FIRECRACKER_VERSION']: env.pop(key,None)
    def run(*args, ok=True, **overrides):
        result=subprocess.run(['bash',str(runner),*args],env={**env,**overrides},capture_output=True,text=True)
        assert (result.returncode==0)==ok,result.stdout+result.stderr
        return result
    run('--help')
    for args in [('--version',),('--version','bad/path'),('--wat',),('--version=',)]: run(*args,ok=False)
    run(ok=False,ARCH='aarch64')
    run(ok=False,JERBOA_PORT='99999')
    run(ok=False,HYPERVISOR='invalid')
    run(ok=False,HYPERVISOR='firecracker')
    run()
    token=(root/'etc/jerboa/daemon.env').read_text()
    config=(root/'home/.jerboa/config.toml').read_text()
    assert token.strip().split('=',1)[1] in config
    assert (root/'home/.jerboa/config.toml').stat().st_mode & 0o777 == 0o600
    for args in [('--version','0.51.2'),('--version=v0.51.2',),('--version','latest')]: run(*args)
    assert (root/'etc/jerboa/daemon.env').read_text()==token
    assert (root/'home/.jerboa/config.toml').read_text()==config
    before=(root/'usr/local/bin/jerboa').read_bytes()
    run(ok=False,CORRUPT='1')
    run(ok=False,BAD_SIGNATURE='1')
    run(ok=False,FAIL_DOWNLOAD='1')
    assert (root/'usr/local/bin/jerboa').read_bytes()==before
    result=run('--version','0.51.1')
    assert 'without a signed checksum' in result.stdout
    calls=(root/'calls').read_text()
    assert '/cli/v0.51.2/jerboa-linux-amd64' in calls and '/daemon/v0.51.2/jerboad-linux-amd64' in calls
    assert '/cli/v0.51.1/jerboa-linux-amd64' in calls
    assert 'systemctl restart jerboad.service' in calls
    assert 'firecracker-microvm' not in calls
    (root/'dev').mkdir()
    (root/'dev/kvm').touch()
    run(HYPERVISOR='firecracker')
    assert (root/'usr/local/bin/firecracker').read_bytes()==b'firecracker'
    print('PASS: version parsing, latest/pinned URLs, platform checks, QEMU without KVM, upgrades, token preservation, signature/checksum/download failures.')

    manifest['components']['macos']={'version':'v0.51.2','platforms':{'darwin-arm64':{
        'sha256':digest,'url':'https://releases.jerboa.dev/macos/v0.51.2/jerboa-0.51.2-macos-arm64.pkg'}}}
    (root/'manifest').write_text(json.dumps(manifest))
    env.update(OS='Darwin', ARCH='arm64', UID_MOCK='501', HYPERVISOR='firecracker')
    (root/'calls').write_text('')
    run()
    run('--version','0.51.2')
    calls=(root/'calls').read_text()
    assert '/macos/v0.51.2/jerboa-0.51.2-macos-arm64.pkg' in calls
    assert '/releases/v0.51.2/manifest.json.minisig' in calls
    assert 'sudo installer -pkg' in calls
    assert 'apt-get' not in calls and 'systemctl' not in calls
    assert '/libexec/jerboa/bin/jerboa' in (root/'usr/local/bin/jerboa').read_text()
    for overrides in [dict(ARCH='x86_64'),dict(MACOS_VERSION='15.0'),dict(UID_MOCK='0'),
                      dict(HYPERVISOR='qemu'),dict(CORRUPT='1'),dict(BAD_SIGNATURE='1'),
                      dict(FAIL_DOWNLOAD='1'),dict(BAD_PACKAGE_SIGNATURE='1')]:
        (root/'calls').write_text('')
        run(ok=False,**overrides)
        assert 'sudo installer' not in (root/'calls').read_text()
    run('--version','0.51.1',ok=False)
    manifest['components'].pop('macos')
    (root/'manifest').write_text(json.dumps(manifest))
    run(ok=False)
    print('PASS: macOS platform/version detection, signed latest/pinned package, native wrappers, fail-closed verification before installation.')
