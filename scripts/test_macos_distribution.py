#!/usr/bin/env python3
"""Security regressions for macOS distribution handling."""

from __future__ import annotations

import importlib.util
import hashlib
import io
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import unittest


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent


def load_script(name: str):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), SCRIPT_DIR / name)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader
    spec.loader.exec_module(module)
    return module


VERIFY = load_script("verify-macos-distribution.py")
SIGNING = load_script("update-macos-signing-metadata.py")
PACKAGE = load_script("package-macos-distribution.py")


class ExtractSecurityTests(unittest.TestCase):
    def archive(self, members):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        path = pathlib.Path(temporary.name) / "input.tar.gz"
        with tarfile.open(path, "w:gz") as output:
            for info, content in members:
                output.addfile(info, io.BytesIO(content) if content is not None else None)
        return path, pathlib.Path(temporary.name) / "extract"

    @staticmethod
    def directory(name):
        info = tarfile.TarInfo(name)
        info.type = tarfile.DIRTYPE
        return info, None

    @staticmethod
    def file(name, content=b"ok"):
        info = tarfile.TarInfo(name)
        info.size = len(content)
        return info, content

    def rejected_without_extraction(self, members, message):
        archive, target = self.archive(members)
        target.mkdir()
        sentinel = target / "sentinel"
        sentinel.write_text("untouched")
        with self.assertRaisesRegex(SystemExit, message):
            VERIFY.safe_extract(archive, target)
        self.assertEqual(list(target.iterdir()), [sentinel])
        self.assertEqual(sentinel.read_text(), "untouched")

    def test_fifo_rejected(self):
        fifo = tarfile.TarInfo("root/fifo")
        fifo.type = tarfile.FIFOTYPE
        self.rejected_without_extraction([self.directory("root"), (fifo, None)], "not a regular")

    def test_device_rejected(self):
        device = tarfile.TarInfo("root/device")
        device.type = tarfile.CHRTYPE
        self.rejected_without_extraction([self.directory("root"), (device, None)], "not a regular")

    def test_symlink_rejected(self):
        link = tarfile.TarInfo("root/link")
        link.type = tarfile.SYMTYPE
        link.linkname = "/tmp/outside"
        self.rejected_without_extraction([self.directory("root"), (link, None)], "not a regular")

    def test_duplicate_rejected(self):
        self.rejected_without_extraction(
            [self.directory("root"), self.file("root/file", b"one"), self.file("root/file", b"two")],
            "duplicate",
        )

    def test_empty_path_rejected(self):
        self.rejected_without_extraction([(tarfile.TarInfo(""), b"")], "empty")

    def test_parent_path_rejected(self):
        self.rejected_without_extraction([self.directory("root"), self.file("root/../outside")], "unsafe")


class TreeSecurityTests(unittest.TestCase):
    def test_file_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            (root / "real").write_text("data")
            (root / "link").symlink_to(root / "real")
            with self.assertRaisesRegex(SystemExit, "symlink"):
                VERIFY.validate_regular_tree(root)

    def test_directory_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary) / "bundle"
            outside = pathlib.Path(temporary) / "outside"
            root.mkdir()
            outside.mkdir()
            (root / "linkdir").symlink_to(outside, target_is_directory=True)
            with self.assertRaisesRegex(SystemExit, "symlink"):
                VERIFY.validate_regular_tree(root)

    def test_checksum_parent_rejected_before_read(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary) / "bundle"
            root.mkdir()
            outside = pathlib.Path(temporary) / "outside"
            outside.write_text("secret")
            (root / "SHA256SUMS").write_text("0" * 64 + "  ../outside\n")
            with self.assertRaisesRegex(SystemExit, "invalid checksum"):
                VERIFY.verify_checksums(root)

    def test_checksum_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary) / "bundle"
            root.mkdir()
            outside = pathlib.Path(temporary) / "outside"
            outside.write_text("secret")
            (root / "link").symlink_to(outside)
            (root / "SHA256SUMS").write_text(hashlib.sha256(b"secret").hexdigest() + "  link\n")
            with self.assertRaisesRegex(SystemExit, "escapes bundle"):
                VERIFY.verify_checksums(root)

    def test_packaging_input_directory_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary) / "input"
            outside = pathlib.Path(temporary) / "outside"
            root.mkdir()
            outside.mkdir()
            (root / "linkdir").symlink_to(outside, target_is_directory=True)
            with self.assertRaisesRegex(SystemExit, "symlink"):
                PACKAGE.validate_input_tree(root, "test input")

    def test_different_installer_payload_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = pathlib.Path(temporary)
            bundle = base / "bundle"
            installed = base / "payload/usr/local/libexec/jerboa"
            bundle.mkdir()
            installed.mkdir(parents=True)
            (bundle / "binary").write_bytes(b"reviewed")
            (installed / "binary").write_bytes(b"different")
            with self.assertRaisesRegex(SystemExit, "bytes differ"):
                VERIFY.compare_installer_payload(bundle, base / "payload")

    @unittest.skipUnless(shutil.which("pkgbuild") and shutil.which("pkgutil"), "macOS package tools required")
    def test_pkgbuild_payload_matches_bundle(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = pathlib.Path(temporary)
            bundle = base / "bundle"
            bundle.mkdir()
            (bundle / "bin").mkdir()
            executable = bundle / "bin/tool"
            executable.write_bytes(b"tool")
            executable.chmod(0o755)
            (bundle / "metadata").write_text("reviewed")
            package_root = base / "root/usr/local/libexec"
            package_root.mkdir(parents=True)
            shutil.copytree(bundle, package_root / "jerboa")
            package = base / "probe.pkg"
            subprocess.run(
                ["pkgbuild", "--root", str(base / "root"), "--identifier", "dev.jerboa.payload-test",
                 "--version", "0.0.0", str(package)], check=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            )
            expanded = base / "expanded"
            subprocess.run(["pkgutil", "--expand-full", str(package), str(expanded)], check=True)
            self.assertEqual(VERIFY.compare_installer_payload(bundle, expanded / "Payload"), 2)


REPRO = load_script("build-macos-reproducible.py")


class ReproducibleBuildTests(unittest.TestCase):
    def snapshot_archive(self, directory, members):
        path = pathlib.Path(directory) / "source-snapshot.tar"
        with tarfile.open(path, "w") as output:
            for info, content in members:
                output.addfile(info, io.BytesIO(content) if content is not None else None)
        return path

    @staticmethod
    def member(name, content=b"x", mode=0o644):
        info = tarfile.TarInfo(name)
        info.size = len(content)
        info.mode = mode
        return info, content

    def test_manifest_ignores_member_order_and_mtime(self):
        with tempfile.TemporaryDirectory() as one, tempfile.TemporaryDirectory() as two:
            a = self.member("jerboa/a", b"one")
            b = self.member("jerboa/b", b"two", 0o755)
            first = self.snapshot_archive(one, [a, b])
            late = self.member("jerboa/a", b"one")
            late[0].mtime = 12345
            second = self.snapshot_archive(two, [b, late])
            self.assertEqual(REPRO.manifest_lines(first), REPRO.manifest_lines(second))
            self.assertIn(" f 755 jerboa/b", REPRO.manifest_lines(first)[1])

    def test_manifest_detects_dirty_content_change(self):
        with tempfile.TemporaryDirectory() as one, tempfile.TemporaryDirectory() as two:
            first = self.snapshot_archive(one, [self.member("jerboa/a", b"clean")])
            second = self.snapshot_archive(two, [self.member("jerboa/a", b"dirty")])
            self.assertNotEqual(REPRO.tree_digest(REPRO.manifest_lines(first)),
                                REPRO.tree_digest(REPRO.manifest_lines(second)))

    def test_snapshot_verification_rejects_tampering(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            self.snapshot_archive(directory, [self.member("jerboa/a", b"frozen")])
            REPRO.finish_snapshot(root)
            self.assertEqual(REPRO.verify_snapshot(root), (root / "tree-digest.txt").read_text().strip())
            (root / "tree-digest.txt").write_text("0" * 64 + "\n")
            with self.assertRaisesRegex(SystemExit, "does not match"):
                REPRO.verify_snapshot(root)

    def test_snapshot_extraction_rejects_escapes(self):
        cases = []
        parent = self.member("jerboa/../outside")
        cases.append(([parent], "unsafe snapshot member"))
        cases.append(([self.member("other/file")], "unexpected snapshot root"))
        link = tarfile.TarInfo("jerboa/link")
        link.type = tarfile.SYMTYPE
        link.linkname = "../../outside"
        cases.append(([(link, None)], "unsafe snapshot link"))
        absolute = tarfile.TarInfo("jerboa/abs")
        absolute.type = tarfile.SYMTYPE
        absolute.linkname = "/etc/passwd"
        cases.append(([(absolute, None)], "unsafe snapshot link"))
        fifo = tarfile.TarInfo("jerboa/fifo")
        fifo.type = tarfile.FIFOTYPE
        cases.append(([(fifo, None)], "unsupported"))
        for members, message in cases:
            with self.subTest(message=message), tempfile.TemporaryDirectory() as directory:
                archive = self.snapshot_archive(directory, members)
                target = pathlib.Path(directory) / "extract"
                target.mkdir()
                with self.assertRaisesRegex(SystemExit, message):
                    REPRO.safe_extract_snapshot(archive, target)
                self.assertEqual(list(target.iterdir()), [])

    def test_snapshot_relative_link_inside_tree_allowed(self):
        with tempfile.TemporaryDirectory() as directory:
            link = tarfile.TarInfo("firecracker-macos/fuzz/IN")
            link.type = tarfile.SYMTYPE
            link.linkname = "../corpus"
            archive = self.snapshot_archive(directory, [self.member("firecracker-macos/corpus"), (link, None)])
            target = pathlib.Path(directory) / "extract"
            target.mkdir()
            REPRO.safe_extract_snapshot(archive, target)
            self.assertTrue((target / "firecracker-macos/fuzz/IN").is_symlink())

    def test_snapshot_chained_links_cannot_escape(self):
        with tempfile.TemporaryDirectory() as directory:
            links = []
            for name, destination in (("jerboa/a", ".."), ("jerboa/b", "a/..")):
                link = tarfile.TarInfo(name)
                link.type = tarfile.SYMTYPE
                link.linkname = destination
                links.append((link, None))
            archive = self.snapshot_archive(directory, links + [self.member("jerboa/b/escaped")])
            target = pathlib.Path(directory) / "extract"
            target.mkdir()
            with self.assertRaisesRegex(SystemExit, "unsafe snapshot"):
                REPRO.safe_extract_snapshot(archive, target)
            self.assertFalse((pathlib.Path(directory) / "escaped").exists())
            self.assertEqual(list(target.iterdir()), [])

    def test_snapshot_rejects_link_parents_and_duplicates(self):
        link = tarfile.TarInfo("jerboa/link")
        link.type = tarfile.SYMTYPE
        link.linkname = "directory"
        for members in (
            [(link, None), self.member("jerboa/link/file")],
            [self.member("jerboa/file"), self.member("jerboa/file")],
        ):
            with tempfile.TemporaryDirectory() as directory:
                archive = self.snapshot_archive(directory, members)
                target = pathlib.Path(directory) / "extract"
                target.mkdir()
                with self.assertRaises(SystemExit):
                    REPRO.safe_extract_snapshot(archive, target)
                self.assertEqual(list(target.iterdir()), [])

    def test_snapshot_rejects_preexisting_destination(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = self.snapshot_archive(directory, [self.member("jerboa/file")])
            target = pathlib.Path(directory) / "extract"
            target.mkdir()
            (target / "jerboa").symlink_to("..")
            with self.assertRaisesRegex(SystemExit, "empty, non-symlink"):
                REPRO.safe_extract_snapshot(archive, target)
            self.assertFalse((pathlib.Path(directory) / "file").exists())

    @staticmethod
    def record(root, files, drift=(), archive="a"):
        return {"build_root": root, "version": "v", "toolchain_drift": list(drift),
                "snapshot": {"tree_digest": "t"}, "recipe_sha256": {"r": "1"},
                "archive_sha256": archive, "components": {"bundle": files}}

    def test_compare_equal_independent_builds(self):
        files = {"bin/x": {"sha256": "s", "unsigned_sha256": "u", "mode": "0o755"}}
        report = REPRO.compare_records(self.record("/a", files), self.record("/b", dict(files)))
        self.assertTrue(report["reproducible"])
        self.assertEqual(report["macho_files"], ["bundle/bin/x"])

    def test_compare_distinguishes_signature_only_difference(self):
        left = {"bin/x": {"sha256": "s", "unsigned_sha256": "u", "mode": "0o755"}}
        right = {"bin/x": {"sha256": "other", "unsigned_sha256": "u", "mode": "0o755"}}
        report = REPRO.compare_records(self.record("/a", left), self.record("/b", right))
        self.assertFalse(report["reproducible"])
        self.assertEqual([d["kind"] for d in report["differences"]], ["sha256"])

    def test_compare_refuses_same_root_or_drift(self):
        files = {"bin/x": {"sha256": "s", "unsigned_sha256": "u", "mode": "0o755"}}
        self.assertFalse(REPRO.compare_records(self.record("/a", files), self.record("/a", files))["reproducible"])
        drifted = self.record("/b", files, drift=["files.clt/clang: locked=1 actual=2"])
        self.assertFalse(REPRO.compare_records(self.record("/a", files), drifted)["reproducible"])

    def test_compare_reports_missing_file(self):
        files = {"bin/x": {"sha256": "s", "unsigned_sha256": "u", "mode": "0o755"}}
        report = REPRO.compare_records(self.record("/a", files), self.record("/b", {}))
        self.assertEqual(report["differences"][0]["kind"], "missing")
        self.assertFalse(report["reproducible"])

    def test_provenance_with_host_path_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "provenance.json"
            path.write_text('{"build_root": "/private/tmp/build-a"}')
            with self.assertRaisesRegex(SystemExit, "host path"):
                PACKAGE.load_provenance(path)
            path.write_text('{"source_snapshot_tree_digest": "abc"}')
            self.assertEqual(PACKAGE.load_provenance(path), {"source_snapshot_tree_digest": "abc"})

    def test_unsigned_hash_ignores_adhoc_signature(self):
        if not (shutil.which("clang") and shutil.which("codesign")):
            self.skipTest("clang/codesign required")
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            (base / "main.c").write_text("int main(void){return 0;}\n")
            binary = base / "probe"
            subprocess.run(["clang", "-o", str(binary), str(base / "main.c")], check=True)
            other = base / "probe-other"
            shutil.copyfile(binary, other)
            # Same signing path, different signature content: pre-signature bytes must match.
            for path, identifier in ((binary, "one.identity"), (other, "another.identity")):
                subprocess.run(["codesign", "--force", "--sign", "-", "--timestamp=none", "--identifier", identifier,
                                str(path)], check=True, capture_output=True)
            self.assertNotEqual(hashlib.sha256(binary.read_bytes()).digest(), hashlib.sha256(other.read_bytes()).digest())
            self.assertEqual(REPRO.unsigned_sha256(binary), REPRO.unsigned_sha256(other))
            self.assertTrue(REPRO.is_macho(binary))
            self.assertFalse(REPRO.is_macho(base / "main.c"))


class SigningMetadataTests(unittest.TestCase):
    def test_upstream_hash_metadata_is_renamed_and_marked(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            metadata = root / "share/firecracker"
            metadata.mkdir(parents=True)
            (metadata / "manifest.json").write_text("{}")
            (metadata / "SHA256SUMS").write_text("old")
            (root / "distribution-manifest.json").write_text('{"version":"test"}')
            (root / "SHA256SUMS").write_text("old-root")
            old_argv = SIGNING.sys.argv
            self.addCleanup(setattr, SIGNING.sys, "argv", old_argv)
            SIGNING.sys.argv = ["update-macos-signing-metadata.py", str(root)]
            SIGNING.main()
            self.assertFalse((metadata / "manifest.json").exists())
            self.assertFalse((metadata / "SHA256SUMS").exists())
            self.assertTrue((metadata / "manifest.upstream-adhoc.json").is_file())
            self.assertTrue((metadata / "SHA256SUMS.upstream-adhoc").is_file())
            self.assertIn("pre-Developer-ID", (metadata / "SIGNING-NOTICE.json").read_text())
            self.assertIn("actual_signing", (root / "distribution-manifest.json").read_text())


if __name__ == "__main__":
    unittest.main()
