#!/usr/bin/env python3
"""Real HVF shared-network matrix; reuses the native guest from test-macos.py."""
import importlib.util
import http.server
import threading
import struct
import shutil
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.request
import urllib.error

ROOT = Path(__file__).resolve().parents[1]
BIN = Path(os.environ['JERBOA_TEST_BIN']).resolve()
FC = Path(os.environ['JERBOA_FIRECRACKER_BIN']).resolve()
spec = importlib.util.spec_from_file_location('native_smoke', ROOT/'scripts/test-macos.py')
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)

def main():
    with tempfile.TemporaryDirectory(prefix='jb-fc-net-',dir='/tmp') as td:
        work=Path(td);(work/'home').mkdir();project=work/'app';project.mkdir()
        program=native.PROGRAM.replace('func main(){','''func main(){
 go func(){ln,err:=net.Listen("tcp",":8082");if err!=nil{panic(err)};for{c,err:=ln.Accept();if err!=nil{return};go func(){defer c.Close();b,err:=io.ReadAll(c);if err==nil{c.Write(append([]byte("final:"),b...))}}()}}()
 http.HandleFunc("/lookup",func(w http.ResponseWriter,r *http.Request){ips,err:=net.LookupHost(r.URL.Query().Get("name"));if err!=nil{http.Error(w,err.Error(),502);return};fmt.Fprintf(w,"%q",strings.Join(ips,","))})
 http.HandleFunc("/udppeer",func(w http.ResponseWriter,r *http.Request){c,err:=net.DialTimeout("udp",r.URL.Query().Get("target"),time.Second);if err!=nil{http.Error(w,err.Error(),502);return};defer c.Close();c.SetDeadline(time.Now().Add(time.Second));c.Write([]byte("guest-udp"));b:=make([]byte,100);n,err:=c.Read(b);if err!=nil{http.Error(w,err.Error(),502);return};fmt.Fprintf(w,"%q",string(b[:n]))})
''')
        (project/'main.go').write_text(program)
        (project/'go.mod').write_text('module shared-smoke\n\ngo 1.25\n')
        (project/'unikernel.toml').write_text('[build]\nlang="go"\ndirs=["/data"]\n')
        env=dict(os.environ,JERBOA_HOST='unix://'+str(work/'daemon.sock'),JERBOA_AUTH_TOKEN='isolated-shared-smoke')
        for key in ('GOCACHE','GOMODCACHE'):env[key]=subprocess.check_output(['go','env',key],text=True).strip()
        env['HOME']=str(work/'home')
        def cli(*args,ok=True):
            r=subprocess.run([str(BIN/'jerboa'),*args],env=env,text=True,capture_output=True,timeout=180)
            if ok and r.returncode:raise RuntimeError(f'{args}: {r.stdout}\n{r.stderr}')
            if not ok:
                assert r.returncode,(args,r.stdout)
                return r.stderr
            return r.stdout.strip()
        def wait(check,label,timeout=35):
            end=time.monotonic()+timeout;last=''
            while time.monotonic()<end:
                try:
                    v=check()
                    if v:return v
                except (OSError,ValueError,RuntimeError) as e:last=str(e)
                time.sleep(.15)
            raise RuntimeError('timeout '+label+' '+last)
        def get(url):
            with urllib.request.urlopen(url,timeout=4) as r:return json.load(r)
        def info(name):return json.loads(cli('inspect',name))
        with socket.socket() as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
        with socket.socket() as s:s.bind(('127.0.0.1',0));halfport=s.getsockname()[1]
        url=f'http://127.0.0.1:{port}'
        with (work/'daemon.log').open('w') as log:
            args=[str(BIN/'jerboad'),'--host',env['JERBOA_HOST'],'--tools-dir',str(BIN/'tools'),'--hypervisor','firecracker','--fc-bin',str(FC)]
            daemon=subprocess.Popen(args,env=env,stdout=log,stderr=log)
            try:
                wait(lambda:cli('status'),'daemon')
                cli('build',str(project),'--platform','linux/arm64','--name','shared-smoke')
                cli('network','create','shared','--subnet','172.25.40.0/24')
                peer=cli('run','shared-smoke:latest','--name','peer','--network','shared','--ip','172.25.40.10','--health-check','tcp:8080')
                client=cli('run','shared-smoke:latest','--name','client','--network','shared','--ip','172.25.40.11','-p',f'127.0.0.1:{port}:8080','-p',f'127.0.0.1:{port}:8081/udp','-p',f'127.0.0.1:{halfport}:8082')
                wait(lambda:get(url+'/peer?target=172.25.40.10'),'direct IP')
                wait(lambda:get(url+'/peer?target=peer'),'service DNS')
                wait(lambda:info('peer').get('health')=='healthy','unpublished health')
                with socket.socket(type=socket.SOCK_DGRAM) as s:
                    s.settimeout(3);s.sendto(b'shared-udp',('127.0.0.1',port));assert s.recv(100)==b'shared-udp'
                with socket.create_connection(('127.0.0.1',halfport),timeout=3) as c:
                    c.sendall(b'request');c.shutdown(socket.SHUT_WR);reply=b''
                    while True:
                        b=c.recv(100)
                        if not b:break
                        reply+=b
                    assert reply==b'final:request',reply
                for proto in ('','/udp'):
                    err=cli('run','shared-smoke:latest','--network','shared','-p',f'127.0.0.1:{port}:8080{proto}',ok=False)
                    assert 'publish ports' in err,err
                cli('network','create','isolated','--subnet','172.25.41.0/24')
                cli('run','shared-smoke:latest','--name','other','--network','isolated','--ip','172.25.41.10','--health-check','tcp:8080')
                wait(lambda:info('other').get('health')=='healthy','isolated readiness')
                for target in ('other','172.25.41.10','example.com','172.25.40.1'):
                    try:get(url+'/peer?target='+target)
                    except urllib.error.HTTPError as e:assert e.code==502
                    else:raise AssertionError('unexpected access '+target)
                print('PASS: two HVF VMs, static IP, service DNS, unpublished health, TCP/UDP, collisions, cross-network IP/DNS isolation, default-denied egress/DNS',flush=True)
                daemon.kill();daemon.wait(10);daemon=subprocess.Popen(args,env=env,stdout=log,stderr=log)
                wait(lambda:get(url+'/peer?target=peer'),'daemon replacement')
                assert info('peer')['id']==peer and info('client')['id']==client
                wait(lambda:info('peer').get('health')=='healthy','restored health')
                for name in ('client','peer','other'):cli('stop',name);cli('rm',name)
                cli('network','rm','shared');cli('network','rm','isolated')
                print('PASS: daemon SIGKILL/restart reconnects same VMs, probes and ports; teardown',flush=True)
                stack=work/'compose.yaml'
                stack.write_text(f'''services:
  postgres:
    image: shared-smoke:latest
    health_check: tcp:8080
    networks:
      app:
        ipv4_address: 172.25.42.10
        aliases: [database.app, db.internal]
  database:
    image: shared-smoke:latest
    networks:
      app:
        ipv4_address: 172.25.42.20
  backend:
    image: shared-smoke:latest
    depends_on: [postgres]
    networks: [app]
    ports: ['127.0.0.1:{port}:8080']
networks:
  app:
    ipam:
      config:
        - subnet: 172.25.42.0/24
''')
                cli('compose','up',str(stack))
                for name in ('postgres','database.app','database.app.app','db.internal','postgres.app'):
                    wait(lambda:get(url+'/peer?target='+name),'Compose '+name)
                    assert get(url+'/lookup?name='+name)=='172.25.42.10',name
                assert get(url+'/lookup?name=database')=='172.25.42.20'
                print('PASS: dotted alias wins over colliding service, qualified names resolve to exact IP',flush=True)
                cli('compose','down',str(stack))
                for typ in (socket.SOCK_STREAM,socket.SOCK_DGRAM):
                    with socket.socket(type=typ) as s:
                        s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',port))
                print('PASS: Compose IPAM, dependencies, service/alias DNS, teardown and port reuse (HTTP fixture, not PostgreSQL)',flush=True)
                implicit=work/'implicit.yaml'
                implicit.write_text(f"services:\n  implicit-peer:\n    image: shared-smoke:latest\n  implicit-client:\n    image: shared-smoke:latest\n    ports: ['127.0.0.1:{port}:8080']\n")
                cli('compose','up',str(implicit))
                wait(lambda:get(url+'/peer?target=implicit-peer'),'implicit Compose network')
                cli('compose','down',str(implicit))
                print('PASS: Compose implicit default network and service DNS',flush=True)
                # Host gateway and DNS permissions are exercised against local,
                # controlled servers; no public Internet dependency is needed.
                class Host(http.server.BaseHTTPRequestHandler):
                    calls=0
                    def do_GET(self):
                        Host.calls+=1;self.send_response(200);self.end_headers();self.wfile.write(b'{"host":true}')
                    def log_message(self,*args):pass
                host=http.server.ThreadingHTTPServer(('127.0.0.1',0),Host)
                threading.Thread(target=host.serve_forever,daemon=True).start()
                dns=socket.socket(type=socket.SOCK_DGRAM);dns.bind(('127.0.0.1',0));dns.settimeout(.2)
                echo=socket.socket(type=socket.SOCK_DGRAM);echo.bind(('127.0.0.1',0));echo.settimeout(.2)
                stop=threading.Event();queries=[]
                def dns_loop():
                    while not stop.is_set():
                        try:
                            q,a=dns.recvfrom(4096);queries.append(q)
                            pos=12
                            while q[pos]:pos+=q[pos]+1
                            end=pos+5;kind=struct.unpack('!H',q[pos+1:pos+3])[0]
                            answer=b'\xc0\x0c'+struct.pack('!HHIH',1,1,30,4)+bytes([192,0,2,25]) if kind==1 else b''
                            dns.sendto(q[:2]+struct.pack('!HHHHH',0x8180,1,bool(answer),0,0)+q[12:end]+answer,a)
                        except socket.timeout:pass
                def echo_loop():
                    while not stop.is_set():
                        try:b,a=echo.recvfrom(4096);echo.sendto(b,a)
                        except socket.timeout:pass
                threads=[threading.Thread(target=dns_loop),threading.Thread(target=echo_loop)]
                for thread in threads:thread.start()
                try:
                    cli('network','create','policy','--subnet','172.25.43.0/24')
                    def run_policy():return cli('run','shared-smoke:latest','--name','policy-client','--network','policy','-p',f'127.0.0.1:{port}:8080')
                    run_policy();wait(lambda:get(url),'policy client')
                    for path in (f'/peer?target=172.25.43.1&port={host.server_port}',f'/udppeer?target=172.25.43.1:{echo.getsockname()[1]}','/lookup?name=allowed.test'):
                        try:get(url+path)
                        except urllib.error.HTTPError as e:assert e.code==502
                        else:raise AssertionError('default policy allowed '+path)
                    assert Host.calls==0 and not queries
                    cli('stop','policy-client');cli('rm','policy-client')
                    daemon.terminate();daemon.wait(10)
                    policy=work/'security.json'
                    policy.write_text(json.dumps({'version':1,'egress':[{'protocol':'tcp','address':'127.0.0.1','port':host.server_port},{'protocol':'udp','address':'127.0.0.1','port':echo.getsockname()[1]}],'dns':{'address':'127.0.0.1','port':dns.getsockname()[1]}}))
                    args+=['--fc-security',str(policy)]
                    daemon=subprocess.Popen(args,env=env,stdout=log,stderr=log);wait(lambda:cli('status'),'policy daemon')
                    run_policy();wait(lambda:get(url),'authorized client')
                    assert get(url+f'/peer?target=172.25.43.1&port={host.server_port}')=={'host':True}
                    assert get(url+f'/udppeer?target=172.25.43.1:{echo.getsockname()[1]}')=='guest-udp'
                    assert get(url+'/lookup?name=allowed.test')=='192.0.2.25'
                    assert Host.calls>0 and queries
                    cli('stop','policy-client');cli('rm','policy-client');cli('network','rm','policy')
                    print('PASS: explicit TCP/UDP host gateway and DNS permissions; controlled servers see zero requests when denied',flush=True)
                finally:
                    stop.set()
                    for thread in threads:thread.join(2)
                    dns.close();echo.close();host.shutdown();host.server_close()
                assert 'WARNING: DATA RACE' not in (work/'daemon.log').read_text()
                assert 'Unsolicited response received' not in (work/'daemon.log').read_text()
            except Exception:
                print((work/'daemon.log').read_text(),flush=True)
                if os.getenv('JERBOA_TEST_FAILURE_DIR'):shutil.copytree(work,os.environ['JERBOA_TEST_FAILURE_DIR'],dirs_exist_ok=True)
                try:
                    for v in json.loads(cli('ps','--output','json')):print(v,cli('logs',v['id']),flush=True)
                except Exception:pass
                raise
            finally:
                if daemon.poll() is not None:
                    daemon=subprocess.Popen(args,env=env,stdout=log,stderr=log)
                    try:wait(lambda:cli('status'),'cleanup recovery',10)
                    except Exception:pass
                try:
                    for v in json.loads(cli('ps','--output','json')):
                        try:cli('kill',v['id'])
                        except Exception:pass
                except Exception:pass
                daemon.terminate()
                try:daemon.wait(10)
                except subprocess.TimeoutExpired:daemon.kill();daemon.wait()
if __name__=='__main__':main()
