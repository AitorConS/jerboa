#!/usr/bin/env python3
"""Exercise the native CLI/daemon against the macOS Firecracker fork."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
BIN = Path(os.getenv('JERBOA_TEST_BIN', str(ROOT / 'dist/macos-arm64')))
FC = Path(os.getenv('JERBOA_FIRECRACKER_BIN', str(ROOT.parent / 'firecracker-macos/build/macos-arm64/firecracker')))
PROGRAM = r'''package main
import("encoding/json";"fmt";"net";"net/http";"os";"runtime";"strconv";"time")
func main(){
 if os.Getenv("MODE")=="fail"{fmt.Println("EXPECTED_GUEST_FAILURE");os.Exit(7)}
 path:="/data/count";b,_:=os.ReadFile(path);n,_:=strconv.Atoi(string(b));n++
 f,e:=os.Create(path);if e!=nil{panic(e)};fmt.Fprint(f,n);if e=f.Sync();e!=nil{panic(e)};f.Close()
 go func(){c,e:=net.ListenPacket("udp",":8081");if e!=nil{panic(e)};defer c.Close();for{b:=make([]byte,1024);n,a,e:=c.ReadFrom(b);if e!=nil{return};c.WriteTo(b[:n],a)}}()
 http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){json.NewEncoder(w).Encode(map[string]any{"count":n,"arch":runtime.GOARCH,"cpus":runtime.NumCPU(),"marker":os.Getenv("MARKER")})})
 http.HandleFunc("/shutdown",func(w http.ResponseWriter,r *http.Request){fmt.Fprint(w,"bye");go func(){time.Sleep(100*time.Millisecond);os.Exit(0)}()})
 fmt.Println("GUEST_READY");if e=http.ListenAndServe(":8080",nil);e!=nil{panic(e)}
}'''


def main():
    if os.uname().sysname != 'Darwin' or os.uname().machine != 'arm64':
        raise SystemExit('Requires macOS ARM64 with Hypervisor.framework')
    if not shutil.which('go') or not FC.is_file():
        raise SystemExit('Put Go on PATH and set JERBOA_FIRECRACKER_BIN to the signed fork executable')
    with tempfile.TemporaryDirectory(prefix='jerboa-fc-test-', dir='/tmp') as td:
        work = Path(td)
        (work / 'home').mkdir()
        project = work / 'app'
        project.mkdir()
        (project / 'main.go').write_text(PROGRAM)
        (project / 'go.mod').write_text('module hvf-smoke\n\ngo 1.25\n')
        (project / 'unikernel.toml').write_text('[build]\nlang = "go"\ndirs = ["/data"]\n')
        env = dict(os.environ, JERBOA_HOST='unix://' + str(work / 'daemon.sock'), JERBOA_AUTH_TOKEN='isolated-fc-smoke')
        # Keep the existing Go cache while isolating all Jerboa state.
        env['GOCACHE'] = subprocess.check_output(['go', 'env', 'GOCACHE'], text=True).strip()
        env['GOMODCACHE'] = subprocess.check_output(['go', 'env', 'GOMODCACHE'], text=True).strip()
        env['HOME'] = str(work / 'home')

        def cli(*args, success=True):
            r = subprocess.run([str(BIN / 'jerboa'), *args], env=env, text=True, capture_output=True, timeout=180)
            if success and r.returncode:
                raise RuntimeError(f'{args}: {r.stdout}\n{r.stderr}')
            if not success:
                assert r.returncode, f'{args} unexpectedly succeeded'
                return r.stderr
            return r.stdout.strip()

        def wait_for(check, label, timeout=40):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                try:
                    if check():
                        return
                except (OSError, ValueError, RuntimeError):
                    pass
                time.sleep(.1)
            raise RuntimeError('Timed out: ' + label)

        with (work / 'daemon.log').open('w') as log:
            daemon = subprocess.Popen([str(BIN / 'jerboad'), '--host', env['JERBOA_HOST'], '--tools-dir', str(BIN / 'tools'), '--hypervisor', 'firecracker', '--fc-bin', str(FC.resolve())], env=env, stdout=log, stderr=log)
            active = []
            try:
                wait_for(lambda: (work / 'daemon.sock').exists(), 'daemon startup')
                cli('status')
                cli('build', str(project), '--platform', 'linux/arm64', '--name', 'fc-smoke')
                cli('volume', 'create', 'fc-data', '--size', '32M')
                with socket.socket() as s:
                    s.bind(('127.0.0.1', 0))
                    port = s.getsockname()[1]
                with socket.socket(type=socket.SOCK_DGRAM) as s:
                    s.bind(('127.0.0.1', 0))
                    udp_port = s.getsockname()[1]
                url = f'http://127.0.0.1:{port}'

                def get():
                    with urllib.request.urlopen(url, timeout=1) as response:
                        return json.load(response)

                for count, cpus, bind in [(1, 4, ""), (2, 1, "127.0.0.1:"), (3, 1, "0.0.0.0:")]:
                    vm = cli('run', 'fc-smoke:latest', '--cpus', str(cpus), '-p', f'{bind}{port}:8080', '-p', f'{bind}{udp_port}:8081/udp', '-e', 'MARKER=hvf-smoke', '-v', 'fc-data:/data', '--health-check', 'http:8080:/')
                    active.append(vm)
                    wait_for(lambda: get() == {'count': count, 'cpus': cpus, 'arch': 'arm64', 'marker': 'hvf-smoke'}, 'guest HTTP, SMP, environment and volume')
                    with socket.socket(type=socket.SOCK_DGRAM) as s:
                        s.settimeout(3)
                        s.sendto(b'hvf-udp', ('127.0.0.1', udp_port))
                        assert s.recv(1024) == b'hvf-udp'
                    for published in (f'{bind}{port}:8080', f'{bind}{udp_port}:8081/udp'):
                        error = cli('run', 'fc-smoke:latest', '-p', published, success=False)
                        assert 'HVF boot failed' in error, error
                    assert get()['count'] == count
                    if count == 1:
                        daemon.kill()
                        daemon.wait(timeout=10)
                        daemon = subprocess.Popen([str(BIN / 'jerboad'), '--host', env['JERBOA_HOST'], '--tools-dir', str(BIN / 'tools'), '--hypervisor', 'firecracker', '--fc-bin', str(FC.resolve())], env=env, stdout=log, stderr=log)
                        wait_for(lambda: json.loads(cli('inspect', vm))['state'] == 'running', 'daemon recovery')
                        assert get()['count'] == count
                    with urllib.request.urlopen(url + '/shutdown', timeout=2) as response:
                        response.read()
                    wait_for(lambda: json.loads(cli('inspect', vm))['state'] == 'stopped', 'guest exit')
                    cli('rm', vm)
                    active.remove(vm)
                    print(f'Firecracker/HVF: {cpus} CPU, bind={bind or "omitted"}, HTTP/UDP, environment, volume count={count}, port collision and guest exit OK', flush=True)
                cli('run', 'fc-smoke:latest', '--cpus', '5', success=False)
                name = 'fc-failure'
                cli('run', 'fc-smoke:latest', '--name', name, '-e', 'MODE=fail', '--restart', 'on-failure:1')
                active.append(name)
                wait_for(lambda: (lambda info: info['state'] == 'stopped' and info.get('restart_count') == 1)(json.loads(cli('inspect', name))), 'failed guest restarts once')
                cli('rm', name)
                active.remove(name)
                vm = cli('run', 'fc-smoke:latest')
                active.append(vm)
                cli('stop', vm)
                wait_for(lambda: json.loads(cli('inspect', vm))['state'] == 'stopped', 'forced stop')
                cli('rm', vm)
                active.remove(vm)
                print('Daemon recovery, failed-guest restart and forced stop OK', flush=True)
            except Exception:
                for vm in active:
                    print(cli('logs', vm), flush=True)
                print((work / 'daemon.log').read_text(), flush=True)
                raise
            finally:
                for vm in active:
                    subprocess.run([str(BIN / 'jerboa'), 'kill', vm], env=env, capture_output=True, timeout=10)
                daemon.terminate()
                try:
                    daemon.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    daemon.kill()
                    daemon.wait()


if __name__ == '__main__':
    main()
