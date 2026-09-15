#!/usr/bin/env python3
"""Real package -> API -> Nanos acceptance. Uses an isolated, retained HOME/store.
Requires JERBOA_TEST_BIN, JERBOA_FIRECRACKER_BIN and JERBOA_PKG_DOCKER_ID.
Run on macOS ARM64 with Go on PATH. No installed binaries are changed.
"""
import atexit, hashlib, json, os, pathlib, shutil, socket, subprocess, tempfile, time, urllib.request
ROOT=pathlib.Path(__file__).resolve().parents[1]
BIN=pathlib.Path(os.environ['JERBOA_TEST_BIN']).resolve()
FC=pathlib.Path(os.environ['JERBOA_FIRECRACKER_BIN']).resolve()
WORK=pathlib.Path(tempfile.mkdtemp(prefix='jpa-',dir='/tmp'))
DEST=pathlib.Path(os.environ.get('JERBOA_TEST_OUTPUT',str(BIN)))/('acceptance-'+time.strftime('%Y%m%d-%H%M%S'))
atexit.register(lambda:shutil.copytree(WORK,DEST,dirs_exist_ok=True))
print('Evidence:',WORK,flush=True)
WORK.mkdir(exist_ok=True)
(WORK/'home').mkdir(exist_ok=True)
env=dict(os.environ,HOME=str(WORK/'home'),JERBOA_HOST='unix://'+str(WORK/'daemon.sock'),JERBOA_AUTH_TOKEN='pkg-arm64-acceptance')
# Carry only the Docker endpoint into the isolated HOME, never credentials.
if not env.get('DOCKER_HOST'):
    env['DOCKER_HOST']=subprocess.check_output(['docker','context','inspect','--format','{{.Endpoints.docker.Host}}'],text=True).strip()
env.update(json.loads(subprocess.check_output(['go','env','-json','GOCACHE','GOMODCACHE','GOPATH'],text=True)))
transcript=(WORK/'commands.log').open('w')
def run(args,success=True,**kw):
    transcript.write('$ '+' '.join(map(str,args))+'\n');transcript.flush()
    r=subprocess.run(list(map(str,args)),env=kw.pop('env',env),text=True,capture_output=True,timeout=240,**kw)
    transcript.write(r.stdout+r.stderr);transcript.flush()
    if success and r.returncode: raise RuntimeError(r.stdout+r.stderr)
    if not success and not r.returncode: raise AssertionError('unexpected success')
    return r.stdout.strip() if success else r.stderr

def cli(*args,**kw):return run([BIN/'jerboa',*args],**kw)
def rpc(method,params):
    with socket.socket(socket.AF_UNIX) as sock:
        sock.connect(env['JERBOA_HOST'][7:]);f=sock.makefile('rwb')
        for i,(m,p) in enumerate([('Auth.Hello',{'token':env['JERBOA_AUTH_TOKEN'],'proto':2}),(method,params)]):
            f.write((json.dumps({'jsonrpc':'2.0','id':i+1,'method':m,'params':p})+'\n').encode());f.flush()
            result=json.loads(f.readline())
            if result.get('error'):raise RuntimeError(str(result['error']))
        return result['result']

def wait(check,label):
    deadline=time.monotonic()+60
    while time.monotonic()<deadline:
        try:
            value=check()
            if value:return value
        except (OSError,ValueError,RuntimeError):pass
        time.sleep(.2)
    raise RuntimeError('timeout: '+label)

static=WORK/'static-arm64'
run(['go','build','-o',static,ROOT/'tests/fixtures/pkg-arm64/static.go'],env=dict(env,GOOS='linux',GOARCH='arm64',CGO_ENABLED='0'))
cli('pkg','create','static-service:1.2.3',static,'--platform','linux/arm64','--program-path','/opt/static/service')
image_id=os.environ['JERBOA_PKG_DOCKER_ID']
cli('pkg','from-docker','dynamic-docker:2.3.4',image_id,'--platform','linux/arm64','--file','/opt/service/bin/service')
cli('pkg','from-docker','wrong-docker:1',image_id,'--platform','linux/amd64','--file','/opt/service/bin/service',success=False)
# Copy the exact runtime files from the same ID, without executing a container.
cid=run(['docker','create','--platform','linux/arm64',image_id])
sysroot=WORK/'sysroot';sysroot.mkdir(exist_ok=True)
try:
    for guest in ['opt/service/bin/service','opt/service/lib/libmessage.so','lib/ld-linux-aarch64.so.1','lib/aarch64-linux-gnu/libc.so.6']:
        dest=sysroot/guest;dest.parent.mkdir(parents=True,exist_ok=True)
        run(['docker','cp','-L',cid+':/'+guest,dest])
finally:run(['docker','rm',cid])
# From here on, Docker is deliberately inaccessible.
env['DOCKER_HOST']='unix:///tmp/jerboa-pkg-no-docker.sock'
cli('pkg','create','dynamic-local:2.3.4',sysroot/'opt/service/bin/service','--sysroot',sysroot,'--program-path','/opt/service/bin/service','--platform','linux/arm64')
packages=json.loads(cli('pkg','list','--source','jerboa','--output-json'))
assert len(packages)==3
for p in packages:
    assert p['platform']=='linux/arm64' and len(p['sha256'])==64
    if p['name']=='dynamic-docker':
        assert p['provenance']['kind']=='docker' and p['provenance']['reference']==image_id
        assert p['provenance']['image_id'].startswith('sha256:')
    else:assert p['provenance']['kind']=='local'
(WORK/'packages.json').write_text(json.dumps(packages,indent=2))
log=(WORK/'daemon.log').open('w')
daemon=subprocess.Popen([str(BIN/'jerboad'),'--host',env['JERBOA_HOST'],'--tools-dir',str(BIN/'tools'),'--hypervisor','firecracker','--fc-bin',str(FC)],env=env,stdout=log,stderr=log)
active=[]
try:
    wait(lambda:cli('status'),'daemon')
    # Exercise the full root command with each endpoint source and config auth.
    config_path=WORK/'home/.jerboa/config.toml'
    config_path.write_text('[daemon]\nendpoint="'+env['JERBOA_HOST']+'"\ntoken="'+env['JERBOA_AUTH_TOKEN']+'"\n')
    config_path.chmod(0o600)
    for mode in ['flag','environment','config']:
        load_env=dict(env)
        load_env.pop('JERBOA_AUTH_TOKEN',None)
        prefix=[]
        if mode=='flag':
            prefix=['--host',env['JERBOA_HOST']]
            load_env['JERBOA_HOST']='unix:///tmp/jerboa-review-unused.sock'
        elif mode=='config':load_env.pop('JERBOA_HOST',None)
        output=cli(*prefix,'pkg','load','static-service:1.2.3','--source','jerboa','--platform','linux/arm64','-d',env=load_env)
        vm=output.splitlines()[-1];active.append(vm)
        assert any(v['id']==vm for v in rpc('VM.List',{}))
        cli('stop',vm);cli('rm',vm);active.remove(vm)
        print('PASS: pkg load through root using '+mode+' endpoint and config token',flush=True)
    for name,version,program,body in [('static-service','1.2.3','/opt/static/service','static-arm64-ok\n'),('dynamic-docker','2.3.4','/opt/service/bin/service','dynamic-arm64-library-ok\n'),('dynamic-local','2.3.4','/opt/service/bin/service','dynamic-arm64-library-ok\n')]:
        project=WORK/name;project.mkdir(exist_ok=True)
        (project/'unikernel.toml').write_text(f'[build]\nlang="raw"\npkg_source="jerboa"\npkgs=["{name}"]\n[program]\npath="{program}"\n')
        cli('build',project,'--platform','linux/arm64','--name',name)
        m=json.loads(cli('images','inspect',name+':latest'))
        assert m['architecture']=='arm64' and m['platform']=='linux/arm64',m
        assert len(m['packages'])==1 and m['packages'][0]['version']==version,m
        p=next(p for p in packages if p['name']==name)
        assert m['packages'][0]['sha256']==p['sha256']
        assert m['packages'][0]['provenance']==p['provenance']
        (WORK/(name+'-manifest.json')).write_text(json.dumps(m,indent=2))
        with socket.socket() as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
        vm=cli('run',name+':latest','-p',f'127.0.0.1:{port}:8080');active.append(vm)
        def get():
            with urllib.request.urlopen(f'http://127.0.0.1:{port}',timeout=2) as r:return r.read().decode()
        assert wait(get,name+' HTTP')==body
        cli('stop',vm)
        old=vm;vm=rpc('VM.Start',{'id':vm})['id'];active.remove(old);active.append(vm)
        assert wait(get,name+' restarted HTTP')==body
        cli('stop',vm);cli('rm',vm);active.remove(vm)
        print('PASS:',name,'metadata, HTTP, stop/start, HTTP',flush=True)
    # Metadata-only integration fixtures: the Go ELF stands in for a runtime.
    # This checks automatic resolution/stages, not JavaScript compatibility.
    cli('pkg','create','node:20',static,'--platform','linux/arm64','--program-path','/node')
    auto=WORK/'auto';auto.mkdir()
    package_json='{"engines":{"node":"20"},"main":"index.js"}'
    (auto/'package.json').write_text(package_json);(auto/'index.js').write_text('// metadata fixture\n')
    (auto/'node_modules').mkdir()
    stamp=hashlib.sha256(b'20\ndarwin/arm64')
    for filename in ['package.json','package-lock.json','npm-shrinkwrap.json','.npmrc']:
        stamp.update(filename.encode())
        if (auto/filename).exists():stamp.update((auto/filename).read_bytes())
    (auto/'node_modules/.jerboa-deps-stamp').write_text(stamp.hexdigest())
    (auto/'unikernel.toml').write_text('[build]\nlang="node"\npkg_source="jerboa"\n')
    cli('build',auto,'--platform','linux/arm64','--name','auto-metadata')
    m=json.loads(cli('images','inspect','auto-metadata:latest'))
    assert [(p['name'],p['version']) for p in m['packages']]==[('node','20')],m
    (auto/'main.go').write_text((ROOT/'tests/fixtures/pkg-arm64/static.go').read_text().replace('//go:build ignore\n\n',''))
    (auto/'go.mod').write_text('module pkg-stage-fixture\n\ngo 1.25\n')
    for copied in [False,True]:
        config='[build]\npkg_source="jerboa"\n[[stages]]\nname="runtime"\nlang="node"\n[[stages]]\nname="final"\nlang="go"\n'
        if copied:config+='copy_from=[{stage="runtime",src="/node",dst="/copied-node"}]\n'
        (auto/'unikernel.toml').write_text(config)
        name='stages-copied' if copied else 'stages-discarded'
        cli('build',auto,'--platform','linux/arm64','--name',name)
        m=json.loads(cli('images','inspect',name+':latest'))
        assert m['platform']=='linux/arm64'
        assert [p['name'] for p in m.get('packages',[])]==(['node'] if copied else []),m
        (WORK/(name+'-manifest.json')).write_text(json.dumps(m,indent=2))
    print('PASS: automatic resolution; stages preserve only contributing package references (runtime surrogate)',flush=True)
    # Cross-arch package creation and mandatory no-preflight rejection.
    amd64=WORK/'static-amd64'
    run(['go','build','-o',amd64,ROOT/'tests/fixtures/pkg-arm64/static.go'],env=dict(env,GOOS='linux',GOARCH='amd64',CGO_ENABLED='0'))
    cli('pkg','create','static-service:1.2.3',amd64,'--platform','linux/amd64')
    cli('build',amd64,'--platform','linux/arm64','--no-preflight','--name','wrong',success=False)
    # A valid amd64 image may be built but cannot reserve a VM on native macOS.
    cli('build',amd64,'--platform','linux/amd64','--name','amd64-image')
    before=rpc('VM.List',{})
    error=cli('run','amd64-image:latest',success=False)
    assert 'ARM64' in error,error
    try:rpc('VM.Run',{'image':'amd64-image:latest','network_name':'uncreated-pkg-network'})
    except RuntimeError as error:assert 'ARM64' in str(error),error
    else:raise AssertionError('incompatible API run succeeded')
    assert rpc('VM.List',{})==before, 'incompatible image created a VM'
    print('PASS: variants coexist; incompatible build/run rejected',flush=True)
finally:
    for vm in active:
        try:cli('stop',vm);cli('rm',vm)
        except Exception:pass
    daemon.terminate()
    try:daemon.wait(10)
    except subprocess.TimeoutExpired:daemon.kill();daemon.wait()
    log.close();transcript.close()
