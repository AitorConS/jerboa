"""Exercise kernel selection, byte identity and promotion rejection without R2 writes."""
import io
import json
import os
from pathlib import Path
import shutil
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import candidate as c

FLOOR = '2026-01-02T03:04:05Z'

# The inventory's fields per pipeline generation (candidate.PIPELINE). A
# promotion only accepts candidates of its own generation, which is what makes
# candidates assembled before a pipeline change get rebuilt.
INVENTORY_FIELDS = {2: {
    'version', 'engine_sha', 'desktop_sha', 'runtime_sha', 'kernel_version', 'kernel_source', 'run_id',
    'repository', 'toolchains', 'min_stable_generated', 'stable_kernel', 'pipeline', 'kernel_toolset',
    'kernel_boot', 'date', 'assets', 'manifest', 'feed', 'native_macos_app', 'macos_x86_tools'}}


class KernelCandidateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        old = os.getcwd()
        os.chdir(self.temp.name)
        self.addCleanup(os.chdir, old)
        self.spec = {'version': 'v1.0.0', 'engine_sha': 'a' * 40,
                     'desktop_sha': 'b' * 40, 'runtime_sha': 'c' * 40,
                     'kernel_version': 'v0.2.1', 'kernel_source': 'build',
                     'run_id': '123', 'repository': 'AitorConS/jerboa',
                     'toolchains': {'go': '1.27.4'}, 'min_stable_generated': FLOOR,
                     'stable_kernel': {'version': 'v0.2.0', 'files': {}}}
        c.write_json('release-lock.json', self.spec)
        Path('scripts').mkdir()
        Path('scripts/install.sh').write_text('#!/bin/sh\n# tested bootstrap\nMIN_GENERATED=""\n')
        Path('kernel').mkdir()
        Path('kernel/VERSION').write_text('0.2.1\n')
        for name in ['tools/bin/mkfs', 'tools/bin/dump', 'platform/pc/boot/boot.img',
                     'platform/pc/bin/kernel.img', 'platform/pc/bin/kernel.elf']:
            p = Path('kernel/output') / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(name.encode())

    def stage(self):
        def strip(*args):
            self.assertEqual(args[:2], ('strip', '-g'))
            shutil.copyfile(args[2], args[4])
        with patch.object(c, 'run', side_effect=strip), patch.object(c, 'download') as download:
            c.kernel()
            download.assert_not_called()
        self.record, _ = c.verify_kernel_toolset('kernel-candidate', self.spec)
        # The selected-kernel upload, as kernel-stage produced it.
        shutil.copytree('kernel-candidate', 'kernel-selected')
        self.boot = {'toolset': self.record, 'checks': {'qemu': 'pass', 'firecracker': 'pass'}}
        c.write_json('kernel-candidate/boot-validation.json', self.boot)

    def test_build_uses_local_files_and_requires_matching_boots(self):
        self.stage()
        record, boot = c.verify_kernel_toolset('kernel-candidate', self.spec, require_boot=True)
        self.assertEqual(record['files']['kernel-fc.img'], c.digest('kernel/output/platform/pc/bin/kernel.elf'))
        self.assertEqual(boot['checks']['firecracker'], 'pass')
        del self.boot['checks']['firecracker']
        c.write_json('kernel-candidate/boot-validation.json', self.boot)
        with self.assertRaisesRegex(ValueError, 'boot evidence'):
            c.verify_kernel_toolset('kernel-candidate', self.spec, require_boot=True)

    def test_tampered_bytes_missing_files_and_symlinks_are_rejected(self):
        self.stage()
        p = Path('kernel-candidate/kernel-fc.img')
        original = p.read_bytes()
        p.write_bytes(b'changed')
        with self.assertRaisesRegex(ValueError, 'bytes changed'):
            c.verify_kernel_toolset('kernel-candidate', self.spec, True)
        p.unlink()
        with self.assertRaisesRegex(ValueError, 'bytes changed'):
            c.verify_kernel_toolset('kernel-candidate', self.spec, True)
        Path('outside').write_bytes(original)
        p.symlink_to(Path('outside').resolve())
        with self.assertRaisesRegex(ValueError, 'bytes changed'):
            c.verify_kernel_toolset('kernel-candidate', self.spec, True)

    def test_boot_validation_may_only_add_its_report(self):
        self.stage()
        c.verify_selected_kernel()
        # A toolset rewritten consistently after selection still verifies on
        # its own, but no longer matches the selected upload.
        for filename in ['kernel-fc.img', 'toolset.json']:
            with self.subTest(filename=filename):
                p = Path('kernel-candidate') / filename
                original = p.read_bytes()
                p.write_bytes(original + b' ')
                with self.assertRaisesRegex(ValueError, 'differs from the selected'):
                    c.verify_selected_kernel()
                p.write_bytes(original)
        # Assembly checks it before anything else.
        Path('kernel-candidate/kernel-fc.img').write_bytes(b'changed')
        with self.assertRaisesRegex(ValueError, 'differs from the selected'):
            self.assemble_only()
        Path('kernel-candidate/kernel-fc.img').unlink()
        shutil.copyfile('kernel-selected/kernel-fc.img', 'kernel-candidate/kernel-fc.img')
        Path('kernel-candidate/extra').write_bytes(b'x')
        with self.assertRaisesRegex(ValueError, 'differs from the selected'):
            c.verify_selected_kernel()
        Path('kernel-candidate/extra').unlink()
        Path('kernel-candidate/boot-validation.json').unlink()
        with self.assertRaisesRegex(ValueError, 'differs from the selected'):
            c.verify_selected_kernel()

    def test_provenance_must_match_candidate(self):
        self.stage()
        for key, value in [('run_id', '456'), ('engine_sha', 'd' * 40),
                           ('kernel_version', 'v0.2.2'), ('kernel_source', 'stable')]:
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, 'provenance'):
                c.verify_kernel_toolset('kernel-candidate', {**self.spec, key: value}, True)

    def test_build_version_must_match_source(self):
        Path('kernel/VERSION').write_text('0.2.0\n')
        with self.assertRaisesRegex(ValueError, 'kernel/VERSION'):
            c.kernel()

    def test_stable_is_explicit_signed_and_checksum_verified(self):
        self.spec['kernel_source'] = 'stable'
        c.write_json('release-lock.json', self.spec)
        file = Path('asset'); file.write_bytes(b'published')
        component = {'version': 'v0.2.1', 'files': {
            name: {'url': f'{c.BASE}/kernel/v0.2.1/{filename}', **c.digest(file)}
            for name, filename in c.KERNEL_FILES.items()}}
        def download(url, target):
            if url.endswith('stable.json'):
                c.write_json(target, {'channel': channel, 'components': {'kernel': component}})
            elif url.endswith('.minisig'):
                Path(target).write_bytes(b'signature')
            else:
                shutil.copyfile(file, target)
        # A validly signed manifest of another channel (an unpromoted
        # candidate) is not the published toolset.
        channel = 'candidate'
        with patch.object(c, 'download', side_effect=download), patch.object(c, 'verify_signature'):
            with self.assertRaisesRegex(ValueError, 'stable channel'):
                c.kernel()
        shutil.rmtree('kernel-candidate')
        channel = 'stable'
        with patch.object(c, 'download', side_effect=download), patch.object(c, 'verify_signature') as verify:
            c.kernel()
            verify.assert_called_once()
        record, _ = c.verify_kernel_toolset('kernel-candidate', self.spec)
        self.assertIn('stable_manifest', record)
        shutil.rmtree('kernel-candidate')
        component['files']['kernel.img']['sha256'] = '0' * 64
        with patch.object(c, 'download', side_effect=download), patch.object(c, 'verify_signature'):
            with self.assertRaisesRegex(ValueError, 'checksum'):
                c.kernel()

    def host_binaries(self):
        elf = bytearray(64); elf[:4] = b'\x7fELF'; elf[18:20] = (183).to_bytes(2, 'little')
        macho = bytearray(64); macho[:4] = b'\xcf\xfa\xed\xfe'; macho[4:8] = (0x0100000c).to_bytes(4, 'little')
        for platform, data in [('linux-arm64', elf), ('darwin-arm64', macho)]:
            p = c.TOOLSET_MKFS[platform]; p.parent.mkdir(exist_ok=True); p.write_bytes(bytes(data))

    def assemble(self):
        self.stage()
        self.host_binaries()
        return self.assemble_only()

    def assemble_only(self):
        for component, platforms in c.REQUIRED.items():
            for platform in platforms:
                if component == 'desktop':
                    name = 'jerboa-desktop-setup-1.0.0.exe' if platform == 'windows-amd64' else 'jerboa-desktop-1.0.0-macos-arm64.dmg'
                    folder = 'desktop-windows' if platform == 'windows-amd64' else 'desktop-macos'
                else:
                    name = ('jerboa' if component == 'cli' else 'jerboad') + '-' + platform + ('.exe' if platform.startswith('windows') else '')
                    folder = 'dist'
                # Release builds carry the candidate's stable floor.
                p = Path(folder) / name; p.parent.mkdir(exist_ok=True); p.write_bytes(name.encode() + b'\0' + FLOOR.encode())
        Path('desktop-windows/jerboa-desktop-setup-1.0.0.exe.blockmap').write_bytes(b'blockmap')
        Path('native-macos').mkdir()
        Path('native-macos/jerboa-desktop-1.0.0-macos-arm64.zip').write_bytes(b'app')
        Path('distro').mkdir()
        with tarfile.open('distro/jerboa-rootfs-amd64.tar.gz', 'w:gz') as archive:
            for name, filename in c.KERNEL_FILES.items():
                archive.add(Path('kernel-candidate') / filename, arcname='root/.jerboa/tools/' + name)
            data = b'daemon'; info = tarfile.TarInfo('usr/local/bin/jerboad'); info.size = len(data)
            archive.addfile(info, io.BytesIO(data))
        with patch.object(c, 'sign'), patch.object(c, 'run', return_value='jerboa v1.0.0\n'), patch.object(c, 'download') as download, \
                patch.object(c, 'release_notes', return_value='### Fixed\n\n- A thing.\n') as notes:
            c.assemble()
            notes.assert_called_once_with('v1.0.0')
            download.assert_not_called()
        with patch.object(c, 'verify_signature'):
            return c.verify_candidate('candidate')

    def evidence(self, inv, **changes):
        c.write_json(f'docs/releases/{inv["version"]}/native-macos.json', {
            'candidate_run': inv['run_id'], 'app_sha256': inv['native_macos_app']['sha256'],
            'version': inv['version'], 'result': 'pass', **changes})

    def test_inventory_fields_are_pinned_to_the_pipeline_generation(self):
        inv, _ = self.assemble()
        self.assertIn(c.PIPELINE, INVENTORY_FIELDS, 'add the inventory fields of the new PIPELINE to INVENTORY_FIELDS')
        self.assertEqual(set(inv), INVENTORY_FIELDS[c.PIPELINE],
                         'the inventory changed: candidates assembled before this change do not have the same '
                         'fields, so bump candidate.PIPELINE (promotion then makes them be rebuilt) and add the '
                         'new set to INVENTORY_FIELDS')
        self.assertEqual(inv['pipeline'], c.PIPELINE)
        # The lock's fields all reach the inventory, floor included.
        self.assertEqual(inv['min_stable_generated'], FLOOR)

    def test_candidate_of_another_pipeline_is_not_promotable(self):
        self.assemble()
        original = json.loads(Path('candidate/inventory.json').read_text())
        for change, message in [({'pipeline': c.PIPELINE - 1}, 'release pipeline 1 and promotion is at'),
                                ({'pipeline': None}, 'release pipeline'),
                                ({'pipeline': c.PIPELINE + 1}, 'dispatch "Build release candidate" again'),
                                ({'min_stable_generated': None}, 'does not record the stable floor'),
                                ({'min_stable_generated': 'yesterday'}, 'Expected a UTC time'),
                                ({'assets': {k: v for k, v in original['assets'].items() if not k.endswith('/notes.md')}}, 'no release notes')]:
            inv = dict(original)
            for key, value in change.items():
                if value is None: del inv[key]
                else: inv[key] = value
            c.write_json('candidate/inventory.json', inv)
            with self.subTest(change=change), patch.object(c, 'verify_signature'), self.assertRaisesRegex(ValueError, message):
                c.verify_candidate('candidate')

    def test_assembly_refuses_binaries_without_the_stable_floor(self):
        self.stage()
        self.host_binaries()
        for name in ['jerboa-windows-amd64.exe', 'jerboad-darwin-arm64']:
            with self.subTest(binary=name):
                shutil.rmtree('candidate', ignore_errors=True)
                for d in ['native-macos', 'distro']: shutil.rmtree(d, ignore_errors=True)
                original = Path.write_bytes
                def write(path, data, skip=name):
                    return original(path, skip.encode() if path.name == skip and path.parent.name == 'dist' else data)
                with patch.object(Path, 'write_bytes', write), self.assertRaisesRegex(ValueError, 'not built with the stable floor'):
                    self.assemble_only()
                self.assertFalse(Path('candidate/inventory.json').exists())

    def test_assembly_refuses_a_manifest_older_than_the_floor(self):
        self.stage()
        self.host_binaries()
        with patch.object(c, 'generated_now', return_value='2026-01-01T00:00:00Z'), \
                self.assertRaisesRegex(ValueError, 'older than its own stable floor'):
            self.assemble_only()

    def test_built_kernel_is_checked_against_the_published_one_before_anything_else(self):
        def strip(*args): shutil.copyfile(args[2], args[4])
        def staged():
            shutil.rmtree('kernel-candidate', ignore_errors=True)
            with patch.object(c, 'run', side_effect=strip): c.kernel()
        staged()
        files = json.loads(Path('kernel-candidate/toolset.json').read_text())['files']
        # The same version with the same bytes is a rebuild of the published kernel.
        self.spec['stable_kernel'] = {'version': 'v0.2.1', 'files': files}
        c.write_json('release-lock.json', self.spec); staged()
        changed = {**files, 'kernel.img': {'sha256': '0' * 64, 'size': 1}}
        for published, message in [({'version': 'v0.2.1', 'files': changed}, 'already published with different bytes: raise kernel/VERSION'),
                                   ({'version': 'v0.3.0', 'files': files}, 'older than the published v0.3.0')]:
            self.spec['stable_kernel'] = published
            c.write_json('release-lock.json', self.spec)
            with self.subTest(published=published['version']), self.assertRaisesRegex(ValueError, message):
                staged()

    def test_assembly_and_inventory_preserve_tested_kernel_bytes(self):
        inv, manifest = self.assemble()
        # A built kernel ships its host mkfs and the toolset document naming them.
        doc = json.loads(Path(f'candidate/payload/kernel/v0.2.1/toolset.json').read_text())
        self.assertEqual(set(doc['mkfs']), {'linux-amd64', 'linux-arm64', 'darwin-arm64'})
        self.assertEqual(doc['mkfs']['linux-amd64'], manifest['components']['kernel']['files']['mkfs'])
        self.assertIn('kernel/v0.2.1/toolset.json', inv['assets'])
        self.assertIn('kernel/v0.2.1/mkfs-darwin-arm64', inv['assets'])
        self.assertEqual(inv['kernel_toolset'], self.record)
        self.assertEqual(inv['macos_x86_tools'], {'version': 'v0.2.1', 'files': {
            name: self.record['files'][name] for name in c.X86_TOOLS}})
        self.assertEqual(manifest['components']['distro']['kernel'], 'v0.2.1')
        # The released installer refuses stable manifests older than this one.
        installer = Path('candidate/payload/releases/v1.0.0/install.sh').read_text()
        self.assertIn(f'MIN_GENERATED="{manifest["generated"]}"\n', installer)
        self.assertIn('releases/v1.0.0/install.sh', inv['assets'])
        # The changelog entry travels in the signed inventory as the release notes.
        self.assertEqual(Path('candidate/payload/releases/v1.0.0/notes.md').read_text(), '### Fixed\n\n- A thing.\n')
        self.assertEqual(inv['assets']['releases/v1.0.0/notes.md'], c.digest('candidate/payload/releases/v1.0.0/notes.md'))
        for name, filename in c.KERNEL_FILES.items():
            self.assertEqual(manifest['components']['kernel']['files'][name]['sha256'], self.record['files'][filename]['sha256'])
        # CLI and daemon binaries ship a zstd variant beside them, covered by
        # the signed inventory and checked against the binary it replaces.
        for name in ['cli', 'daemon']:
            for asset in manifest['components'][name]['platforms'].values():
                self.assertIn(asset['url'][len(c.BASE)+1:] + c.ZSTD_SUFFIX, inv['assets'])
        c.verify_zstd_variants(manifest, Path('candidate/payload'))
        key = manifest['components']['cli']['platforms']['linux-amd64']['url'][len(c.BASE)+1:]
        packed = Path('candidate/payload') / (key + c.ZSTD_SUFFIX)
        c.zstd_variant(Path('dist/jerboad-linux-amd64'), packed)
        with self.assertRaisesRegex(ValueError, 'zstd variant differs'):
            c.verify_zstd_variants(manifest, Path('candidate/payload'))
        with patch.object(c, 'verify_signature'), self.assertRaisesRegex(ValueError, 'Candidate bytes changed'):
            c.verify_candidate('candidate')
        c.zstd_variant(Path('dist/jerboa-linux-amd64'), packed)
        inv['kernel_boot']['checks']['qemu'] = 'fail'
        c.write_json('candidate/inventory.json', inv)
        with patch.object(c, 'verify_signature'), self.assertRaisesRegex(ValueError, 'boot evidence'):
            c.verify_candidate('candidate')

    def test_promotion_publishes_new_kernel_before_advancing_stable(self):
        inv, manifest = self.assemble()
        self.evidence(inv)
        c.write_json('authorized-run.json', {'run_id': inv['run_id'], 'engine_sha': inv['engine_sha']})
        for name in ['manifest.json', 'inventory.json']:
            Path('candidate/' + name + '.minisig').write_bytes(b'signature')
        store = {}; events = []
        def put(path, key, *args, **kwargs):
            store[key] = Path(path).read_bytes(); events.append(('put', key))
        def download(url, target):
            key = url[len(c.BASE) + 1:]
            Path(target).write_bytes(store[key]); events.append(('get', key))
        def sign(path, comment):
            Path(str(path) + '.minisig').write_bytes(b'signature')
        with patch.object(c, 'verify_signature'), patch.object(c, 'existing', return_value=None), \
                patch.object(c, 'put', side_effect=put), patch.object(c, 'download', side_effect=download), \
                patch.object(c, 'sign', side_effect=sign), \
                patch.dict(os.environ, {'MACOS_EVIDENCE': 'https://example.org/report', 'GITHUB_RUN_ID': '456'}):
            c.promote()
        self.assertEqual(manifest['channel'], 'candidate')
        stable = json.loads(store['channels/stable.json'])
        self.assertEqual(stable, {**manifest, 'channel': 'stable'})
        self.assertEqual(stable['components']['kernel'], manifest['components']['kernel'])
        switch = events.index(('put', 'channels/stable.json'))
        for filename in c.KERNEL_FILES.values():
            key = 'kernel/v0.2.1/' + filename
            self.assertEqual(store[key], Path('kernel-candidate', filename).read_bytes())
            self.assertLess(events.index(('get', key)), switch)

    def test_mac_x86_guest_files_are_the_selected_toolset(self):
        from types import SimpleNamespace
        self.stage()
        dest = Path('dist/macos-arm64/tools/x86')
        c.x86(SimpleNamespace(out=str(dest), check=False))
        self.assertEqual((dest / 'kernel-version.txt').read_text(), 'v0.2.1\n')
        for name in c.X86_TOOLS:
            self.assertEqual(c.digest(dest / name), self.record['files'][name])
        c.x86(SimpleNamespace(out=str(dest), check=True))
        # Staging never overwrites, and a changed, extra or missing file fails.
        with self.assertRaises(FileExistsError):
            c.x86(SimpleNamespace(out=str(dest), check=False))
        (dest / 'kernel.img').write_bytes(b'stable kernel')
        with self.assertRaisesRegex(ValueError, 'differs from the candidate toolset'):
            c.x86(SimpleNamespace(out=str(dest), check=True))
        shutil.copyfile('kernel-selected/kernel.img', dest / 'kernel.img')
        (dest / 'kernel-version.txt').write_text('v0.2.0\n')
        with self.assertRaisesRegex(ValueError, 'version differs'):
            c.x86(SimpleNamespace(out=str(dest), check=True))
        (dest / 'kernel-version.txt').write_text('v0.2.1\n')
        (dest / 'extra').write_bytes(b'')
        with self.assertRaisesRegex(ValueError, 'Unexpected x86'):
            c.x86(SimpleNamespace(out=str(dest), check=True))
        # The selected toolset itself is checked against the lock first.
        Path('kernel-selected/boot.img').write_bytes(b'changed')
        with self.assertRaisesRegex(ValueError, 'bytes changed'):
            c.x86(SimpleNamespace(out='other', check=False))

    def test_candidate_signed_for_stable_is_not_promotable(self):
        self.assemble()
        m = json.loads(Path('candidate/manifest.json').read_text()); m['channel'] = 'stable'
        c.write_json('candidate/manifest.json', m)
        inv = json.loads(Path('candidate/inventory.json').read_text())
        inv['manifest'] = c.digest('candidate/manifest.json')
        c.write_json('candidate/inventory.json', inv)
        with patch.object(c, 'verify_signature'), self.assertRaisesRegex(ValueError, 'candidate channel'):
            c.verify_candidate('candidate')

    def test_rootfs_cannot_ship_another_kernel(self):
        _, manifest = self.assemble()
        manifest['components']['kernel']['files']['kernel.img']['sha256'] = '0' * 64
        with patch.object(c, 'run', return_value='jerboa v1.0.0\n'):
            with self.assertRaisesRegex(ValueError, 'Distro kernel differs'):
                c.verify_linux_versions(manifest, Path('candidate/payload'), 'v1.0.0')

    def test_promotion_rejects_kernel_downgrade_and_reused_version_before_writing(self):
        inv, manifest = self.assemble()
        self.evidence(inv)
        c.write_json('authorized-run.json', {'run_id': inv['run_id'], 'engine_sha': inv['engine_sha']})
        # A release promoted after this candidate was built overtakes the
        # floor of its binaries: nothing is written, and the message says what to do.
        old = {'generated': '2026-01-02T03:04:06Z', 'components': {'cli': {'version': 'v0.9.0'}, 'kernel': manifest['components']['kernel']}}
        c.write_json('old.json', old)
        with patch.object(c, 'verify_signature'), patch.object(c, 'existing', return_value=Path('old.json')), patch.object(c, 'put') as put:
            with self.assertRaisesRegex(ValueError, 'promoted after this candidate was built.*dispatch "Build release candidate" again'):
                c.promote()
            put.assert_not_called()
        # The native Mac report must be this candidate's, and a pass.
        for change, message in [({'candidate_run': '999'}, "candidate_run '999'"), ({'app_sha256': '0' * 64}, 'app_sha256'),
                                ({'version': 'v0.9.9'}, 'version'), ({'result': 'fail'}, "result 'fail'"), (None, 'No native Mac validation report')]:
            if change is None: Path('docs/releases/v1.0.0/native-macos.json').unlink()
            else: self.evidence(inv, **change)
            with self.subTest(evidence=change), patch.object(c, 'verify_signature'), patch.object(c, 'existing', return_value=None), patch.object(c, 'put') as put:
                with self.assertRaisesRegex(ValueError, message + '.*native-macos.sh'):
                    c.promote()
                put.assert_not_called()
        self.evidence(inv)
        for kernel_version, message in [('v0.2.2', 'kernel downgrade'), ('v0.2.1', 'different bytes')]:
            old = {'components': {'cli': {'version': 'v0.9.0'}, 'kernel': {'version': kernel_version, 'files': {}}}}
            c.write_json('old.json', old)
            with patch.object(c, 'verify_signature'), patch.object(c, 'existing', return_value=Path('old.json')), patch.object(c, 'put') as put:
                with self.assertRaisesRegex(ValueError, message):
                    c.promote()
                put.assert_not_called()

    def test_toolset_rejects_wrong_host_binary(self):
        self.stage()
        self.host_binaries()
        # Swapped artifacts: the Linux arm64 slot holds the macOS binary.
        shutil.copyfile(c.TOOLSET_MKFS['darwin-arm64'], c.TOOLSET_MKFS['linux-arm64'])
        with self.assertRaisesRegex(ValueError, 'not a linux-arm64 binary'):
            self.assemble_only()

    def test_tampered_toolset_document_is_rejected(self):
        self.assemble()
        doc = Path('candidate/payload/kernel/v0.2.1/toolset.json')
        data = json.loads(doc.read_text()); data['mkfs']['linux-arm64']['sha256'] = '0' * 64
        c.write_json(doc, data)
        inv = json.loads(Path('candidate/inventory.json').read_text())
        inv['assets']['kernel/v0.2.1/toolset.json'] = c.digest(doc)
        c.write_json('candidate/inventory.json', inv)
        with patch.object(c, 'verify_signature'), self.assertRaisesRegex(ValueError, 'does not match the candidate'):
            c.verify_candidate('candidate')


if __name__ == '__main__':
    unittest.main()
