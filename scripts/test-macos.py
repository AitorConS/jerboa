#!/usr/bin/env python3
"""Native HVF smoke: RPC auth, ARM64 build/boot, env, HTTP, volume persistence.
Run after build-macos.sh. All engine data lives in a temporary directory.
"""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request
import http.server
import threading

ROOT = Path(__file__).resolve().parent.parent
BIN = Path(os.getenv('JERBOA_TEST_BIN', str(ROOT / 'dist/macos-arm64')))
PROGRAM = r'''package main
import("fmt";"net/http";"os";"strconv";"strings";"runtime";"net";"io";"time")
func main(){
 if code:=os.Getenv("EXIT_CODE");code!=""{n,_:=strconv.Atoi(code);fmt.Println("GUEST_EXIT",n);os.Exit(n)}
 go func(){conn,err:=net.ListenPacket("udp",":8081");if err!=nil{panic(err)};for{b:=make([]byte,1024);n,addr,err:=conn.ReadFrom(b);if err!=nil{return};conn.WriteTo(b[:n],addr)}}()
 http.HandleFunc("/peer",func(w http.ResponseWriter,r *http.Request){c:=http.Client{Timeout:2*time.Second};port:=r.URL.Query().Get("port");if port==""{port="8080"};resp,err:=c.Get("http://"+r.URL.Query().Get("target")+":"+port+"/");if err!=nil{http.Error(w,err.Error(),502);return};defer resp.Body.Close();io.Copy(w,resp.Body)})
 b,_:=os.ReadFile("/data/count")
 n,_:=strconv.Atoi(strings.TrimSpace(string(b)));n++
 f,err:=os.OpenFile("/data/count",os.O_CREATE|os.O_TRUNC|os.O_WRONLY,0600)
 if err!=nil{panic(err)}
 if _,err=fmt.Fprint(f,n);err!=nil{panic(err)}
 if err=f.Sync();err!=nil{panic(err)};f.Close()
 fmt.Println("NATIVE_SMOKE_READY",n,os.Getenv("NATIVE_MARKER"))
 http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){
 fmt.Fprintf(w,`{"arch":%q,"marker":%q,"count":%d}`,runtime.GOARCH,os.Getenv("NATIVE_MARKER"),n)
 })
 if err=http.ListenAndServe(":8080",nil);err!=nil{panic(err)}
}'''

def main():
    if os.uname().sysname != 'Darwin' or os.uname().machine != 'arm64':
        raise SystemExit('Requires macOS ARM64 with HVF')
    go = shutil.which('go')
    qemu = shutil.which('qemu-system-aarch64')
    if not go or not qemu:
        raise SystemExit('Put Go and qemu-system-aarch64 on PATH')
    with tempfile.TemporaryDirectory(prefix='jerboa-hvf-', dir='/tmp') as td:
        work = Path(td)
        (work/'home').mkdir()
        env = dict(os.environ, HOME=str(work/'home'), JERBOA_HOST='unix://'+str(work/'daemon.sock'), JERBOA_AUTH_TOKEN='isolated-native-smoke', JERBOA_DNS_UPSTREAM='')
        project=work/'app'
        project.mkdir()
        (project/'main.go').write_text(PROGRAM)
        (project/'go.mod').write_text('module native-smoke\n\ngo 1.25\n')
        (project/'unikernel.toml').write_text('[build]\nlang = "go"\ndirs = ["/data"]\n')
        def cli(*args):
            r = subprocess.run([str(BIN/'jerboa'), *args], env=env, text=True, capture_output=True, timeout=180 if args[0]=="build" else 45)
            if r.returncode:
                raise RuntimeError(f'{args}: {r.stdout}\n{r.stderr}')
            return r.stdout.strip()
        with (work/'daemon.log').open('w') as log:
            daemon = subprocess.Popen([str(BIN/'jerboad'), '--host', env['JERBOA_HOST'], '--tools-dir', str(BIN/'tools'), '--qemu', qemu], env=env, stdout=log, stderr=log)
            vm = None
            try:
                for _ in range(100):
                    if (work/'daemon.sock').exists(): break
                    if daemon.poll() is not None: raise RuntimeError('daemon exited during startup')
                    time.sleep(.05)
                cli('status')
                cli('build', str(project), '--platform', 'linux/arm64', '--name', 'native-smoke')
                cli('volume', 'create', 'native-data', '--size', '32M')
                with socket.socket() as probe:
                    probe.bind(('127.0.0.1',0)); port=probe.getsockname()[1]
                for expected in (1,2):
                    vm=cli('run','native-smoke:latest','--cpus','2','-p',f'127.0.0.1:{port}:8080','-e','NATIVE_MARKER=hvf-arm64','-v','native-data:/data')
                    deadline=time.monotonic()+20
                    while True:
                        try:
                            with urllib.request.urlopen(f'http://127.0.0.1:{port}',timeout=1) as response:
                                result=json.load(response)
                            break
                        except (OSError,ValueError):
                            if time.monotonic()>deadline: raise RuntimeError('HTTP never became ready: '+cli('logs',vm))
                            time.sleep(.1)
                    assert result=={'arch':'arm64','marker':'hvf-arm64','count':expected}, result
                    print(f'HVF boot {expected}: ARM64, environment, HTTP and volume counter={expected} OK',flush=True)
                    cli('stop',vm)
                    cli('rm',vm)
                    vm=None
                def wait_for(check, label, timeout=25):
                    deadline=time.monotonic()+timeout
                    last=None
                    while time.monotonic()<deadline:
                        try:
                            result=check()
                            if result:return result
                        except (OSError,ValueError,RuntimeError) as e:last=e.read().decode() if hasattr(e,"read") else str(e)
                        time.sleep(.2)
                    raise RuntimeError('Timed out: '+label+' '+str(last))
                def info(name):return json.loads(cli('inspect',name))
                def get(url):
                    with urllib.request.urlopen(url,timeout=3) as r:return json.load(r)
                cli('network','create','shared')
                peer=cli('run','native-smoke:latest','--name','peer','--network','shared','--ip','10.100.0.10','--health-check','tcp:8080')
                vm=cli('run','native-smoke:latest','--name','client','--network','shared','-p',f'127.0.0.1:{port}:8080','-p',f'127.0.0.1:{port}:8081/udp','--health-check','http:8080:/')
                wait_for(lambda:get(f'http://127.0.0.1:{port}/peer?target=peer'), 'inter-VM DNS and HTTP')
                wait_for(lambda:info('peer').get('health')=='healthy','unpublished TCP health')
                wait_for(lambda:info('client').get('health')=='healthy','HTTP health')
                with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as udp:
                    udp.settimeout(3);udp.sendto(b'native-udp',('127.0.0.1',port));assert udp.recv(1024)==b'native-udp'
                print('Shared network, static IP, guest DNS, unpublished TCP health, HTTP health and UDP forwarding OK',flush=True)
                # A host port collision fails without disturbing its existing owner.
                r=subprocess.run([str(BIN/'jerboa'),'run','native-smoke:latest','--network','shared','-p',f'127.0.0.1:{port}:8080'],env=env,text=True,capture_output=True,timeout=10)
                assert r.returncode and 'publish ports' in r.stderr,r
                assert get(f'http://127.0.0.1:{port}/')['arch']=='arm64'
                class HostHandler(http.server.BaseHTTPRequestHandler):
                    def do_GET(self):
                        self.send_response(200);self.end_headers();self.wfile.write(b'{"host":true}')
                    def log_message(self,*args):pass
                host=http.server.ThreadingHTTPServer(('127.0.0.1',0),HostHandler)
                threading.Thread(target=host.serve_forever,daemon=True).start()
                try:assert get(f'http://127.0.0.1:{port}/peer?target=10.100.0.1&port={host.server_port}')=={'host':True}
                finally:host.shutdown();host.server_close()
                cli('network','create','separate')
                other=cli('run','native-smoke:latest','--name','isolated','--network','separate')
                r=subprocess.run([str(BIN/'jerboa'),'dns','resolve','peer','--network','separate'],env=env,text=True,capture_output=True,timeout=10)
                assert r.returncode,r
                cli('stop',other);cli('rm',other);cli('network','rm','separate')
                # Abrupt replacement preserves QEMU; the next daemon restores links,
                # forwarding and health checks to those same VM IDs.
                daemon.kill();daemon.wait(10)
                daemon=subprocess.Popen([str(BIN/'jerboad'),'--host',env['JERBOA_HOST'],'--tools-dir',str(BIN/'tools'),'--qemu',qemu],env=env,stdout=log,stderr=log)
                wait_for(lambda:get(f'http://127.0.0.1:{port}/peer?target=peer'),'network recovery')
                assert info('client')['id']==vm and info('peer')['id']==peer
                wait_for(lambda:info('peer').get('health')=='healthy','recovered health checks')
                print('Host gateway access, port collision rollback, DNS scope and daemon-crash network recovery OK',flush=True)
                cli('stop',vm);cli('rm',vm);vm=None
                cli('stop',peer);cli('rm',peer);cli('network','rm','shared')
                for code in (0,3):
                    name='exit-'+str(code)
                    vm=cli('run','native-smoke:latest','--name',name,'-e','EXIT_CODE='+str(code),'--restart','on-failure:1')
                    wait_for(lambda:info(name)['state']=='stopped' and info(name).get('restart_count',0)==(1 if code else 0),'guest exit '+str(code))
                    if code==0:
                        time.sleep(2);assert info(name).get('restart_count',0)==0
                    cli('rm',name);vm=None
                print('HVF guest exit(0) does not restart; exit(3) restarts exactly once OK',flush=True)
                stack=work/'compose.yaml'
                stack.write_text(f"version: '1'\nservices:\n  db:\n    image: native-smoke:latest\n    networks: [app]\n  web:\n    image: native-smoke:latest\n    depends_on: [db]\n    networks: [app]\n    ports: ['127.0.0.1:{port}:8080']\nnetworks:\n  app:\n    driver: bridge\n")
                cli('compose','up',str(stack))
                wait_for(lambda:get(f'http://127.0.0.1:{port}/peer?target=db'),'Compose guest service DNS')
                cli('compose','down',str(stack))
                print('Compose dependency startup, service DNS and teardown OK',flush=True)
                # Native raw-disk entry point and sampled resource enforcement.
                raw=next((work/'home/.jerboa/images').glob('*/disk.img'))
                vm=cli('run',str(raw),'-p',f'127.0.0.1:{port}:8080','--cpu-shares','100','--memory-max','512M')
                wait_for(lambda:get(f'http://127.0.0.1:{port}/'),'raw ARM64 disk')
                stats=json.loads(cli('stats',vm,'--output','json'))
                assert stats['source']=='darwin-libproc' and stats['mem_bytes']>0 and stats['net_rx_bytes']>0 and stats['net_tx_bytes']>0,stats
                assert any('watchdog' in w for w in info(vm).get('warnings',[]))
                cli('stop',vm);cli('rm',vm);vm=None
                vm=cli('run','native-smoke:latest','--memory-max','1M')
                wait_for(lambda:info(vm)['state']=='stopped','memory watchdog')
                assert any('exceeded' in w for w in info(vm).get('warnings',[]))
                cli('rm',vm);vm=None
                print('Raw ARM64 disks, native accounting, CPU priority and RSS watchdog OK',flush=True)
                # Legacy x86 images must be rejected unless explicitly emulated.
                if (BIN/'tools/x86/boot.img').exists():
                    cli('build',str(project),'--platform','linux/amd64','--name','x86-smoke')
                    r=subprocess.run([str(BIN/'jerboa'),'run','x86-smoke:latest'],env=env,text=True,capture_output=True,timeout=10)
                    assert r.returncode and 'ARM64' in r.stderr,r
                    vm=cli('run','x86-smoke:latest','--emulate-x86','-p',f'127.0.0.1:{port}:8080')
                    result=wait_for(lambda:get(f'http://127.0.0.1:{port}/'),'explicit x86 compatibility',45)
                    assert result['arch']=='amd64',result
                    cli('stop',vm);cli('rm',vm);vm=None
                    print('Explicit x86 TCG compatibility boots; no implicit emulation OK',flush=True)
                else: print('x86 compatibility skipped: optional tools/x86 not installed',flush=True)
                if os.getenv('JERBOA_TEST_RUNTIMES')=='1':
                    node=work/'node';node.mkdir();(node/'node_modules').mkdir()
                    (node/'package.json').write_text('{"name":"native-node-test","version":"1.0.0","main":"index.js","engines":{"node":"20"}}')
                    (node/'index.js').write_text("require('http').createServer((req,res)=>res.end(JSON.stringify({runtime:'node',arch:process.arch}))).listen(8080)")
                    python=work/'python';python.mkdir()
                    (python/'pyproject.toml').write_text('[project]\nname="native-python-test"\nrequires-python=">=3.10"\n')
                    (python/'main.py').write_text("from http.server import BaseHTTPRequestHandler, HTTPServer\nimport json,platform\nclass Handler(BaseHTTPRequestHandler):\n def do_GET(self):\n  self.send_response(200);self.end_headers();self.wfile.write(json.dumps({'runtime':'python','arch':platform.machine()}).encode())\nHTTPServer(('0.0.0.0',8080),Handler).serve_forever()\n")
                    for runtime_project in (node,python):
                        cli('build',str(runtime_project),'--platform','linux/arm64','--name',runtime_project.name)
                        vm=cli('run',runtime_project.name+':latest','--memory','512M','-p',f'127.0.0.1:{port}:8080')
                        result=wait_for(lambda:get(f'http://127.0.0.1:{port}/'),runtime_project.name+' ARM64 runtime',45)
                        assert result['runtime']==runtime_project.name and result['arch'] in ('arm64','aarch64'),result
                        cli('stop',vm);cli('rm',vm);vm=None
                        print(runtime_project.name+' ARM64 runtime package, image build and HTTP OK',flush=True)
                assert 'WARNING: DATA RACE' not in (work/'daemon.log').read_text()
                print('Native parity smoke passed',flush=True)
            except Exception:
                print((work/'daemon.log').read_text())
                for name in ('peer','client'):
                    try:print(name,cli('logs',name))
                    except Exception:pass
                raise
            finally:
                try:
                    for item in json.loads(cli('ps','--output','json')):
                        try:cli('kill',item['id'])
                        except Exception:pass
                except Exception:
                    if vm:
                        try:cli('kill',vm)
                        except Exception:pass
                daemon.terminate()
                try:daemon.wait(10)
                except subprocess.TimeoutExpired:daemon.kill();daemon.wait()

if __name__=='__main__':
    main()
