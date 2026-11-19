#!/usr/bin/env python3
"""Bind workflow artifacts to the job that produced them.

Artifacts of one workflow run share a namespace that every job of the run,
including those on the self-hosted KVM runner, can write to, and consumers
fetch them by name. The producing job therefore records a digest of exactly
what it uploads as a job output, which no other job can change, and each
trusted consumer checks the downloaded files against it before using them.

  artifact-digest.py record OUTPUT_NAME PATH...   append OUTPUT_NAME=<digest> to $GITHUB_OUTPUT
  artifact-digest.py verify EXPECTED PATH...      fail unless PATH... has that digest
  artifact-digest.py copy SRC DEST [NAME...]       copy a downloaded artifact into place

Artifacts are downloaded into a directory of their own outside the checkout
(${{ runner.temp }}/artifacts/<name>), so no artifact can replace a script,
workflow or source file a later step runs; copy then puts its regular files
under DEST, relative to the working directory. With NAMEs, SRC must hold
exactly those files. It never overwrites a file, never writes outside the
working directory or into what the job runs or builds from (PROTECTED), and
refuses symlinks and special files.

A PATH that is a file contributes one entry named after its basename (how
upload-artifact stores a single file, or files of one directory); a directory
contributes every file below it by its relative path (how upload-artifact
stores a directory, and how download-artifact lays it out again). Symlinks
and anything other than regular files and directories are refused.
"""
import hashlib
import os
from pathlib import Path
import re
import shutil
import sys

# Top-level entries of a checkout that an artifact may never be copied into.
PROTECTED = {'.git', '.github', 'scripts', 'cmd', 'internal', 'kernel', 'distro', 'tests',
             'bin', 'go.mod', 'go.sum', 'Makefile'}
PROTECTED_FOLDED = {p.casefold() for p in PROTECTED}


def entries(paths):
    out = {}
    def add(name, path):
        if name in out:
            raise ValueError('Duplicate artifact entry: ' + name)
        h = hashlib.sha256()
        with path.open('rb') as f:
            while b := f.read(1024 * 1024):
                h.update(b)
        out[name] = (h.hexdigest(), path.stat().st_size)
    for raw in paths:
        root = Path(raw)
        if root.is_symlink():
            raise ValueError('Artifact path is a symlink: ' + raw)
        if root.is_file():
            add(root.name, root)
        elif root.is_dir():
            for p in sorted(root.rglob('*')):
                if p.is_symlink() or not (p.is_file() or p.is_dir()):
                    raise ValueError('Unexpected artifact entry: ' + str(p))
                if p.is_file():
                    add(p.relative_to(root).as_posix(), p)
        else:
            raise ValueError('Artifact path missing: ' + raw)
    if not out:
        raise ValueError('Artifact is empty')
    return out


def tree_digest(paths):
    h = hashlib.sha256()
    for name, (sha, size) in sorted(entries(paths).items()):
        h.update(f'{name}\0{sha}\0{size}\n'.encode())
    return h.hexdigest()


def record(name, paths):
    if not re.fullmatch(r'[A-Za-z0-9_]+', name):
        raise ValueError('Invalid output name: ' + name)
    value = tree_digest(paths)
    with open(os.environ['GITHUB_OUTPUT'], 'a') as out:
        out.write(f'{name}={value}\n')
    print(f'{name}={value}')


def verify(expected, paths):
    # An empty expectation means the producer's output did not arrive: fail
    # closed rather than accept whatever the artifact now holds.
    if not re.fullmatch(r'[0-9a-f]{64}', expected):
        raise ValueError('Missing or malformed expected artifact digest')
    actual = tree_digest(paths)
    if actual != expected:
        raise ValueError(f'Artifact differs from what its producing job uploaded: {" ".join(paths)}')
    print('Verified', ' '.join(paths), actual)


def copy(src, dest, names=()):
    src = Path(src)
    if src.is_symlink() or not src.is_dir():
        raise ValueError('Artifact directory missing: ' + str(src))
    files = {}
    for p in sorted(src.rglob('*')):
        if p.is_symlink() or not (p.is_file() or p.is_dir()):
            raise ValueError('Unexpected artifact entry: ' + str(p))
        if p.is_file():
            files[p.relative_to(src).as_posix()] = p
    if not files:
        raise ValueError('Artifact is empty: ' + str(src))
    if names and set(files) != set(names):
        raise ValueError(f'Artifact {src} holds {sorted(files)}, expected exactly {sorted(names)}')
    cwd = Path.cwd().resolve()
    targets = {}
    for name in files:
        if '..' in Path(name).parts:
            raise ValueError(f'Refusing artifact entry {name}')
        target = (Path(dest) / name).resolve()
        try:
            rel = target.relative_to(cwd)
        except ValueError:
            raise ValueError(f'Refusing to copy {name} outside the working directory: {target}') from None
        # Case-folded: on NTFS and APFS, Scripts/ is scripts/.
        if not rel.parts or rel.parts[0].casefold() in PROTECTED_FOLDED:
            raise ValueError(f'Refusing to copy {name} into {rel.as_posix() or "."}')
        if target.exists() or target.is_symlink():
            raise ValueError(f'Refusing to overwrite {rel.as_posix()}')
        targets[name] = target
    for name, target in targets.items():
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(files[name], target)
        print('Copied', name, 'to', target.relative_to(cwd).as_posix())


def main(argv):
    if len(argv) < 3 or argv[0] not in ('record', 'verify', 'copy'):
        raise SystemExit(__doc__)
    try:
        if argv[0] == 'copy':
            copy(argv[1], argv[2], argv[3:])
        else:
            (record if argv[0] == 'record' else verify)(argv[1], argv[2:])
    except ValueError as e:
        raise SystemExit('error: ' + str(e))


if __name__ == '__main__':
    main(sys.argv[1:])
