#!/usr/bin/env python3
"""Record the on-disk state a released Jerboa leaves behind, as a test fixture.

Downloads the jerboa and jerboad of one released version, checks them against
that release's signed manifest, drives them through a fixed scenario (images,
tags, a signature, volumes, networks, VMs in every resting state, a compose
project, a registry login, and on macOS a snapshot), stops the daemon the way
an upgrade does and packs what is left of ~/.jerboa and the compose project.
internal/upgradetest restores that state and checks that the current code
still opens all of it (see its package documentation).

The state holds absolute paths, and a compose project is identified by a hash
of its path, so the scenario always runs under one fixed directory (ROOT
below) and the tests restore it there. Nothing outside that directory is
written, and the daemon it starts is the only process it stops.

Usage (as root on Linux x86_64, or as yourself on macOS 26+ Apple Silicon):

    python3 scripts/upgrade/make-fixture.py --version v0.59.0 \
        --out internal/upgradetest/testdata

Linux needs qemu-system-x86_64, go, zstd, curl, and minisign or the Go
toolchain of this checkout; scripts/upgrade/make-fixture-docker.sh provides
all of it in a container. macOS has no published kernel toolset, so --bin-dir
names the bin directory of that release's Jerboa Desktop app
(Contents/Resources/bin: jerboa, jerboad, firecracker, tools/), whose binaries
are then used instead of the downloads.
"""
import argparse
import hashlib
import http.server
import json
import os
import platform
import re
import secrets
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import threading
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
BASE = 'https://releases.jerboa.dev'
LINUX = sys.platform.startswith('linux')
# Not below os.TempDir(): the CLI treats files there as shared with other
# users. /var/tmp is /private/var/tmp on macOS, and compose hashes the
# resolved path.
ROOT = Path('/var/tmp/jerboa-upgrade-fixture' if LINUX else '/private/var/tmp/jerboa-upgrade-fixture')
PORT, SQLITE_PORT, REGISTRY_PORT = 47891, 47892, 47893
REGISTRY = f'127.0.0.1:{REGISTRY_PORT}'
REGISTRY_TOKEN = 'jrb_pat_upgrade_fixture_not_a_real_token'

PROBE = r'''// Guest of the upgrade fixtures: records each boot on its volume, prints
// what the volume holds, then stays up (or exits when asked to).
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	const log = "/data/boots.txt"
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = fmt.Fprintf(f, "boot %s\n", os.Getenv("PROBE_MARK"))
		_ = f.Sync()
		_ = f.Close()
	}
	data, err := os.ReadFile(log)
	fmt.Printf("probe: mark=%s volume=%q err=%v\n", os.Getenv("PROBE_MARK"), data, err)
	if os.Getenv("PROBE_EXIT") != "" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}
'''

COMPOSE = '''version: "1"
services:
  api:
    image: probe:latest
    memory: 128M
    networks: [backend]
    volumes: ["state:/data"]
    environment: ["PROBE_MARK=compose"]
networks:
  backend:
    driver: bridge
volumes:
  state:
    size: 16M
'''


def log(message):
    print(f'==> {message}', flush=True)


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        while block := f.read(1 << 20):
            h.update(block)
    return h.hexdigest()


def fetch(url, path):
    subprocess.run(['curl', '--proto', '=https', '--fail', '--silent', '--show-error', '--location',
                    '--retry', '3', '--output', str(path), url], check=True)


def verify_signature(manifest):
    """Check the manifest's detached signature against the embedded release key."""
    if shutil.which('minisign'):
        key = re.search(r'PublicKeyB64 = "([^"]+)"', (REPO / 'internal/release/config.go').read_text()).group(1)
        subprocess.run(['minisign', '-Vm', str(manifest), '-x', f'{manifest}.minisig', '-P', key],
                       check=True, stdout=subprocess.DEVNULL)
    else:
        subprocess.run(['go', 'run', './cmd/jerboa-verify', str(manifest)], check=True, cwd=REPO)


def download_release(version, dest):
    """Download jerboa and jerboad of a release and check them against its signed manifest."""
    dest.mkdir(parents=True)
    manifest = dest / 'manifest.json'
    fetch(f'{BASE}/releases/{version}/manifest.json', manifest)
    fetch(f'{BASE}/releases/{version}/manifest.json.minisig', f'{manifest}.minisig')
    verify_signature(manifest)
    components = json.loads(manifest.read_text())['components']
    target = 'linux-amd64' if LINUX else 'darwin-arm64'
    digests = {}
    for component, binary in (('cli', 'jerboa'), ('daemon', 'jerboad')):
        if components[component]['version'] != version:
            raise SystemExit(f'the signed manifest does not list {component} {version}')
        asset = components[component]['platforms'][target]
        fetch(asset['url'], dest / binary)
        digests[binary] = sha256(dest / binary)
        if digests[binary] != asset['sha256']:
            raise SystemExit(f'{binary} {version} does not match its signed checksum')
        (dest / binary).chmod(0o755)
        if version not in subprocess.run([str(dest / binary), '--version'], capture_output=True, text=True).stdout.split():
            raise SystemExit(f'{binary} {version} does not run on this host')
    return digests


class Registry(http.server.BaseHTTPRequestHandler):
    """The least a registry must answer for `jerboa login --token`."""

    def do_GET(self):  # noqa: N802 - http.server's naming
        body = json.dumps({'api': 1, 'cdn': f'http://{REGISTRY}'}).encode()
        self.send_response(200 if self.path == '/v2/' else 404)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


class Old:
    """One released jerboad over a home directory, and its CLI."""

    def __init__(self, bindir, home, port, args):
        self.bindir, self.home, self.port, self.args = bindir, home, port, args
        self.endpoint = f'tcp://127.0.0.1:{port}' if LINUX else 'unix://' + str(home / '.jerboa' / 'jerboad.sock')
        self.token = ''
        self.daemon = None
        self.env = {k: v for k, v in os.environ.items() if not k.startswith('JERBOA_')}
        self.env.update(HOME=str(home), JERBOA_HOST=self.endpoint)

    def start(self, *extra):
        daemon_env = dict(self.env)
        if self.token:
            daemon_env['JERBOA_AUTH_TOKEN'] = self.token
        out = open(ROOT / f'daemon-{self.port}.log', 'ab')
        command = [str(self.bindir / 'jerboad'), '--host', self.endpoint, *self.args, *extra]
        self.daemon = subprocess.Popen(command, env=daemon_env, stdout=out, stderr=out, cwd=self.home)
        for _ in range(150):
            if self.daemon.poll() is not None:
                raise SystemExit('jerboad exited at startup:\n' + Path(out.name).read_text()[-4000:])
            if subprocess.run([str(self.bindir / 'jerboa'), 'ps'], env=self.env, capture_output=True).returncode == 0:
                return
            time.sleep(0.2)
        raise SystemExit('jerboad did not answer')

    def stop(self):
        """Stop the daemon as a service manager does before an upgrade: SIGTERM, then wait."""
        self.daemon.send_signal(signal.SIGTERM)
        self.daemon.wait(timeout=60)
        self.daemon = None
        # The daemon stops its VMs on the way out; nothing of this run may stay.
        for _ in range(50):
            if not self.vmm_processes():
                return
            time.sleep(0.2)
        raise SystemExit(f'VM processes survived the daemon: {self.vmm_processes()}')

    def vmm_processes(self):
        out = subprocess.run(['pgrep', '-f', str(self.home / '.jerboa')], capture_output=True, text=True).stdout
        return [int(pid) for pid in out.split() if int(pid) != os.getpid()]

    def run(self, *args, check=True, cwd=None):
        result = subprocess.run([str(self.bindir / 'jerboa'), *args], env=self.env, capture_output=True, text=True, cwd=cwd)
        if check and result.returncode != 0:
            raise SystemExit(f'jerboa {" ".join(args)} failed:\n{result.stdout}{result.stderr}')
        return result.stdout

    def has(self, *command, flag=None):
        """Whether this release has the command (and the flag): older ones lack some."""
        result = subprocess.run([str(self.bindir / 'jerboa'), *command, '--help'], env=self.env, capture_output=True, text=True)
        return result.returncode == 0 and 'unknown command' not in result.stderr and (flag is None or flag in result.stdout)

    def json(self, *args):
        return json.loads(self.run(*args, '--output', 'json'))

    def vm(self, name):
        return json.loads(self.run('inspect', name))

    def wait_state(self, name, state, timeout=120):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if self.vm(name)['state'] == state:
                return
            time.sleep(0.3)
        raise SystemExit(f'VM {name} did not reach {state}: {self.vm(name)}')

    def wait_log(self, name, text, timeout=180):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if text in self.run('logs', name, check=False):
                return
            time.sleep(0.5)
        raise SystemExit(f'VM {name} never printed {text!r}:\n{self.run("logs", name, check=False)}')

    def build(self, source, name, *extra):
        out = self.run('build', str(source), '--name', name, '--memory', '128M', *extra)
        return re.search(r'sha256:[0-9a-f]{64}', out).group(0)


def write_probe(directory):
    directory.mkdir(parents=True)
    (directory / 'main.go').write_text(PROBE)
    (directory / 'go.mod').write_text('module probe\n\ngo 1.22\n')
    (directory / 'unikernel.toml').write_text('[build]\nlang = "go"\ndirs = ["/data"]\n')


def main_scenario(old, work, snapshot):
    """Everything a user of the default (file) VM store can leave behind."""
    layout = ['--layout', 'standard'] if LINUX and not os.path.exists('/dev/kvm') and old.has('build', flag='--layout') else []
    probe = work / 'probe'
    write_probe(probe)
    expected = {}

    # Settings, as the installer of this platform leaves them.
    config = old.home / '.jerboa' / 'config.toml'
    if LINUX:
        old.token = secrets.token_hex(32)
        config.parent.mkdir(mode=0o755)
        # scripts/install.sh writes exactly this file for the invoking user.
        config.write_text(f'hypervisor = "{old.hypervisor}"\n\n[daemon]\nendpoint = "{old.endpoint}"\ntoken = "{old.token}"\n')
        config.chmod(0o600)
        del old.env['JERBOA_HOST']  # the CLI finds the daemon through the config, as after an install
    old.start()
    if not LINUX:
        old.run('config', 'set', 'hypervisor', old.hypervisor)
    expected['config'] = {'hypervisor': old.run('config', 'get', 'hypervisor').strip(),
                          'endpoint': old.endpoint if LINUX else '', 'token': old.token}

    # Images: a tagged one, a second name for it, a signature, an image a
    # rebuild replaced that a VM still boots from, and one nothing refers to.
    (probe / 'main.go').write_text(PROBE.replace('probe: mark', 'probe1: mark'))
    first = old.build(probe, 'legacy', *layout)
    old.run('run', 'legacy:latest', '--name', 'veteran', '-e', 'PROBE_EXIT=1', '-e', 'PROBE_MARK=veteran')
    old.wait_state('veteran', 'stopped')
    (probe / 'main.go').write_text(PROBE.replace('probe: mark', 'probe2: mark'))
    orphan = old.build(probe, 'legacy', *layout)
    (probe / 'main.go').write_text(PROBE.replace('probe: mark', 'probe3: mark'))
    old.build(probe, 'legacy', *layout)
    (probe / 'main.go').write_text(PROBE)
    digest = old.build(probe, 'probe', *layout)
    # (Releases before the registry have no `tag`: the second name is a build.)
    if old.has('tag'):
        old.run('tag', 'probe:latest', 'probe:v1')
    else:
        old.build(probe, 'probe', '--tag', 'v1', *layout)
    expected['signed'] = old.has('sign')
    if expected['signed']:
        old.run('sign', 'probe:latest')
        old.run('verify', 'probe:latest')
    expected['replaced_images'] = {'in_use': first, 'unused': orphan}
    expected['image_dirs'] = sorted(p.name for p in (old.home / '.jerboa' / 'images').iterdir() if p.is_dir())

    # Volumes and networks, used and unused.
    old.run('volume', 'create', 'data', '--size', '16M')
    old.run('volume', 'create', 'spare', '--size', '8M')
    old.run('network', 'create', 'appnet', '--subnet', '10.77.0.0/24')
    old.run('network', 'create', 'idle', '--subnet', '10.78.0.0/24')

    # VMs in every state an upgrade finds them in: running, stopped by the
    # user, exited on their own, and (veteran, above) on a replaced image.
    old.run('run', 'probe:latest', '--name', 'web', '-v', 'data:/data', '--network', 'appnet', '--ip', '10.77.0.10',
            '-p', '127.0.0.1:47894:8080', '-e', 'PROBE_MARK=web', '-e', 'API_KEY=fixture-secret-value',
            '--restart', 'on-failure', '--health-check', 'tcp:8080')
    old.wait_log('web', 'probe: mark=web volume="boot web\\n"')
    old.run('run', 'probe:latest', '--name', 'halted', '--network', 'appnet', '-e', 'PROBE_MARK=halted', '--cpus', '2')
    old.wait_log('halted', 'probe: mark=halted')
    old.run('stop', 'halted')
    old.wait_state('halted', 'stopped')
    old.run('run', 'probe:v1', '--name', 'finished', '-e', 'PROBE_EXIT=1', '-e', 'PROBE_MARK=finished')
    old.wait_state('finished', 'stopped')

    # A compose project, left up.
    project = ROOT / 'project'
    project.mkdir()
    (project / 'compose.yaml').write_text(COMPOSE)
    old.run('compose', 'up', 'compose.yaml', cwd=project)
    old.wait_log('api', 'probe: mark=compose volume="boot compose\\n"')
    expected['compose'] = {'file': str(project / 'compose.yaml'),
                           'ps': json.loads(old.run('compose', 'ps', 'compose.yaml', '--output', 'json', cwd=project))}
    # Up to 0.59 the state sits next to the compose file; from 0.60 in
    # ~/.jerboa/compose.
    state_files = sorted(project.glob('.jerboa-compose-*.json')) or sorted((old.home / '.jerboa' / 'compose').glob('*.json'))
    expected['compose']['state_file'] = str(state_files[0])
    expected['compose']['state'] = json.loads(state_files[0].read_text())

    # A registry login and the package index cache.
    expected['credentials'] = {}
    if old.has('login', flag='--token'):
        server = http.server.ThreadingHTTPServer(('127.0.0.1', REGISTRY_PORT), Registry)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        old.run('login', REGISTRY, '--token', REGISTRY_TOKEN)
        server.shutdown()
        expected['credentials'] = {REGISTRY: REGISTRY_TOKEN}
    old.run('pkg', 'search', 'redis', check=False)
    old.run('version', check=False)

    if snapshot:
        # Snapshots are of a running VM on a named network, without volumes.
        old.run('run', 'probe:latest', '--name', 'snapshotted', '--network', 'appnet', '-e', 'PROBE_MARK=snapshotted')
        old.wait_log('snapshotted', 'probe: mark=snapshotted')
        old.run('snapshot', 'create', 'snapshotted', 'before-upgrade')
        expected['snapshots'] = old.json('snapshot', 'ls')

    expected['images'] = old.json('images')
    expected['volumes'] = old.json('volume', 'ls')
    expected['networks'] = old.json('network', 'ls')
    expected['vms'] = {vm['name']: old.vm(vm['id']) for vm in old.json('ps')}
    expected['image_digest'] = digest
    old.stop()
    return expected


def sqlite_scenario(old, work):
    """A daemon that was moved to --vm-store sqlite and used there."""
    layout = ['--layout', 'standard'] if LINUX and not os.path.exists('/dev/kvm') and old.has('build', flag='--layout') else []
    probe = work / 'probe-sqlite'
    write_probe(probe)
    if LINUX:
        # A TCP endpoint is refused without a token; this home has no config
        # file, so the CLI takes both from its environment.
        old.token = old.env['JERBOA_AUTH_TOKEN'] = secrets.token_hex(32)
    old.start()
    old.build(probe, 'probe', *layout)
    for name in ('kept', 'removed'):
        old.run('run', 'probe:latest', '--name', name, '-e', 'PROBE_EXIT=1', '-e', f'PROBE_MARK={name}')
        old.wait_state(name, 'stopped')
    removed = old.vm('removed')['id']
    old.stop()
    # The released daemon migrates the file registry into vms.db at this start.
    old.start('--vm-store', 'sqlite')
    old.run('rm', 'removed')
    old.run('run', 'probe:latest', '--name', 'late', '-e', 'PROBE_MARK=late', '-e', 'DB_PASSWORD=fixture-secret-value')
    old.wait_log('late', 'probe: mark=late')
    expected = {'vms': {vm['name']: old.vm(vm['id']) for vm in old.json('ps')},
                # Removed under sqlite, but its state.json is still in the
                # file registry: the released daemon migrated it again at
                # every start.
                'removed_but_left_on_disk': removed}
    old.stop()
    return expected


def pack(out, version, meta):
    """Write state.tar.zst (the state, bytes and modes as left) and fixture.json."""
    name = f'{version}-{"linux-amd64" if LINUX else "darwin-arm64"}'
    target = out / name
    target.mkdir(parents=True, exist_ok=True)
    files = []
    tar_path = ROOT.parent / f'{ROOT.name}.tar'

    def keep(path):
        rel = path.relative_to(ROOT)
        mode = path.lstat().st_mode
        if stat.S_ISSOCK(mode):
            return False
        if rel.parts[0] == 'project' or rel.parts in (('home',), ('home-sqlite',)):
            return True
        # Of a home only Jerboa's own directory: not the build caches of the
        # language toolchains beside it.
        if rel.parts[0] not in ('home', 'home-sqlite') or rel.parts[1] != '.jerboa':
            return False
        # The kernel toolset cache is downloaded again when it is missing;
        # what version it held is still state.
        if rel.parts[2:3] == ('tools',) and path.is_file() and path.name != 'kernel-version.txt':
            return False
        return True

    with tarfile.open(tar_path, 'w', format=tarfile.PAX_FORMAT) as tar:
        for path in sorted(ROOT.rglob('*')):
            if not keep(path):
                continue
            rel = path.relative_to(ROOT).as_posix()
            info = tar.gettarinfo(str(path), arcname=rel)
            info.uid = info.gid = 0
            info.uname = info.gname = ''
            entry = {'path': rel, 'mode': oct(stat.S_IMODE(info.mode))}
            if info.isreg():
                entry.update(size=info.size, sha256=sha256(path))
                with open(path, 'rb') as f:
                    tar.addfile(info, f)
            elif info.isdir():
                entry['dir'] = True
                tar.addfile(info)
            else:
                # Directories and regular files only: the tests restore
                # nothing else.
                raise SystemExit(f'unexpected file type in the state: {path}')
            files.append(entry)
    archive = target / 'state.tar.zst'
    # A long window finds the image again in every VM boot disk cloned from it.
    subprocess.run(['zstd', '-19', '--long=27', '-T0', '-q', '-f', str(tar_path), '-o', str(archive)], check=True)
    tar_path.unlink()
    meta.update(archive_sha256=sha256(archive), files=files)
    (target / 'fixture.json').write_text(json.dumps(meta, indent=1, sort_keys=True) + '\n')
    log(f'wrote {archive} ({archive.stat().st_size >> 10} KiB, {len(files)} entries)')


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--version', required=True, help='released version to record, e.g. v0.59.0')
    parser.add_argument('--out', required=True, type=Path, help='fixture directory (internal/upgradetest/testdata)')
    parser.add_argument('--bin-dir', type=Path, help='use the jerboa and jerboad of this directory (macOS: the app bundle)')
    parser.add_argument('--hypervisor', help='qemu or firecracker (default: qemu on Linux, firecracker on macOS)')
    args = parser.parse_args()
    if not re.fullmatch(r'v\d+\.\d+\.\d+', args.version):
        raise SystemExit('--version must be vMAJOR.MINOR.PATCH')
    # A Linux host of another architecture can still run the x86_64 release
    # through binfmt emulation (Docker Desktop does); the binaries decide.
    if not LINUX and (sys.platform != 'darwin' or platform.machine() != 'arm64'):
        raise SystemExit('fixtures are recorded on Linux or macOS Apple Silicon')
    if LINUX and os.geteuid() != 0:
        raise SystemExit('run as root: the daemon creates bridges and TAP devices')
    if not LINUX and not args.bin_dir:
        raise SystemExit('macOS needs --bin-dir: the bin directory of that release\'s Jerboa Desktop app')
    if ROOT.exists():
        raise SystemExit(f'{ROOT} exists: remove it (another run may be using it)')
    hypervisor = args.hypervisor or ('qemu' if LINUX else 'firecracker')
    out = args.out.resolve()
    ROOT.mkdir(mode=0o700)
    daemons = []
    try:
        work = ROOT / 'work'
        work.mkdir()
        digests = download_release(args.version, work / 'release')
        bindir = work / 'release'
        daemon_args = ['--hypervisor', hypervisor]
        if args.bin_dir:
            bindir = args.bin_dir.resolve()
            for binary in ('jerboa', 'jerboad'):
                if args.version not in subprocess.run([str(bindir / binary), '--version'], capture_output=True, text=True).stdout.split():
                    raise SystemExit(f'{bindir / binary} is not {args.version}')
                # The app's binaries are built with the app; record which.
                digests[binary] = sha256(bindir / binary)
            # The app's VMM is bin/spark; releases before the rename ship bin/firecracker.
            # Their daemons only know --fc-bin and the firecracker value, which later
            # daemons still accept.
            vmm = bindir / 'spark' if (bindir / 'spark').exists() else bindir / 'firecracker'
            daemon_args += ['--tools-dir', str(bindir / 'tools'), '--fc-bin', str(vmm)]
        for home in ('home', 'home-sqlite'):
            (ROOT / home).mkdir()
        main_daemon = Old(bindir, ROOT / 'home', PORT, daemon_args)
        sqlite_daemon = Old(bindir, ROOT / 'home-sqlite', SQLITE_PORT, daemon_args)
        main_daemon.hypervisor = sqlite_daemon.hypervisor = hypervisor
        daemons = [main_daemon, sqlite_daemon]
        log(f'recording {args.version} with {hypervisor} under {ROOT}')
        expected = main_scenario(main_daemon, work, snapshot=not LINUX and hypervisor == 'firecracker')
        expected['sqlite'] = sqlite_scenario(sqlite_daemon, work)
        pack(out, args.version, {
            'version': args.version, 'os': 'linux' if LINUX else 'darwin', 'arch': 'amd64' if LINUX else 'arm64',
            'root': str(ROOT), 'hypervisor': hypervisor, 'kvm': os.path.exists('/dev/kvm'),
            'recorded': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
            'binaries_sha256': digests, 'expected': expected})
    finally:
        for daemon in daemons:
            if daemon.daemon is not None:
                daemon.daemon.kill()
                daemon.daemon.wait()
            for pid in daemon.vmm_processes():
                os.kill(pid, signal.SIGKILL)
        shutil.rmtree(ROOT, ignore_errors=True)


if __name__ == '__main__':
    main()
