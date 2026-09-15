#!/usr/bin/env python3
"""End-to-end acceptance for Jerboa's integrated snapshot lifecycle (macOS HVF).

Drives an isolated jerboad (own HOME, TMPDIR, socket and dynamic localhost
ports) through the public CLI and raw JSON-RPC: create, in-place restore,
negative cases, races and daemon SIGKILL recovery on a real shared network.
It never signals processes whose command line lacks this run's private path.
"""
import argparse
import hashlib
import json
import os
import shutil
import signal
import socket
import stat
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

PROGRAM = r'''package main
import("fmt";"net/http";"os";"strconv";"strings";"net";"io";"time";"sync")
var mu sync.Mutex
var ramValue string
var bootToken=strconv.FormatInt(time.Now().UnixNano(),36)
func main(){
 go func(){conn,err:=net.ListenPacket("udp",":8081");if err!=nil{panic(err)};for{b:=make([]byte,1024);n,addr,err:=conn.ReadFrom(b);if err!=nil{return};conn.WriteTo(b[:n],addr)}}()
 go func(){ln,err:=net.Listen("tcp",":8082");if err!=nil{panic(err)};for{c,err:=ln.Accept();if err!=nil{return};go func(){defer c.Close();b,err:=io.ReadAll(c);if err==nil{c.Write(append([]byte("final:"),b...))}}()}}()
 http.HandleFunc("/peer",func(w http.ResponseWriter,r *http.Request){c:=http.Client{Timeout:2*time.Second};resp,err:=c.Get("http://"+r.URL.Query().Get("target")+":8080/boot");if err!=nil{http.Error(w,err.Error(),502);return};defer resp.Body.Close();io.Copy(w,resp.Body)})
 http.HandleFunc("/lookup",func(w http.ResponseWriter,r *http.Request){ips,err:=net.LookupHost(r.URL.Query().Get("name"));if err!=nil{http.Error(w,err.Error(),502);return};fmt.Fprintf(w,"%q",strings.Join(ips,","))})
 http.HandleFunc("/set",func(w http.ResponseWriter,r *http.Request){mu.Lock();ramValue=r.URL.Query().Get("v");mu.Unlock();fmt.Fprint(w,`"ok"`)})
 http.HandleFunc("/get",func(w http.ResponseWriter,r *http.Request){mu.Lock();v:=ramValue;mu.Unlock();fmt.Fprintf(w,"%q",v)})
 http.HandleFunc("/boot",func(w http.ResponseWriter,r *http.Request){fmt.Fprintf(w,"%q",bootToken)})
 http.HandleFunc("/disk",func(w http.ResponseWriter,r *http.Request){if v:=r.URL.Query().Get("v");v!=""{if err:=os.WriteFile("/data/marker",[]byte(v),0600);err!=nil{http.Error(w,err.Error(),500);return}};b,_:=os.ReadFile("/data/marker");fmt.Fprintf(w,"%q",string(b))})
 http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){fmt.Fprint(w,`{"ok":true}`)})
 if err:=http.ListenAndServe(":8080",nil);err!=nil{panic(err)}
}'''

RESULTS = {}


def record(key, value=True):
    RESULTS[key] = value
    print(f'PASS {key}: {json.dumps(value)}', flush=True)


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def wait(check, label, timeout=40, interval=0.1):
    end = time.monotonic() + timeout
    last = ''
    while time.monotonic() < end:
        try:
            value = check()
            if value:
                return value
        except Exception as error:  # noqa: BLE001 - probes are retried
            last = str(error)
        time.sleep(interval)
    raise RuntimeError(f'timeout waiting for {label}: {last}')


def get(url, timeout=4):
    with urllib.request.urlopen(url, timeout=timeout) as response:
        return json.load(response)


def http_error_code(url):
    try:
        get(url)
    except urllib.error.HTTPError as error:
        return error.code
    except (OSError, ValueError):
        return 'unreachable'
    return 200


class RPCError(Exception):
    pass


class RPC:
    def __init__(self, path, token):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(path)
        self.file = self.sock.makefile('rwb')
        self.seq = 0
        result = self.call('Auth.Hello', {'token': token, 'proto': 2})
        assert result['status'] == 'ok', result

    def call(self, method, params):
        self.seq += 1
        self.file.write(json.dumps({'jsonrpc': '2.0', 'id': self.seq, 'method': method, 'params': params}).encode() + b'\n')
        self.file.flush()
        line = self.file.readline()
        if not line:
            raise ConnectionError('daemon closed the connection')
        response = json.loads(line)
        if response.get('error'):
            raise RPCError(response['error']['message'])
        return response.get('result')

    def close(self):
        self.file.close()
        self.sock.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin-dir', type=Path, required=True, help='directory with the jerboa and jerboad under test')
    parser.add_argument('--firecracker-bin', type=Path, required=True)
    parser.add_argument('--tools-dir', type=Path, required=True)
    parser.add_argument('--go-bin', type=Path, required=True)
    parser.add_argument('--output-dir', type=Path, required=True)
    args = parser.parse_args()
    bindir, firecracker, tools = args.bin_dir.resolve(), args.firecracker_bin.resolve(), args.tools_dir.resolve()
    output = args.output_dir.resolve()
    output.mkdir(parents=True, exist_ok=True)
    artifacts = Path(tempfile.mkdtemp(prefix='run-', dir=output))

    work = Path(tempfile.mkdtemp(prefix='jbsnap-', dir='/tmp'))
    home, tmp = work / 'h', work / 't'
    home.mkdir()
    tmp.mkdir()
    sock = str(work / 'd.sock')
    token = 'snapshot-acceptance'
    env = dict(os.environ, HOME=str(home), TMPDIR=str(tmp), JERBOA_HOST='unix://' + sock, JERBOA_AUTH_TOKEN=token)
    env['PATH'] = str(args.go_bin.resolve().parent) + os.pathsep + env['PATH']
    for key in ('GOCACHE', 'GOMODCACHE'):
        env[key] = subprocess.check_output([str(args.go_bin), 'env', key], text=True).strip()
    store = home / '.jerboa' / 'snapshots'
    log_path = work / 'daemon.log'
    log = log_path.open('a')
    base_args = [str(bindir / 'jerboad'), '--host', env['JERBOA_HOST'], '--tools-dir', str(tools),
                 '--hypervisor', 'firecracker', '--fc-bin', str(firecracker), '--snapshot-max-count', '4']
    state = {'daemon': None}

    def cli(*argv, ok=True, timeout=240):
        r = subprocess.run([str(bindir / 'jerboa'), *argv], env=env, text=True, capture_output=True, timeout=timeout)
        if ok:
            if r.returncode:
                raise RuntimeError(f'{argv}: {r.stdout}\n{r.stderr}')
            return r.stdout.strip()
        assert r.returncode, (argv, r.stdout)
        return r.stderr

    def start_daemon(extra=()):
        state['daemon'] = subprocess.Popen(base_args + list(extra), env=env, stdout=log, stderr=log)
        wait(lambda: cli('status'), 'daemon', 60)

    def kill_daemon():
        state['daemon'].kill()
        state['daemon'].wait(10)

    def rpc(method, params):
        client = RPC(sock, token)
        try:
            return client.call(method, params)
        finally:
            client.close()

    def rpc_error(method, params):
        try:
            rpc(method, params)
        except RPCError as error:
            return str(error)
        raise AssertionError(f'{method} {params} unexpectedly succeeded')

    def inspect(name):
        return json.loads(cli('inspect', name))

    def snapshot_names():
        return {s['name'] for s in json.loads(cli('snapshot', 'ls', '--output', 'json') or '[]')}

    def my_processes(fragment=''):
        out = subprocess.run(['/bin/ps', '-axww', '-o', 'pid=,command='], text=True, capture_output=True).stdout
        rows = []
        for line in out.splitlines():
            pid, _, command = line.strip().partition(' ')
            if str(work) in command and 'firecracker' in command and fragment in command:
                rows.append((int(pid), command))
        return rows

    def supervisors(vm_id):
        return [p for p in my_processes(f'fc-{vm_id}.sock') if '--hvf-child' not in p[1]]

    def journal_entries():
        path = store / '.operations'
        return sorted(p.name for p in path.iterdir()) if path.exists() else []

    def staging_entries():
        path = store / '.staging'
        return sorted(p.name for p in path.iterdir()) if path.exists() else []

    def wait_state(name, want, timeout=60):
        return wait(lambda: inspect(name)['state'] == want, f'{name} {want}', timeout)

    def assert_clean_stopped(vm_id, label):
        info = inspect(vm_id)
        assert info['state'] == 'stopped', (label, info['state'])
        wait(lambda: not supervisors(vm_id), f'{label}: supervisor exit', 15)
        run_dir = tmp / f'jerboa-run-{os.getuid()}'
        leftovers = [p.name for p in run_dir.glob(f'*{vm_id}*')] if run_dir.exists() else []
        assert not leftovers, (label, leftovers)
        assert not journal_entries(), (label, journal_entries())

    def restore_ok(vm_id, name):
        result = rpc('VM.SnapshotRestore', {'id': vm_id, 'name': name})
        assert result['id'] == vm_id and result['state'] == 'running', result
        return result

    try:
        start_daemon()
        project = work / 'app'
        project.mkdir()
        (project / 'main.go').write_text(PROGRAM)
        (project / 'go.mod').write_text('module snapshot-fixture\n\ngo 1.25\n')
        (project / 'unikernel.toml').write_text('[build]\nlang="go"\ndirs=["/data"]\n')
        cli('build', str(project), '--platform', 'linux/arm64', '--name', 'snapfix')
        cli('network', 'create', 'snapnet', '--subnet', '172.25.60.0/24')
        cli('network', 'create', 'isolated', '--subnet', '172.25.61.0/24')

        cp = {k: free_port() for k in ('http', 'udp', 'tcp')}
        peer_port = free_port()
        rpc('VM.Run', {'image': 'snapfix:latest', 'name': 'peer', 'network_name': 'snapnet', 'ip_address': '172.25.60.10',
                       'static_ip': True, 'network_aliases': ['peer-alias'],
                       'port_maps': [{'host_port': peer_port, 'guest_port': 8080, 'protocol': 'tcp', 'bind_addr': '127.0.0.1'}]})
        client = rpc('VM.Run', {
            'image': 'snapfix:latest', 'name': 'client', 'network_name': 'snapnet', 'ip_address': '172.25.60.11',
            'static_ip': True, 'network_aliases': ['api.snapnet', 'svc-client'],
            'health_check': {'type': 'tcp', 'port': 8080, 'interval_seconds': 1, 'timeout_seconds': 1, 'retries': 1},
            'port_maps': [{'host_port': cp['http'], 'guest_port': 8080, 'protocol': 'tcp', 'bind_addr': '127.0.0.1'},
                          {'host_port': cp['udp'], 'guest_port': 8081, 'protocol': 'udp', 'bind_addr': '127.0.0.1'},
                          {'host_port': cp['tcp'], 'guest_port': 8082, 'protocol': 'tcp', 'bind_addr': '127.0.0.1'}]})['id']
        cli('run', 'snapfix:latest', '--name', 'other', '--network', 'isolated', '--ip', '172.25.61.10')
        url = f'http://127.0.0.1:{cp["http"]}'
        peer_url = f'http://127.0.0.1:{peer_port}'
        wait(lambda: get(url + '/boot'), 'client HTTP')
        wait(lambda: get(peer_url + '/boot'), 'peer HTTP')
        wait(lambda: get(url + '/peer?target=peer'), 'client->peer DNS/TCP')
        record('setup_two_networks_three_vms')

        # ---- create ----------------------------------------------------------
        assert get(url + '/set?v=before') == 'ok'
        assert get(url + '/disk?v=disk-before') == 'disk-before'
        boot1 = get(url + '/boot')
        t0 = time.monotonic()
        cli('snapshot', 'create', 'client', 'snap1')
        RESULTS['create_seconds'] = round(time.monotonic() - t0, 3)
        assert get(url + '/boot') == boot1 and get(url + '/get') == 'before'
        record('cli_create_resumes_guest_without_reboot')
        snap = {s['name']: s for s in json.loads(cli('snapshot', 'ls', '--output', 'json'))}['snap1']
        mac = '02:' + ':'.join(f'{b:02x}' for b in hashlib.sha256(client.encode()).digest()[:5])
        assert snap['vm_id'] == client and snap['ip_address'] == '172.25.60.11' and snap['mac'] == mac, snap
        assert snap['aliases'] == ['api.snapnet', 'svc-client'] and len(snap['ports']) == 3, snap
        assert snap['subnet'] == '172.25.60.0/24' and snap['gateway'] == '172.25.60.1' and snap['size_bytes'] > 256 << 20, snap
        assert json.loads(cli('snapshot', 'inspect', 'snap1'))['manifest_sha256'] == snap['manifest_sha256']
        RESULTS['snapshot_bytes'] = snap['size_bytes']
        record('list_inspect_identity_mac_ip_aliases_ports')
        for path in [store, store / 'snap1', store / 'snap1' / 'vm.snapshot']:
            assert stat.S_IMODE(path.lstat().st_mode) == 0o700, path
        for path in [store / 'snap1' / 'jerboa-snapshot.json', *(store / 'snap1' / 'vm.snapshot').iterdir()]:
            st = path.lstat()
            assert stat.S_ISREG(st.st_mode) and stat.S_IMODE(st.st_mode) == 0o600 and st.st_nlink == 1, path
        sidecar = (store / 'snap1' / 'jerboa-snapshot.json').read_text()
        assert '.sock' not in sidecar and str(tmp) not in sidecar
        record('private_store_permissions_and_sidecar_without_paths')

        # ---- create negatives and quotas ------------------------------------
        assert 'exists' in cli('snapshot', 'create', 'client', 'snap1', ok=False)
        assert 'invalid snapshot name' in cli('snapshot', 'create', 'client', '../escape', ok=False)
        slirp_port = free_port()
        cli('run', 'snapfix:latest', '--name', 'slirp', '-p', f'127.0.0.1:{slirp_port}:8080')
        assert 'UNSUPPORTED_CAPABILITY' in cli('snapshot', 'create', 'slirp', 'slirp-snap', ok=False)
        cli('stop', 'slirp')
        cli('rm', 'slirp')
        record('create_rejects_duplicate_invalid_name_and_slirp')
        cli('volume', 'create', 'snapvol', '--size', '64M')
        try:
            cli('run', 'snapfix:latest', '--name', 'withvol', '--network', 'snapnet', '--ip', '172.25.60.20', '-v', 'snapvol:/data')
        except RuntimeError as error:
            RESULTS['create_rejects_volume_vm'] = f'NOT RUN: volume VM did not start: {str(error)[:300]}'
        else:
            err = cli('snapshot', 'create', 'withvol', 'vol-snap', ok=False)
            assert 'UNSUPPORTED_CAPABILITY' in err and 'volumes' in err, err
            cli('stop', 'withvol')
            cli('rm', 'withvol')
            record('create_rejects_volume_vm')
        cli('snapshot', 'create', 'client', 'snap2')
        cli('snapshot', 'create', 'peer', 'snap3')
        cli('snapshot', 'create', 'client', 'tamper')
        assert 'quota' in cli('snapshot', 'create', 'client', 'snap5', ok=False)
        assert not staging_entries() and not journal_entries()
        cli('snapshot', 'rm', 'snap2')
        record('count_quota_without_leftovers')

        # ---- mutate after snapshot, stop, restore negatives -------------------
        assert get(url + '/set?v=after') == 'ok'
        assert get(url + '/disk?v=disk-after') == 'disk-after'
        established = socket.create_connection(('127.0.0.1', cp['tcp']), timeout=3)
        assert 'requires a stopped VM' in rpc_error('VM.SnapshotRestore', {'id': client, 'name': 'snap1'})
        cli('stop', 'client')
        wait_state('client', 'stopped')
        assert 'running VM is required' in cli('snapshot', 'create', 'client', 'late', ok=False)
        assert 'belongs to VM' in cli('snapshot', 'restore', 'client', 'snap3', ok=False)
        assert 'not found' in cli('snapshot', 'restore', 'client', 'missing', ok=False)
        assert_clean_stopped(client, 'foreign/missing')
        record('restore_rejects_running_foreign_missing')

        with (store / 'tamper' / 'vm.snapshot' / 'ram.bin').open('r+b') as f:
            f.seek(4096)
            byte = f.read(1)
            f.seek(4096)
            f.write(bytes([byte[0] ^ 0xFF]))
        assert 'integrity' in cli('snapshot', 'restore', 'client', 'tamper', ok=False)
        assert_clean_stopped(client, 'tampered hash')
        cli('snapshot', 'rm', 'tamper')
        os.chmod(store / 'snap1', 0o755)
        assert 'integrity' in cli('snapshot', 'restore', 'client', 'snap1', ok=False)
        os.chmod(store / 'snap1', 0o700)
        assert_clean_stopped(client, 'permissions')
        record('restore_rejects_tampered_hash_and_permissions')

        blocker = socket.socket()
        # Earlier HTTP probes leave TIME_WAIT entries; an active listener still
        # excludes the daemon's publication.
        blocker.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        blocker.bind(('127.0.0.1', cp['http']))
        blocker.listen(1)
        try:
            err = cli('snapshot', 'restore', 'client', 'snap1', ok=False)
            assert 'publish ports' in err, err
        finally:
            blocker.close()
        assert_clean_stopped(client, 'port collision')
        record('restore_port_collision_rolls_back_without_orphans')

        intruder = rpc('VM.Run', {'image': 'snapfix:latest', 'name': 'intruder', 'network_name': 'snapnet',
                                  'ip_address': '172.25.60.30', 'static_ip': True, 'network_aliases': ['svc-client']})['id']
        assert 'held by live VM' in rpc_error('VM.SnapshotRestore', {'id': client, 'name': 'snap1'})
        cli('stop', intruder)
        cli('rm', intruder)
        assert_clean_stopped(client, 'alias collision')
        record('restore_alias_collision_rejected')

        cli('network', 'create', 'ephemeral', '--subnet', '172.25.62.0/24')
        eph = rpc('VM.Run', {'image': 'snapfix:latest', 'name': 'eph', 'network_name': 'ephemeral',
                             'ip_address': '172.25.62.10', 'static_ip': True})['id']
        cli('snapshot', 'create', eph, 'esnap')
        cli('stop', eph)
        cli('network', 'rm', 'ephemeral')
        assert 'not registered' in rpc_error('VM.SnapshotRestore', {'id': eph, 'name': 'esnap'})
        cli('network', 'create', 'ephemeral', '--subnet', '172.25.63.0/24')
        assert 'changed subnet' in rpc_error('VM.SnapshotRestore', {'id': eph, 'name': 'esnap'})
        assert_clean_stopped(eph, 'network changed')
        cli('rm', eph)
        cli('network', 'rm', 'ephemeral')
        cli('snapshot', 'rm', 'esnap')
        record('restore_rejects_removed_or_changed_network')

        # ---- JSON-RPC restore -------------------------------------------------
        t0 = time.monotonic()
        restore_ok(client, 'snap1')
        RESULTS['restore_seconds'] = round(time.monotonic() - t0, 3)
        info = inspect('client')
        assert info['id'] == client and info['ip_address'] == '172.25.60.11' and info['state'] == 'running', info
        assert len(supervisors(client)) == 1, supervisors(client)
        wait(lambda: get(url + '/boot'), 'restored HTTP')
        assert get(url + '/boot') == boot1, 'same guest incarnation (no reboot)'
        assert get(url + '/get') == 'before', 'RAM state is the snapshot point'
        assert get(url + '/disk') == 'disk-before', 'root disk state is the snapshot point'
        record('rpc_restore_same_id_ip_no_reboot_ram_and_disk_state')
        with socket.socket(type=socket.SOCK_DGRAM) as s:
            s.settimeout(3)
            s.sendto(b'restored-udp', ('127.0.0.1', cp['udp']))
            assert s.recv(100) == b'restored-udp'
        with socket.create_connection(('127.0.0.1', cp['tcp']), timeout=3) as c:
            c.sendall(b'restored-tcp')
            c.shutdown(socket.SHUT_WR)
            reply = b''
            while chunk := c.recv(100):
                reply += chunk
            assert reply == b'final:restored-tcp', reply
        try:
            established.sendall(b'old-connection')
            established.shutdown(socket.SHUT_WR)
            established.settimeout(3)
            old = established.recv(100)
            RESULTS['established_tcp_across_stop_restore'] = 'reply' if old.startswith(b'final:') else f'closed ({old!r})'
        except OSError as error:
            RESULTS['established_tcp_across_stop_restore'] = f'broken ({error.__class__.__name__})'
        finally:
            established.close()
        assert get(url + '/lookup?name=peer') == '172.25.60.10'
        assert get(url + '/peer?target=peer-alias')
        for name in ('client', 'api.snapnet', 'svc-client'):
            assert get(peer_url + '/lookup?name=' + name) == '172.25.60.11', name
        assert get(peer_url + '/peer?target=svc-client') == boot1
        for path in ('/peer?target=172.25.61.10', '/lookup?name=other'):
            assert http_error_code(url + path) == 502, path
        assert rpc('DNS.Resolve', {'name': 'svc-client', 'network': 'snapnet'})['ip'] == '172.25.60.11'
        record('restored_http_udp_tcp_dns_aliases_and_cross_network_denial')
        wait(lambda: inspect('client').get('health') == 'healthy', 'restored health', 30)
        stats = json.loads(cli('stats', 'client', '--output', 'json'))
        assert stats['source'] != 'fallback' and stats['mem_bytes'] > 0, stats
        record('restored_health_and_stats', {'source': stats['source'], 'mem_bytes': stats['mem_bytes']})

        # ---- races ------------------------------------------------------------
        cli('stop', 'client')
        wait_state('client', 'stopped')
        outcomes = []

        def restore_worker(name='snap1'):
            try:
                outcomes.append(('ok', rpc('VM.SnapshotRestore', {'id': client, 'name': name})['id']))
            except (RPCError, OSError, ValueError) as error:
                outcomes.append(('err', str(error)))
        threads = [threading.Thread(target=restore_worker) for _ in range(3)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert len([o for o in outcomes if o[0] == 'ok']) == 1, outcomes
        assert len(supervisors(client)) == 1 and inspect('client')['state'] == 'running'
        record('concurrent_restores_exactly_one_wins', sorted(o[1][:70] for o in outcomes))

        create_out, stop_errors, stop_ok = [], [], []

        def create_worker():
            try:
                rpc('VM.SnapshotCreate', {'id': client, 'name': 'race'})
                create_out.append('ok')
            except (RPCError, OSError, ValueError) as error:
                create_out.append(str(error))
        worker = threading.Thread(target=create_worker)
        worker.start()
        while worker.is_alive():
            try:
                rpc('VM.Stop', {'id': client})
                stop_ok.append(True)
                break
            except RPCError as error:
                stop_errors.append(str(error))
            time.sleep(0.005)
        worker.join()
        # Every stop attempt may have been refused while the capture held the
        # claim; stop now so the following races start from a stopped VM.
        if inspect('client')['state'] == 'running':
            cli('stop', 'client')
        wait_state('client', 'stopped')
        assert not staging_entries() and not journal_entries()
        if create_out == ['ok']:
            assert 'race' in snapshot_names()
            cli('snapshot', 'rm', 'race')
        assert all('snapshot-create' in e for e in stop_errors), stop_errors
        record('stop_during_create_refused_while_capturing', {'create': create_out[0][:80],
                                                              'stop_busy_rejections': len(stop_errors), 'stop_succeeded_after': bool(stop_ok)})

        restore_ok(client, 'snap1')
        cli('snapshot', 'create', 'client', 'snaprm')
        cli('stop', 'client')
        wait_state('client', 'stopped')
        outcomes.clear()
        rm_errors, vm_rm_errors, removed_at = [], [], []
        worker = threading.Thread(target=restore_worker, args=('snaprm',))
        worker.start()
        # Wait until restore owns the snapshot. Before acquisition, removal is
        # allowed to win; that is not a test of deletion during restoration.
        wait(lambda: client + '.json' in journal_entries() or not worker.is_alive(),
             'snapshot restore reservation before deletion race')
        while worker.is_alive():
            try:
                rpc('Snapshot.Remove', {'name': 'snaprm'})
                removed_at.append(worker.is_alive())
                break
            except RPCError as error:
                rm_errors.append(str(error))
            try:
                rpc('VM.Remove', {'id': client})
                raise AssertionError('VM removed during restore')
            except RPCError as error:
                vm_rm_errors.append(str(error))
        worker.join()
        assert outcomes and outcomes[0][0] == 'ok', outcomes
        assert inspect('client')['state'] == 'running'
        assert all('in use' in e for e in rm_errors), rm_errors
        assert not removed_at or removed_at == [False] or 'snaprm' not in snapshot_names()
        if 'snaprm' in snapshot_names():
            cli('snapshot', 'rm', 'snaprm')
        record('snapshot_rm_and_vm_rm_during_restore_refused', {'snapshot_rm_in_use_rejections': len(rm_errors),
                                                                'vm_rm_rejections': sorted({e[:60] for e in vm_rm_errors})})
        # Kill during a restore is rejected by the lifecycle claim; probe it in
        # the restoring window and tolerate a kill that lands after completion.
        cli('stop', 'client')
        wait_state('client', 'stopped')
        outcomes.clear()
        kill_results = []
        worker = threading.Thread(target=restore_worker)
        worker.start()
        while worker.is_alive():
            try:
                if inspect('client')['state'] == 'restoring':
                    rpc('VM.Kill', {'id': client})
                    kill_results.append('ok')
                    break
            except RPCError as error:
                kill_results.append(str(error))
            except RuntimeError:
                pass
        worker.join()
        record('kill_during_restore', {'restore': outcomes[0][1][:60], 'kill': sorted({k[:60] for k in kill_results})})
        if inspect('client')['state'] != 'running':
            wait_state('client', 'stopped')
            restore_ok(client, 'snap1')
        wait(lambda: get(url + '/boot') == boot1, 'client ready after kill race')

        # ---- daemon SIGKILL recovery -----------------------------------------
        def kill_during(phase_names, action):
            observed = {}
            thread = threading.Thread(target=action)
            thread.start()
            path = store / '.operations' / f'{client}.json'
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline and thread.is_alive():
                try:
                    phase = json.loads(path.read_text())['phase']
                    if phase in phase_names:
                        observed['phase_killed'] = phase
                        break
                except (OSError, ValueError, KeyError):
                    pass
                time.sleep(0.001)
            kill_daemon()
            thread.join(30)
            start_daemon()
            observed['journal_after'] = journal_entries()
            observed['staging_after'] = staging_entries()
            return observed

        crash_results = {}
        for phases in ({'supervisor-started'}, {'loading'}, {'resuming'}):
            cli('stop', 'client')
            wait_state('client', 'stopped')
            outcomes.clear()
            observed = kill_during(phases, restore_worker)
            info = inspect('client')
            observed['state_after_recovery'] = info['state']
            assert not observed['journal_after'], observed
            if info['state'] == 'running':
                wait(lambda: get(url + '/boot') == boot1, 'adopted restored guest')
                assert len(supervisors(client)) == 1
            else:
                assert info['state'] == 'stopped', info
                assert_clean_stopped(client, 'crash during restore')
                restore_ok(client, 'snap1')
                wait(lambda: get(url + '/boot') == boot1, 'restore after rollback')
            crash_results['/'.join(sorted(phases))] = observed
        record('daemon_sigkill_during_restore_recovers', crash_results)

        created = []

        def create_crash_worker():
            try:
                rpc('VM.SnapshotCreate', {'id': client, 'name': 'crashcreate'})
                created.append('ok')
            except (RPCError, OSError, ValueError) as error:
                created.append(str(error))
        observed = kill_during({'capturing'}, create_crash_worker)
        assert inspect('client')['state'] == 'running'
        wait(lambda: get(url + '/boot') == boot1, 'guest resumed after interrupted capture', 60)
        observed['published'] = 'crashcreate' in snapshot_names()
        assert not observed['journal_after'] and not observed['staging_after'], observed
        if observed['published']:
            cli('snapshot', 'rm', 'crashcreate')
        record('daemon_sigkill_during_create_resumes_and_cleans_staging', observed)

        kill_daemon()
        start_daemon()
        assert inspect('client')['id'] == client and inspect('client')['state'] == 'running'
        wait(lambda: get(url + '/boot') == boot1, 'adopted after restart')
        wait(lambda: inspect('client').get('health') == 'healthy', 'adopted health', 30)
        assert get(url + '/lookup?name=peer') == '172.25.60.10'
        cli('stop', 'client')
        wait_state('client', 'stopped')
        wait(lambda: not supervisors(client), 'adopted supervisor exit', 45)
        record('restored_vm_adopted_after_daemon_sigkill_and_stoppable')

        # ---- start vs restore, then post-restore semantics --------------------
        outcomes.clear()
        start_results = []

        def start_worker():
            try:
                start_results.append(('ok', rpc('VM.Start', {'id': client})['id']))
            except (RPCError, OSError, ValueError) as error:
                start_results.append(('err', str(error)))
        threads = [threading.Thread(target=restore_worker), threading.Thread(target=start_worker)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert len([o for o in outcomes + start_results if o[0] == 'ok']) == 1, (outcomes, start_results)
        running = [v for v in json.loads(cli('ps', '--output', 'json')) if v.get('name') == 'client' and v['state'] == 'running']
        assert len(running) == 1, running
        record('start_vs_restore_exactly_one_incarnation', {'restore': outcomes[0][1][:60], 'start': start_results[0][1][:60]})
        if start_results[0][0] != 'ok':
            cli('stop', 'client')
            wait_state('client', 'stopped')
            fresh = rpc('VM.Start', {'id': client})['id']
        else:
            fresh = start_results[0][1]
        assert fresh != client, 'VM.Start on a stopped VM is a normal boot with a replacement ID'
        wait(lambda: get(url + '/boot') != boot1, 'normal boot')
        assert get(url + '/get') == ''
        cli('stop', fresh)
        wait_state(fresh, 'stopped')
        assert 'belongs to VM' in cli('snapshot', 'restore', fresh, 'snap1', ok=False)
        record('start_is_normal_boot_and_snapshot_not_retargetable')

        kill_daemon()
        start_daemon(['--snapshot-max-bytes', str(1 << 20)])
        rpc('VM.Start', {'id': fresh})
        wait(lambda: get(url + '/boot'), 'client for byte quota')
        assert 'quota' in cli('snapshot', 'create', 'client', 'toolarge', ok=False)
        assert get(url + '/boot'), 'byte quota is checked before pausing'
        assert not staging_entries() and not journal_entries()
        record('byte_quota_rejects_before_pausing')

        for name in ('client', 'peer', 'other'):
            cli('stop', name)
            cli('rm', name)
        for name in sorted(snapshot_names()):
            cli('snapshot', 'rm', name)
        assert snapshot_names() == set()
        state['daemon'].terminate()
        state['daemon'].wait(60)
        wait(lambda: not my_processes(), 'all VMM processes gone', 30)
        record('teardown_no_orphan_processes')
        RESULTS['result'] = 'PASS'
    except BaseException as error:
        RESULTS['result'] = f'FAIL: {error.__class__.__name__}: {error}'
        print(log_path.read_text()[-8000:], flush=True)
        raise
    finally:
        daemon = state['daemon']
        if daemon and daemon.poll() is None:
            daemon.terminate()
            try:
                daemon.wait(60)
            except subprocess.TimeoutExpired:
                daemon.kill()
        leaked = my_processes()
        if leaked:
            RESULTS['leaked_processes_killed'] = [p[1][:160] for p in leaked]
        for pid, _ in leaked:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        log.close()
        shutil.copy2(log_path, artifacts / 'daemon.log')
        (artifacts / 'result.json').write_text(json.dumps(RESULTS, indent=2, sort_keys=True) + '\n')
        print('artifacts:', artifacts, flush=True)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == '__main__':
    main()
