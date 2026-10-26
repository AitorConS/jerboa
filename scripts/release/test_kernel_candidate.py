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
                     'run_id': '123', 'repository': 'AitorConS/jerboa'}
        c.write_json('release-lock.json', self.spec)
        Path('scripts').mkdir()
        Path('scripts/install.sh').write_text('#!/bin/sh\n# tested bootstrap\n')
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
                c.write_json(target, {'components': {'kernel': component}})
            elif url.endswith('.minisig'):
                Path(target).write_bytes(b'signature')
            else:
                shutil.copyfile(file, target)
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
                p = Path(folder) / name; p.parent.mkdir(exist_ok=True); p.write_bytes(name.encode())
        Path('desktop-windows/jerboa-desktop-setup-1.0.0.exe.blockmap').write_bytes(b'blockmap')
        Path('native-macos').mkdir()
        Path('native-macos/jerboa-desktop-1.0.0-macos-arm64.zip').write_bytes(b'app')
        Path('distro').mkdir()
        with tarfile.open('distro/jerboa-rootfs-amd64.tar.gz', 'w:gz') as archive:
            for name, filename in c.KERNEL_FILES.items():
                archive.add(Path('kernel-candidate') / filename, arcname='root/.jerboa/tools/' + name)
            data = b'daemon'; info = tarfile.TarInfo('usr/local/bin/jerboad'); info.size = len(data)
            archive.addfile(info, io.BytesIO(data))
        with patch.object(c, 'sign'), patch.object(c, 'run', return_value='jerboa v1.0.0\n'), patch.object(c, 'download') as download:
            c.assemble()
            download.assert_not_called()
        with patch.object(c, 'verify_signature'):
            return c.verify_candidate('candidate')

    def test_assembly_and_inventory_preserve_tested_kernel_bytes(self):
        inv, manifest = self.assemble()
        # A built kernel ships its host mkfs and the toolset document naming them.
        doc = json.loads(Path(f'candidate/payload/kernel/v0.2.1/toolset.json').read_text())
        self.assertEqual(set(doc['mkfs']), {'linux-amd64', 'linux-arm64', 'darwin-arm64'})
        self.assertEqual(doc['mkfs']['linux-amd64'], manifest['components']['kernel']['files']['mkfs'])
        self.assertIn('kernel/v0.2.1/toolset.json', inv['assets'])
        self.assertIn('kernel/v0.2.1/mkfs-darwin-arm64', inv['assets'])
        self.assertEqual(inv['kernel_toolset'], self.record)
        self.assertEqual(manifest['components']['distro']['kernel'], 'v0.2.1')
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
        self.assertEqual(json.loads(store['channels/stable.json'])['components']['kernel'], manifest['components']['kernel'])
        switch = events.index(('put', 'channels/stable.json'))
        for filename in c.KERNEL_FILES.values():
            key = 'kernel/v0.2.1/' + filename
            self.assertEqual(store[key], Path('kernel-candidate', filename).read_bytes())
            self.assertLess(events.index(('get', key)), switch)

    def test_rootfs_cannot_ship_another_kernel(self):
        _, manifest = self.assemble()
        manifest['components']['kernel']['files']['kernel.img']['sha256'] = '0' * 64
        with patch.object(c, 'run', return_value='jerboa v1.0.0\n'):
            with self.assertRaisesRegex(ValueError, 'Distro kernel differs'):
                c.verify_linux_versions(manifest, Path('candidate/payload'), 'v1.0.0')

    def test_promotion_rejects_kernel_downgrade_and_reused_version_before_writing(self):
        inv, manifest = self.assemble()
        c.write_json('authorized-run.json', {'run_id': inv['run_id'], 'engine_sha': inv['engine_sha']})
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
