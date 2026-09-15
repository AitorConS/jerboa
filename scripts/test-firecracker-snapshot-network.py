#!/usr/bin/env python3
"""Real HVF unix-stream snapshot/restore against a recreated Jerboa switch.

The input seed is a Firecracker snapshot of the shared-network smoke guest
(HTTP 8080, UDP 8081, raw TCP 8082, /lookup and /peer). This harness owns the
switch lifecycle; it does not claim that jerboad currently exposes snapshot RPCs.
"""
import argparse,copy,http.client,json,os,shutil,socket,subprocess,tempfile,time,urllib.error,urllib.request
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1]

def port():
    with socket.socket() as sock:sock.bind(('127.0.0.1',0));return sock.getsockname()[1]
def api(sock,method,path,body=None):
    class UnixHTTP(http.client.HTTPConnection):
        def connect(self):
            self.sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);self.sock.settimeout(35);self.sock.connect(str(sock))
    conn=UnixHTTP('localhost')
    try:
        conn.request(method,path,None if body is None else json.dumps(body),{'Content-Type':'application/json'})
        response=conn.getresponse();data=response.read();return response.status,json.loads(data) if data else None
    finally:conn.close()
def wait(check,label,timeout=35):
    deadline=time.monotonic()+timeout;last=''
    while time.monotonic()<deadline:
        try:
            value=check()
            if value:return value
        except Exception as error:last=str(error)
        time.sleep(.05)
    raise RuntimeError(f'timeout {label}: {last}')
def state(sock,want):return wait(lambda:(lambda value:value if value['state']==want else None)(api(sock,'GET','/')[1]),want)
def get(url):
    with urllib.request.urlopen(url,timeout=4) as response:return json.load(response)

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--firecracker-bin',type=Path,required=True)
    parser.add_argument('--seed-snapshot',type=Path,required=True)
    parser.add_argument('--output-dir',type=Path,required=True)
    parser.add_argument('--go-bin',default='go')
    args=parser.parse_args()
    firecracker=args.firecracker_bin.resolve();seed=args.seed_snapshot.resolve();output=args.output_dir.resolve()
    output.mkdir(parents=True,exist_ok=True);output.chmod(0o700)
    artifacts=Path(tempfile.mkdtemp(prefix='run-',dir=output));artifacts.chmod(0o700)
    temporary=tempfile.TemporaryDirectory(prefix='jbs-',dir='/tmp');run=Path(temporary.name)
    helper=run/'snapshot-switch'
    subprocess.run([args.go_bin,'build','-o',str(helper),'./tests/helpers/snapshot-switch'],cwd=ROOT,check=True)
    vm_sock,link_sock,snapshot=run/'api.sock',run/'link.sock',run/'snapshot'
    manifest=json.loads((seed/'manifest.json').read_text());cfg=manifest['config'];net=cfg['network-interfaces'][0]
    cfg['boot-source']['kernel_image_path']=str(seed/'kernel')
    cfg['drives'][0]['path_on_host']=str(run/'root.img');shutil.copy2(seed/'disk-0.bin',run/'root.img')
    for index,key in enumerate(sorted(cfg['firmware'])):cfg['firmware'][key]=str(seed/f'firmware-{index}')
    net['socket_path']=str(link_sock);cfg['security']={'version':1,'unix_stream':str(link_sock)}
    config=run/'config.json';config.write_text(json.dumps(cfg));config.chmod(0o600)
    http_port,udp_port,tcp_port=port(),port(),port()
    def start_switch():
        log=(run/'switch.log').open('a')
        process=subprocess.Popen([str(helper),str(link_sock),net['guest_mac'],str(http_port),str(udp_port),str(tcp_port)],stdout=log,stderr=log)
        wait(lambda:link_sock.exists(),'switch');return process,log
    switch,switch_log=start_switch();vmm_log=(run/'vmm.log').open('w')
    vmm=subprocess.Popen([str(firecracker),'--api-sock',str(vm_sock),'--config-file',str(config)],stdout=vmm_log,stderr=vmm_log)
    url=f'http://127.0.0.1:{http_port}'
    def reject_load(label):
        code,body=api(vm_sock,'PUT','/snapshot/load',{'snapshot_path':str(snapshot)})
        assert code==400,(label,code,body);return body['fault_message']
    try:
        wait(lambda:vm_sock.exists(),'API');state(vm_sock,'Running');wait(lambda:get(url),'initial HTTP')
        assert api(vm_sock,'PUT','/actions',{'action_type':'Pause'})[0]==202;state(vm_sock,'Paused')
        code,operation=api(vm_sock,'PUT','/snapshot/create',{'snapshot_path':str(snapshot)});assert code==202,(code,operation)
        state(vm_sock,'Paused');result=api(vm_sock,'GET',f"/operations/{operation['operation_id']}")[1];assert result['status']=='succeeded',result
        created=json.loads((snapshot/'manifest.json').read_text())
        empty=[name for name,value in created['components'].items() if name.startswith('firmware-') and value['size']==0]
        assert empty,'fixture must exercise valid empty firmware restoration'
        assert api(vm_sock,'PUT','/actions',{'action_type':'ForceStop'})[0]==202;state(vm_sock,'Exited')
        switch.terminate();switch.wait(timeout=5);switch_log.close();wait(lambda:not link_sock.exists(),'old switch close')
        switch,switch_log=start_switch()

        original_network=copy.deepcopy(cfg['network-interfaces']);original_security=copy.deepcopy(cfg['security'])
        wrong=copy.deepcopy(original_network);wrong[0]['guest_mac']='02:00:00:00:00:fe'
        assert api(vm_sock,'PUT','/network-interfaces',wrong)[0]==204
        assert 'incompatible' in reject_load('MAC')

        assert api(vm_sock,'PUT','/network-interfaces',original_network)[0]==204
        bad_policy={**original_security,'egress':[{'protocol':'tcp','address':'192.0.2.10','port':443}]}
        assert api(vm_sock,'PUT','/security',bad_policy)[0]==204
        assert 'IP policy' in reject_load('policy')

        relative=copy.deepcopy(original_network);relative[0]['socket_path']='relative.sock'
        assert api(vm_sock,'PUT','/network-interfaces',relative)[0]==204
        assert api(vm_sock,'PUT','/security',{'version':1,'unix_stream':'relative.sock'})[0]==204
        assert 'invalid Unix stream socket path' in reject_load('relative socket')

        mismatched=copy.deepcopy(original_network);mismatched[0]['socket_path']=str(run/'other.sock')
        assert api(vm_sock,'PUT','/network-interfaces',mismatched)[0]==204
        assert api(vm_sock,'PUT','/security',original_security)[0]==204
        assert 'matching security.unix_stream' in reject_load('socket capability')

        assert api(vm_sock,'PUT','/network-interfaces',original_network)[0]==204
        assert api(vm_sock,'PUT','/security',original_security)[0]==204
        code,operation=api(vm_sock,'PUT','/snapshot/load',{'snapshot_path':str(snapshot)});assert code==202,(code,operation)
        state(vm_sock,'Paused');assert api(vm_sock,'PUT','/actions',{'action_type':'Resume'})[0]==202;state(vm_sock,'Running')
        wait(lambda:get(url),'restored HTTP');assert get(url+'/lookup?name=peer')=='172.25.40.11'
        wait(lambda:get(url+'/peer?target=peer'),'restored DNS/TCP')
        with socket.socket(type=socket.SOCK_DGRAM) as sock:
            sock.settimeout(3);sock.sendto(b'snapshot-udp',('127.0.0.1',udp_port));assert sock.recv(100)==b'snapshot-udp'
        with socket.create_connection(('127.0.0.1',tcp_port),timeout=3) as conn:
            conn.sendall(b'snapshot-tcp');conn.shutdown(socket.SHUT_WR);assert conn.recv(100)==b'final:snapshot-tcp'
        for path in ('/peer?target=172.25.41.10','/lookup?name=other'):
            try:get(url+path)
            except urllib.error.HTTPError as error:assert error.code==502
            else:raise AssertionError('cross-network access unexpectedly succeeded')
        report={'snapshot_restore':True,'http':True,'dns':True,'tcp':True,'udp':True,
                'rejected':['mac','policy','relative_socket','mismatched_socket_capability'],
                'empty_firmware':empty,'cross_network_denied':True,'established_tcp_preserved':'not promised'}
        (artifacts/'result.json').write_text(json.dumps(report,indent=2,sort_keys=True)+'\n');print(json.dumps(report,sort_keys=True))
    finally:
        if vmm.poll() is None:vmm.terminate();vmm.wait(timeout=5)
        vmm_log.close()
        if switch.poll() is None:switch.terminate();switch.wait(timeout=5)
        switch_log.close()
        for name in ('config.json','vmm.log','switch.log'):
            if (run/name).is_file():shutil.copy2(run/name,artifacts/name)
        temporary.cleanup()
if __name__=='__main__':main()
