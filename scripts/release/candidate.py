#!/usr/bin/env python3
"""Release protocol: build immutable candidates; promote the SAME verified bytes.

No command other than promote writes public release artifacts or channel pointers.
The promotion workflow serializes writers. Existing immutable keys must match exactly.
"""
import argparse
import base64
import datetime
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tempfile
import tarfile
from urllib.parse import urlparse

BASE = 'https://releases.jerboa.dev'
KERNEL_FILES = {'mkfs': 'mkfs-linux-amd64', 'dump': 'dump-linux-amd64',
                'boot.img': 'boot.img', 'kernel.img': 'kernel.img',
                'kernel-fc.img': 'kernel-fc.img'}

REQUIRED = {'cli': {'windows-amd64', 'linux-amd64', 'linux-arm64', 'darwin-arm64'},
            'daemon': {'linux-amd64', 'darwin-arm64'},
            'desktop': {'windows-amd64', 'darwin-arm64'}}


def run(*args, **kwargs):
    return subprocess.check_output([str(a) for a in args], **kwargs)


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')


def version(value):
    if not re.fullmatch(r'v\d+\.\d+\.\d+', value):
        raise ValueError('Expected stable version vMAJOR.MINOR.PATCH')
    return value


def version_tuple(value):
    return tuple(map(int, version(value)[1:].split('.')))


def sha(value):
    if not re.fullmatch(r'[0-9a-f]{40}', value):
        raise ValueError('Source revisions must be complete commit SHAs')
    return value


def safe_path(value):
    p = PurePosixPath(value)
    if p.is_absolute() or '..' in p.parts or '\\' in value or not value or str(p) != value:
        raise ValueError(f'Unsafe artifact path: {value}')
    return value


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as f:
        while b := f.read(1024 * 1024):
            h.update(b)
    return {'sha256': h.hexdigest(), 'size': Path(path).stat().st_size}


def download(url, path):
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    run('curl', '--fail', '--silent', '--show-error', '--location', '--retry', '3',
        '--max-time', '240', '--output', path, url)


def build_verifier():
    if not Path('bin/jerboa-verify').exists():
        run('go', 'build', '-o', 'bin/jerboa-verify', './cmd/jerboa-verify')


def verify_signature(path):
    build_verifier()
    run('bin/jerboa-verify', path)


def sign(path, comment):
    run('bin/jerboa-sign', '-in', path, '-key-id', os.environ['KEY_ID'], '-comment', comment)
    verify_signature(path)


def lock():
    v = version(os.environ['VERSION'])
    if v != 'v' + Path('VERSION.md').read_text().strip():
        raise ValueError('Candidate version must match VERSION.md')
    kernel_source = os.environ['KERNEL_SOURCE']
    if kernel_source not in ('build', 'stable'):
        raise ValueError('Kernel source must be build or stable')
    kernel_version = version(os.environ['KERNEL_VERSION'])
    if kernel_source == 'build' and kernel_version != 'v' + Path('kernel/VERSION').read_text().strip():
        raise ValueError('Built kernel version must match kernel/VERSION')
    value = {'kernel_source': kernel_source, 'version': v, 'engine_sha': sha(os.environ['GITHUB_SHA']),
             'desktop_sha': sha(os.environ['DESKTOP_SHA']),
             'runtime_sha': sha(os.environ['RUNTIME_SHA']),
             'kernel_version': version(os.environ['KERNEL_VERSION']),
             'run_id': os.environ['GITHUB_RUN_ID'], 'repository': os.environ['GITHUB_REPOSITORY'],
             'toolchains': {'go': '1.27.1', 'node': '22', 'rust': '1.97.0'}}
    write_json('release-lock.json', value)
    with open(os.environ['GITHUB_OUTPUT'], 'a') as out:
        for k in ['version', 'desktop_sha', 'runtime_sha', 'kernel_version', 'kernel_source']:
            out.write(f'{k}={value[k]}\n')


def cli(args):
    # This manifest is authenticated by the same GitHub run's artifact boundary.
    # Production manifests are additionally signed during assembly.
    write_json(args.out, {'version': version(args.version), 'file': Path(args.file).name,
                         **digest(args.file)})


def assert_manifest(manifest, expected):
    version(expected)
    components = manifest['components']
    for name in ['cli', 'daemon', 'desktop', 'distro']:
        if components[name]['version'] != expected:
            raise ValueError(f'Version mismatch: {name}')
    for name, platforms in REQUIRED.items():
        if not platforms <= set(components[name].get('platforms', {})):
            raise ValueError(f'Missing required platforms: {name}')
    if components['distro']['kernel'] != components['kernel']['version']:
        raise ValueError('Kernel compatibility mismatch')
    for asset in assets(manifest):
        if not asset['url'].startswith(BASE + '/') or not re.fullmatch('[0-9a-f]{64}', asset['sha256']) or asset['size'] <= 0:
            raise ValueError('Malformed release asset')
        safe_path(asset['url'][len(BASE)+1:])


def assets(manifest):
    for component in manifest['components'].values():
        yield from (component.get('platforms') or component.get('files') or {'file': component}).values()


def verify_linux_versions(manifest, payload, expected):
    # This job runs on Linux amd64. Check actual release binary contents, not
    # just filenames/metadata, including the daemon embedded in the WSL rootfs.
    for name in ['cli', 'daemon']:
        asset = manifest['components'][name]['platforms']['linux-amd64']
        binary = payload / asset['url'][len(BASE)+1:]
        binary.chmod(0o755)
        output = run(binary.resolve(), '--version', text=True)
        if expected not in output.split():
            raise ValueError('Binary version mismatch: ' + output)
    distro = manifest['components']['distro']['url'][len(BASE)+1:]
    with tarfile.open(payload/distro) as archive, tempfile.TemporaryDirectory() as tmp:
        for name, asset in manifest['components']['kernel']['files'].items():
            path = 'root/.jerboa/tools/' + name
            entries = [m for m in archive.getmembers() if m.name.removeprefix('./') == path]
            if len(entries) != 1 or not entries[0].isfile():
                raise ValueError('Distro kernel tool missing or non-regular: ' + name)
            target = Path(tmp) / name
            with archive.extractfile(entries[0]) as source, target.open('wb') as dest:
                shutil.copyfileobj(source, dest)
            if digest(target) != {'sha256': asset['sha256'], 'size': asset['size']}:
                raise ValueError('Distro kernel differs from tested toolset: ' + name)
        members = [m for m in archive.getmembers() if m.name.lstrip('./') == 'usr/local/bin/jerboad']
        if len(members) != 1 or not members[0].isfile():
            raise ValueError('Distro daemon missing, duplicated or non-regular')
        binary=Path(tmp)/'jerboad'
        with archive.extractfile(members[0]) as source:
            binary.write_bytes(source.read())
        binary.chmod(0o755)
        output=run(binary,'--version',text=True)
        if expected not in output.split():
            raise ValueError('Distro binary version mismatch: ' + output)


def verify_kernel_evidence(record, boot, spec):
    for key in ('engine_sha', 'run_id', 'kernel_version', 'kernel_source'):
        if record[key] != spec[key]:
            raise ValueError('Kernel provenance mismatch: ' + key)
    if record['kernel_source'] not in ('build', 'stable'):
        raise ValueError('Unknown kernel source')
    version(record['kernel_version']); sha(record['engine_sha'])
    if set(record['files']) != set(KERNEL_FILES.values()):
        raise ValueError('Incomplete kernel toolset')
    if boot is not None:
        if boot.get('toolset') != record or boot.get('checks') != {'qemu': 'pass', 'firecracker': 'pass'}:
            raise ValueError('Kernel boot evidence does not match toolset')


def verify_kernel_toolset(directory, spec, require_boot=False):
    directory = Path(directory)
    record = json.loads((directory / 'toolset.json').read_text())
    boot = json.loads((directory / 'boot-validation.json').read_text()) if require_boot else None
    verify_kernel_evidence(record, boot, spec)
    for filename, expected in record['files'].items():
        file = directory / filename
        if file.is_symlink() or not file.is_file() or digest(file) != expected:
            raise ValueError('Kernel toolset bytes changed: ' + filename)
        if expected['size'] <= 0:
            raise ValueError('Empty kernel artifact: ' + filename)
    return record, boot


def kernel():
    """Stage the selected toolset once; boot validation and assembly reuse it."""
    spec = json.loads(Path('release-lock.json').read_text())
    root = Path('kernel-candidate'); root.mkdir(exist_ok=False)
    source = spec['kernel_source']
    provenance = {}
    if source == 'build':
        if spec['kernel_version'] != 'v' + Path('kernel/VERSION').read_text().strip():
            raise ValueError('Built kernel version must match kernel/VERSION')
        output = Path('kernel/output')
        paths = {'mkfs-linux-amd64': output / 'tools/bin/mkfs',
                 'dump-linux-amd64': output / 'tools/bin/dump',
                 'boot.img': output / 'platform/pc/boot/boot.img',
                 'kernel.img': output / 'platform/pc/bin/kernel.img'}
        for filename, path in paths.items():
            shutil.copyfile(path, root / filename)
        run('strip', '-g', output / 'platform/pc/bin/kernel.elf', '-o', root / 'kernel-fc.img')
    elif source == 'stable':
        # Reuse is explicit and still needs boot checks. Never silently replace
        # a requested build with the currently published kernel.
        manifest = root / 'source-manifest.json'
        download(BASE + '/channels/stable.json', manifest)
        download(BASE + '/channels/stable.json.minisig', str(manifest) + '.minisig')
        verify_signature(manifest)
        component = json.loads(manifest.read_text())['components']['kernel']
        if component['version'] != spec['kernel_version'] or set(component['files']) != set(KERNEL_FILES):
            raise ValueError('Published kernel does not match requested toolset')
        provenance = {'stable_manifest': digest(manifest)}
        for name, filename in KERNEL_FILES.items():
            asset = component['files'][name]
            if asset['url'] != f'{BASE}/kernel/{spec["kernel_version"]}/{filename}':
                raise ValueError('Kernel URL outside selected version')
            download(asset['url'], root / filename)
            if digest(root / filename) != {'sha256': asset['sha256'], 'size': asset['size']}:
                raise ValueError('Published kernel checksum mismatch')
    else:
        raise ValueError('Unknown kernel source')
    for filename in ('mkfs-linux-amd64', 'dump-linux-amd64'):
        (root / filename).chmod(0o755)
    record = {key: spec[key] for key in ('engine_sha', 'run_id', 'kernel_version', 'kernel_source')}
    record.update(provenance)
    record['files'] = {filename: digest(root / filename) for filename in KERNEL_FILES.values()}
    write_json(root / 'toolset.json', record)
    verify_kernel_toolset(root, spec)


def assemble():
    spec = json.loads(Path('release-lock.json').read_text())
    v = version(spec['version']); n = v[1:]
    root = Path('candidate'); root.mkdir(exist_ok=True)
    payload = root / 'payload'
    manifest = {'channel': 'stable', 'components': {}}
    components = manifest['components']
    def stage(source, key):
        key = safe_path(key); target = payload / key
        target.parent.mkdir(parents=True, exist_ok=True); shutil.copyfile(source, target)
        return {'url': BASE + '/' + key, **digest(target)}
    kernel_record, kernel_boot = verify_kernel_toolset('kernel-candidate', spec, require_boot=True)
    components['kernel'] = {'version': spec['kernel_version'], 'files': {
        name: stage(Path('kernel-candidate') / filename, f'kernel/{spec["kernel_version"]}/{filename}')
        for name, filename in KERNEL_FILES.items()}}
    for component, platforms in REQUIRED.items():
        components[component] = {'version': v, 'platforms': {}}
        for platform in sorted(platforms):
            if component == 'desktop':
                name = f'jerboa-desktop-setup-{n}.exe' if platform == 'windows-amd64' else f'jerboa-desktop-{n}-macos-arm64.dmg'
                source = Path('desktop-windows' if platform == 'windows-amd64' else 'desktop-macos') / name
                key = f'desktop/{name}' if platform == 'windows-amd64' else f'desktop/{v}/{name}'
            else:
                name = ('jerboa' if component == 'cli' else 'jerboad') + '-' + platform + ('.exe' if platform.startswith('windows') else '')
                source = Path('dist') / name; key = f'{component}/{v}/{name}'
            components[component]['platforms'][platform] = stage(source, key)
    components['distro'] = {'version': v, 'kernel': spec['kernel_version'], **stage('distro/jerboa-rootfs-amd64.tar.gz', f'distro/{v}/jerboa-rootfs-amd64.tar.gz')}
    installer = f'jerboa-desktop-setup-{n}.exe'
    stage(Path('desktop-windows') / (installer + '.blockmap'), 'desktop/' + installer + '.blockmap')
    # Generate the updater feed ourselves from the staged final installer bytes.
    binary = payload / 'desktop' / installer
    h = hashlib.sha512(binary.read_bytes()).digest(); h = base64.b64encode(h).decode()
    date = datetime.datetime.now(datetime.timezone.utc).date().isoformat()
    feed = f'version: {n}\nfiles:\n  - url: {installer}\n    sha512: {h}\n    size: {binary.stat().st_size}\npath: {installer}\nsha512: {h}\nreleaseDate: "{date}T00:00:00.000Z"\n'
    (root / 'latest.yml').write_text(feed)
    assert_manifest(manifest, v)
    verify_linux_versions(manifest, payload, v)
    write_json(root / 'manifest.json', manifest)
    record = {**spec, 'kernel_toolset': kernel_record, 'kernel_boot': kernel_boot, 'date': date, 'assets': {str(p.relative_to(payload)): digest(p) for p in sorted(payload.rglob('*')) if p.is_file()},
              'manifest': digest(root / 'manifest.json'), 'feed': digest(root / 'latest.yml'),
              'native_macos_app': digest(Path('native-macos') / f'jerboa-desktop-{n}-macos-arm64.zip')}
    write_json(root / 'inventory.json', record)
    sign(root / 'manifest.json', f'release:{v}')
    sign(root / 'inventory.json', f'candidate:{spec["run_id"]} version:{v}')


def verify_candidate(root):
    root = Path(root)
    for p in root.rglob('*'):
        if p.is_symlink():
            raise ValueError('Candidate must not contain symlinks')
    verify_signature(root / 'inventory.json'); verify_signature(root / 'manifest.json')
    inv = json.loads((root / 'inventory.json').read_text())
    manifest = json.loads((root / 'manifest.json').read_text())
    assert_manifest(manifest, inv['version'])
    if digest(root/'manifest.json') != inv['manifest'] or digest(root/'latest.yml') != inv['feed']:
        raise ValueError('Candidate metadata hash mismatch')
    for key, expected in inv['assets'].items():
        if digest(root/'payload'/safe_path(key)) != expected:
            raise ValueError(f'Candidate bytes changed: {key}')
    actual = {str(p.relative_to(root/'payload')) for p in (root/'payload').rglob('*') if p.is_file()}
    if actual != set(inv['assets']):
        raise ValueError('Unexpected candidate payload')
    for a in assets(manifest):
        key = a['url'][len(BASE)+1:]
        if inv['assets'].get(key) != {'sha256': a['sha256'], 'size': a['size']}:
            raise ValueError('Manifest does not match inventory')
    if 'kernel_source' in inv:
        verify_kernel_evidence(inv['kernel_toolset'], inv['kernel_boot'], inv)
        expected = {name: {'url': f'{BASE}/kernel/{inv["kernel_version"]}/{filename}',
                           **inv['kernel_toolset']['files'][filename]}
                    for name, filename in KERNEL_FILES.items()}
        if manifest['components']['kernel'] != {'version': inv['kernel_version'], 'files': expected}:
            raise ValueError('Kernel manifest does not match tested toolset')
    return inv, manifest


def api(path):
    return json.loads(run('gh', 'api', path))


def authorize():
    ident = os.environ['CANDIDATE_RUN']
    if not ident.isdigit():
        raise ValueError('Invalid candidate run ID')
    result = api(f'repos/{os.environ["GITHUB_REPOSITORY"]}/actions/runs/{ident}')
    if result['path'] != '.github/workflows/release.yml' or result['event'] != 'workflow_dispatch' or result['conclusion'] != 'success' or result['head_branch'] != 'main':
        raise ValueError('Promotion requires a successful candidate built from main')
    evidence = urlparse(os.environ['MACOS_EVIDENCE'])
    if evidence.scheme != 'https' or not evidence.netloc:
        raise ValueError('Native Mac evidence must be an HTTPS report URL')
    write_json('authorized-run.json', {'run_id': ident, 'engine_sha': result['head_sha']})


def aws(*args):
    return run('aws', 's3api', *args, '--endpoint-url', os.environ['ENDPOINT'])


def existing(key, temp):
    # Only a confirmed missing object permits a new immutable write. Auth/network
    # failures must abort rather than accidentally authorize an overwrite.
    proc = subprocess.run(['aws', 's3api', 'head-object', '--bucket', os.environ['BUCKET'], '--key', key,
                           '--endpoint-url', os.environ['ENDPOINT']], capture_output=True, text=True)
    if proc.returncode:
        if '(404)' in proc.stderr or '(NoSuchKey)' in proc.stderr or '(NotFound)' in proc.stderr:
            return None
        raise RuntimeError('Cannot inspect existing object: ' + proc.stderr)
    aws('get-object', '--bucket', os.environ['BUCKET'], '--key', key, temp)
    return Path(temp)


def put(path, key, immutable=True, content_type='application/octet-stream'):
    if immutable:
        with tempfile.TemporaryDirectory() as tmp:
            prev = existing(key, str(Path(tmp)/'existing'))
            if prev:
                if digest(prev) != digest(path):
                    raise ValueError('Refusing to overwrite published bytes: ' + key)
                print('Already verified:', key); return
    aws('put-object', '--bucket', os.environ['BUCKET'], '--key', key, '--body', path,
        '--content-type', content_type, '--cache-control', 'public, max-age=31536000, immutable' if immutable else 'no-cache')


def promote():
    root = Path('candidate');inv, manifest = verify_candidate(root)
    authorized = json.loads(Path('authorized-run.json').read_text())
    if inv['run_id'] != authorized['run_id'] or inv['engine_sha'] != authorized['engine_sha']:
        raise ValueError('Artifact provenance does not match authorized run')
    v = inv['version'];before = Path('previous-stable.json')
    # Read origin directly; a previous promotion may have failed between the
    # manifest and its detached signature. Same-version resume is allowed only
    # when the manifest bytes match this signed candidate exactly.
    previous = existing('channels/stable.json', str(before))
    if previous:
        old = json.loads(previous.read_text())
        old_v = old['components']['cli']['version']
        if version_tuple(old_v) > version_tuple(v):
            raise ValueError('Refusing an accidental downgrade')
        if version_tuple(old['components']['kernel']['version']) > version_tuple(manifest['components']['kernel']['version']):
            raise ValueError('Refusing a kernel downgrade')
        if old['components']['kernel']['version'] == manifest['components']['kernel']['version'] and old['components']['kernel'] != manifest['components']['kernel']:
            raise ValueError('Kernel version already published with different bytes; bump kernel/VERSION')
        if old_v == v and digest(previous) != digest(root/'manifest.json'):
            raise ValueError('Version already published with another manifest; cut a new release')
    # Persist a signed promotion record before changing channel pointers.
    promotion = {'version': v, 'candidate_run': inv['run_id'], 'inventory': inv,
                 'macos_evidence': os.environ['MACOS_EVIDENCE']}
    write_json('promotion.json', promotion);sign('promotion.json', f'promotion:{v}')
    for key in sorted(inv['assets']):
        put(root/'payload'/key, key)
    for name in ['manifest.json', 'manifest.json.minisig', 'inventory.json', 'inventory.json.minisig', 'latest.yml']:
        put(root/name, f'releases/{v}/{name}')
    # Immutable record is candidate-specific; re-approval may supply a different
    # evidence URL, so keep promotion receipts per workflow run.
    receipt = f'releases/{v}/promotions/{os.environ["GITHUB_RUN_ID"]}'
    put('promotion.json', receipt+'.json');put('promotion.json.minisig', receipt+'.json.minisig')
    # Verify every public download BEFORE changing stable.
    with tempfile.TemporaryDirectory() as tmp:
        for key, expected in inv['assets'].items():
            p = Path(tmp)/'asset';download(BASE+'/'+key,p)
            if digest(p) != expected:raise ValueError('Public download mismatch: '+key)
    put(root/'manifest.json', 'channels/stable.json', False, 'application/json')
    put(root/'manifest.json.minisig', 'channels/stable.json.minisig', False, 'text/plain')
    put(root/'latest.yml', 'desktop/latest.yml', False, 'text/yaml')
    # Read-back pointer verification is separate from artifact verification.
    with tempfile.TemporaryDirectory() as tmp:
        for remote,local in [('channels/stable.json','manifest.json'),('channels/stable.json.minisig','manifest.json.minisig'),('desktop/latest.yml','latest.yml')]:
            p=Path(tmp)/'pointer';download(BASE+'/'+remote,p)
            if digest(p)!=digest(root/local):raise ValueError('Pointer mismatch: '+remote)
    # Website release date is the first completed promotion, not build time.
    published_key=f'releases/{v}/published.json'
    with tempfile.TemporaryDirectory() as tmp:
        prior=existing(published_key,str(Path(tmp)/'published.json'))
        if prior:
            published=json.loads(prior.read_text())
            if published['version']!=v or published['inventory_sha256']!=digest(root/'inventory.json')['sha256']:
                raise ValueError('Publication record belongs to another candidate')
        else:
            published={'version':v,'date':datetime.datetime.now(datetime.timezone.utc).date().isoformat(),
                       'inventory_sha256':digest(root/'inventory.json')['sha256']}
        write_json('published.json',published);sign('published.json',f'published:{v}')
        put('published.json',published_key,content_type='application/json')
        put('published.json.minisig',published_key+'.minisig',content_type='text/plain')
    print('PROMOTED',v)


def website():
    inv=json.loads(Path('candidate/inventory.json').read_text())
    body={'ref':'main','inputs':{'version':inv['version']}}
    run('gh','api','--method','POST','repos/AitorConS/Jerboa_Docs/actions/workflows/release-sync.yml/dispatches','--input','-',input=json.dumps(body).encode())


def summary():
    inventory=Path('candidate/inventory.json')
    promotion=os.environ.get('PROMOTION_RESULT')
    if inventory.exists():
        inv=json.loads(inventory.read_text())
        text=f"## Release {inv['version']}\n\nCandidate run: {inv['run_id']}\n\nEngine: `{inv['engine_sha']}`\n\nDesktop: `{inv['desktop_sha']}`\n\nArtifacts: {len(inv['assets'])}\n\n"
        if 'kernel_source' in inv:
            text+=f"Kernel: `{inv['kernel_version']}` ({inv['kernel_source']}); QEMU/KVM and Firecracker boot evidence verified.\n\n"
    else:
        text='## Release promotion\n\nNo candidate inventory was downloaded.\n\n'
    if promotion=='success':
        text+='Publication: **complete**, with public downloads and channel pointers verified.\n\n'
    elif promotion is not None:
        text+=f'Publication step: **{promotion or "not started"}**. Inspect the failed step before retrying; pointers may be partially updated.\n\n'
    else:
        text+='Candidate assembled. **Stable has not been changed by this workflow.**\n\n'
    if promotion is not None:
        web=os.environ.get('WEBSITE_RESULT','not started')
        text+=f'Website dispatch: **{web}**. A successful dispatch means queued, not deployed; verify release-sync and Vercel separately.\n'
    with open(os.environ.get('GITHUB_STEP_SUMMARY','release-summary.md'),'a') as f:f.write(text)


if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('command',choices=['lock','cli','assemble','kernel','authorize','promote','website','summary']);p.add_argument('--file');p.add_argument('--version');p.add_argument('--out');a=p.parse_args()
    if a.command=='cli':cli(a)
    else:globals()[a.command]()
