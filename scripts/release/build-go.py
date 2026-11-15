#!/usr/bin/env python3
"""Build a Go program from its upstream source (scripts/build-go.sh).

The recipe in scripts/release/packages.json for PACKAGE_NAME and
PACKAGE_VERSION has a "go" object:

  main      the package to build, relative to dir (default ".")
  dir       the module directory to build in, relative to the source root
  ldflags   linker flags; {version} is the upstream version
  tags      build tags, comma-separated
  cgo       true to build with cgo (default false)
  env       more environment variables for the build, e.g. GOEXPERIMENT
  assets    files to add to the source before building, each
            {"url", "sha256", "dest", "strip"}: a .tar.gz, .tgz or .tar.xz
            archive extracted into dest, less strip leading path parts
  node      true when the prepare commands build a web UI with Node.js
  prepare   shell commands run in the source root before building, with
            VERSION set to the upstream version

"upstream_version" names the upstream release when the package version adds
a revision (3.15.1-1 for a rebuild of 3.15.1), and "source_strip" how many
leading path parts to drop from the source archive (default 1, its top
directory). The workflow has already
checked SOURCE_URL against the recipe's sha256.

Before building, every module in go-floor.txt that the program requires at an
older version is raised to that version. The program is built with the Go
toolchain GO_VERSION names, for linux and the host's architecture (or GOARCH).
Its module and build caches are GOMODCACHE and GOCACHE when set (to share
them between builds), and are otherwise made for the build and removed with
it.
"""
import hashlib
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import urllib.request
from pathlib import Path

GO_VERSION = 'go1.27.4'
# Node.js for the recipes that build a web UI: the release, and the sha256 of
# its linux-<arch>.tar.xz from the release's SHASUMS256.txt.
NODE_VERSION = 'v24.22.1'
NODE_SHA256 = {
    'x64': '6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6',
    'arm64': 'dbabf7a1cbf947e6059ffad2c27b80041db97dea1e553b62afdee7f13fca336e',
}
REPO = Path(__file__).resolve().parents[2]


def log(message):
    print(f'==> {message}', flush=True)


def run(cmd, cwd, env, shell=False):
    log(cmd if shell else ' '.join(cmd))
    subprocess.run(['bash', '-euo', 'pipefail', '-c', cmd] if shell else cmd, cwd=cwd, env=env, check=True)


def fetch(url, dest, sha256):
    with urllib.request.urlopen(url) as response, open(dest, 'wb') as out:
        shutil.copyfileobj(response, out)
    digest = hashlib.sha256(Path(dest).read_bytes()).hexdigest()
    if digest != sha256:
        raise SystemExit(f'sha256 mismatch for {url}: expected {sha256}, got {digest}')


def extract(archive, dest, strip=0):
    dest.mkdir(parents=True, exist_ok=True)
    subprocess.run(['tar', '-xf', str(archive), '-C', str(dest), f'--strip-components={strip}'], check=True)


def version_key(v):
    m = re.match(r'v?(\d+)\.(\d+)\.(\d+)(.*)', v)
    if not m:
        return None
    # A pre-release sorts before its release.
    return (int(m[1]), int(m[2]), int(m[3]), 0 if m[4] and not m[4].startswith('+') else 1)


def floors():
    out = {}
    for line in (REPO / 'scripts/release/go-floor.txt').read_text().splitlines():
        line = line.split('#', 1)[0].split()
        if line:
            out[line[0]] = line[1:]
    return out


def floor_for(current, versions):
    """The version current must be raised to, or None."""
    cur = version_key(current)
    if cur is None:
        return None
    same_line = [v for v in versions if version_key(v)[:2] == cur[:2]]
    target = same_line[0] if same_line else max(versions, key=version_key)
    if not same_line and version_key(target)[:2] < cur[:2]:
        return None
    return target if version_key(target) > cur else None


def required(moddir, env):
    out = subprocess.run(['go', 'mod', 'edit', '-json'], cwd=moddir, env=env, check=True,
                         stdout=subprocess.PIPE, text=True).stdout
    return {r['Path']: r['Version'] for r in json.loads(out).get('Require') or []}


def raise_floors(moddir, env):
    # One module at a time, reading the requirements again after each: raising
    # one can raise another past its floor (x/net raises x/crypto), and asking
    # for the floor then would lower it.
    changed = False
    for module, versions in floors().items():
        have = required(moddir, env)
        if module in have:
            target = floor_for(have[module], versions)
            if target:
                log(f'{module} {have[module]} -> {target}')
                run(['go', 'get', f'{module}@{target}'], moddir, env)
                changed = True
    if changed:
        if (moddir / 'vendor').is_dir():
            run(['go', 'mod', 'vendor'], moddir, env)


def node(tmp, env):
    """Puts Node.js on PATH: the pinned release on Linux, the host's elsewhere."""
    if platform.system() != 'Linux':
        if not shutil.which('node'):
            raise SystemExit('this recipe needs Node.js on PATH')
        return
    arch = {'x86_64': 'x64', 'aarch64': 'arm64'}[platform.machine()]
    archive = tmp / 'node.tar.xz'
    fetch(f'https://nodejs.org/dist/{NODE_VERSION}/node-{NODE_VERSION}-linux-{arch}.tar.xz', archive, NODE_SHA256[arch])
    extract(archive, tmp / 'node', strip=1)
    env['PATH'] = f"{tmp / 'node' / 'bin'}:{env['PATH']}"


def main():
    name = os.environ['PACKAGE_NAME']
    version = os.environ['PACKAGE_VERSION']
    source_url = os.environ['SOURCE_URL']
    recipe = next((r for r in json.loads((REPO / 'scripts/release/packages.json').read_text())
                   if r['package'] == name and r['version'] == version), None)
    if not recipe or 'go' not in recipe:
        raise SystemExit(f'no Go recipe for {name}:{version}')
    go = recipe['go']
    upstream = recipe.get('upstream_version', version)
    arch = os.environ.get('GOARCH') or {'x86_64': 'amd64', 'amd64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}[platform.machine().lower()]

    # A fixed build directory, so paths the build records do not vary.
    tmp = Path(f'/tmp/jerboa-pkg-{name}-{version}')
    shutil.rmtree(tmp, ignore_errors=True)
    src = tmp / 'src'
    src.mkdir(parents=True)

    env = dict(os.environ)
    # -modcacherw: a module cache Go leaves read-only cannot be removed.
    env.update(GOOS='linux', GOARCH=arch, CGO_ENABLED='1' if go.get('cgo') else '0', GOTOOLCHAIN=GO_VERSION,
               GOFLAGS='-mod=mod -modcacherw', GOPATH=str(tmp / 'gopath'), VERSION=upstream)
    env.setdefault('GOCACHE', str(tmp / 'gocache'))
    env.setdefault('GOMODCACHE', str(tmp / 'gopath' / 'mod'))
    # A go.work in the source would put the build in workspace mode, where
    # -mod=mod is refused, and build with modules the floors do not raise.
    env['GOWORK'] = 'off'
    env.update(go.get('env', {}))
    if go.get('cgo') and platform.system() != 'Linux':
        raise SystemExit('a cgo recipe builds on Linux only')

    archive = tmp / 'source'
    with urllib.request.urlopen(source_url) as response, open(archive, 'wb') as out:
        shutil.copyfileobj(response, out)
    extract(archive, src, strip=recipe.get('source_strip', 1))
    for i, asset in enumerate(go.get('assets', [])):
        path = tmp / f'asset-{i}'
        log(f"asset {asset['url']}")
        fetch(asset['url'], path, asset['sha256'])
        extract(path, src / asset['dest'], strip=asset.get('strip', 0))
    if go.get('node'):
        node(tmp, env)
    for cmd in go.get('prepare', []):
        run(cmd, src, env, shell=True)

    moddir = src / go.get('dir', '.')
    raise_floors(moddir, env)
    out = REPO / 'dist' / 'pkg' / name / version / name
    out.parent.mkdir(parents=True, exist_ok=True)
    ldflags = '-s -w ' + go.get('ldflags', '').replace('{version}', upstream)
    cmd = ['go', 'build', '-trimpath', '-buildvcs=false', '-ldflags', ldflags, '-o', str(out)]
    if go.get('tags'):
        cmd += ['-tags', go['tags']]
    run(cmd + [go.get('main', '.')], moddir, env)
    run(['go', 'version', '-m', str(out)], moddir, env)
    out.chmod(0o755)
    log(f'built {name}:{version}')


if __name__ == '__main__':
    try:
        sys.exit(main())
    finally:
        tmp = Path(f"/tmp/jerboa-pkg-{os.environ.get('PACKAGE_NAME')}-{os.environ.get('PACKAGE_VERSION')}")
        subprocess.run(['chmod', '-R', 'u+w', str(tmp)], stderr=subprocess.DEVNULL)
        shutil.rmtree(tmp, ignore_errors=True)
