#!/usr/bin/env python3
"""Prepare the version bump of a release, for the pull request that precedes it.

  prepare-release.py 0.60.0
  prepare-release.py 0.60.0 --date 2026-12-01

In CHANGELOG.md it names the Unreleased section after the version
(`## [0.60.0] - YYYY-MM-DD`, today in UTC unless --date is given), opens a new
empty `## [Unreleased]` above it and adds the version's link definition after
the Unreleased one, in the form the newest release's link has. It writes the
version to VERSION.md. Existing entries are not touched.

The candidate workflow refuses a version without a changelog entry (see
candidate.py release_notes), and names this command when it does. Running it
twice for the same version changes nothing.
"""
import argparse
import datetime
import importlib.util
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DEFAULT_LINK = 'https://github.com/AitorConS/jerboa/tree/main/docs/releases/v{version}'


def load_changelog():
    spec = importlib.util.spec_from_file_location('changelog', Path(__file__).with_name('changelog.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def link_for(version, lines, links, newest):
    """The new version's link, shaped like the newest release's when that names its version."""
    if newest is not None and newest in links:
        url = lines[links[newest] - 1].split(':', 1)[1].strip()
        if url.count(newest) == 1:
            return url.replace(newest, version)
    return DEFAULT_LINK.format(version=version)


def prepare(text, version, date, changelog):
    """Return the changelog with Unreleased released as version, or None when it already is."""
    if not re.fullmatch(r'(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)', version):
        raise ValueError(f'{version!r} is not a version like 0.60.0 (no "v")')
    problems = changelog.check(text)
    if problems:
        raise ValueError('CHANGELOG.md is malformed; fix it first:\n  ' + '\n  '.join(problems))
    sections, links, _ = changelog.parse(text)
    released = [s for s in sections if s.label != changelog.UNRELEASED]
    if any(s.label == version for s in released):
        return None
    unreleased = sections[0]
    if not unreleased.text():
        raise ValueError('the Unreleased section is empty: there is nothing to release as ' + version)
    newest = released[0] if released else None
    if newest is not None:
        if changelog.version_key(version) <= changelog.version_key(newest.label):
            raise ValueError(f'{version} is not newer than {newest.label}, the newest version in the changelog')
        if newest.date and date < newest.date:
            raise ValueError(f'{date} is before {newest.date}, the date of {newest.label}')
    if changelog.UNRELEASED not in links:
        raise ValueError('CHANGELOG.md has no [Unreleased] link definition to add the new one after')
    lines = text.split('\n')
    link = f'[{version}]: {link_for(version, lines, links, newest.label if newest else None)}'
    # Bottom first, so the heading's line number is still right.
    lines.insert(links[changelog.UNRELEASED], link)
    lines[unreleased.line:unreleased.line] = ['', f'## [{version}] - {date.isoformat()}']
    result = '\n'.join(lines)
    problems = changelog.check(result)
    if problems:
        raise ValueError('the prepared changelog would be malformed; nothing was written:\n  ' + '\n  '.join(problems))
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description='Name the Unreleased changelog section after a version and write VERSION.md.')
    parser.add_argument('version', help='the version to release, X.Y.Z')
    parser.add_argument('--date', type=datetime.date.fromisoformat,
                        default=datetime.datetime.now(datetime.timezone.utc).date(),
                        help='UTC day of publication (default: today)')
    parser.add_argument('--root', type=Path, default=ROOT, help='repository to change (default: this one)')
    args = parser.parse_args(argv)
    log, version_file = args.root / 'CHANGELOG.md', args.root / 'VERSION.md'
    try:
        result = prepare(log.read_text(encoding='utf-8'), args.version, args.date, load_changelog())
    except (OSError, ValueError) as err:
        print(f'prepare-release: {err}', file=sys.stderr)
        return 1
    if result is None:
        print(f'CHANGELOG.md already has an entry for {args.version}')
    else:
        log.write_text(result, encoding='utf-8')
        print(f'CHANGELOG.md: Unreleased is now [{args.version}] - {args.date.isoformat()}')
    if not version_file.exists() or version_file.read_text().strip() != args.version:
        version_file.write_text(args.version + '\n')
        print(f'VERSION.md: {args.version}')
    print('Review the entry, open a pull request with both files and merge it before building the candidate.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
