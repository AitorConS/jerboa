#!/usr/bin/env python3
"""Validate CHANGELOG.md and extract the section of one version.

  changelog.py check                     structure of CHANGELOG.md
  changelog.py extract 0.60.0            that version's section, on stdout
  changelog.py extract 0.60.0 -o notes.md
  changelog.py extract unreleased

`extract` checks the structure first and fails when the version has no
section or an empty one, so a release step that calls it both gets its release
notes and refuses a version nobody wrote an entry for.

The format is Keep a Changelog 1.1.0 (https://keepachangelog.com/en/1.1.0/):

  # Changelog
  ## [Unreleased]
  ## [1.2.3] - 2026-01-31
  ### Added | Changed | Deprecated | Removed | Fixed | Security
  [1.2.3]: https://...

Rules checked: the title; `## [Unreleased]` first and only once; every other
second-level heading is `## [X.Y.Z] - YYYY-MM-DD`, newest first, with a real
date that is not later than the one above it; third-level headings are the six
above, once each per version, and none is empty; every released version says
something; and every heading has its link definition at the end of the file.
"""
import argparse
import datetime
import re
import sys
from pathlib import Path

DEFAULT_FILE = Path(__file__).resolve().parents[2] / 'CHANGELOG.md'
CATEGORIES = ('Added', 'Changed', 'Deprecated', 'Removed', 'Fixed', 'Security')
UNRELEASED = 'Unreleased'

VERSION = r'(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?'
RELEASE_HEADING = re.compile(rf'^## \[({VERSION})\] - (\d{{4}}-\d{{2}}-\d{{2}})( \[YANKED\])?$')
LINK_DEFINITION = re.compile(r'^\[([^\]]+)\]:\s+(\S+)\s*$')
FENCE = re.compile(r'^\s*(```|~~~)')


class ChangelogError(Exception):
    """The changelog is malformed, or has no usable section for a version."""


class Section:
    def __init__(self, label, line, date=None):
        self.label, self.line, self.date = label, line, date
        self.body = []        # lines after the heading, as written
        self.categories = []  # (name, line, has_content)

    def text(self):
        return '\n'.join(self.body).strip('\n')


def version_key(version):
    """Order versions; a pre-release sorts before its release."""
    core, _, pre = version.partition('-')
    return tuple(int(part) for part in core.split('.')), pre == '', pre


def parse(text):
    """Return (sections, link definitions by label, errors as (line, message))."""
    sections, links, errors = [], {}, []
    current, fenced, title = None, False, None
    for number, line in enumerate(text.splitlines(), 1):
        if FENCE.match(line):
            fenced = not fenced
        heading = None if fenced else re.match(r'^(#{1,3}) ', line)
        if title is None and line.strip():
            title = line
            if line != '# Changelog':
                errors.append((number, 'the file must start with "# Changelog"'))
            continue
        if heading and len(heading.group(1)) == 1:
            errors.append((number, 'only the title may be a first-level heading'))
        elif heading and len(heading.group(1)) == 2:
            current = section_for(line, number, errors)
            sections.append(current)
            continue
        elif heading and current is not None:
            name = line[4:].strip()
            if name not in CATEGORIES:
                errors.append((number, f'"### {name}" is not one of: {", ".join(CATEGORIES)}'))
            elif name in [c[0] for c in current.categories]:
                errors.append((number, f'"### {name}" appears twice in [{current.label}]'))
            current.categories.append([name, number, False])
        elif heading:
            errors.append((number, 'a third-level heading must come under a version'))
        definition = None if fenced else LINK_DEFINITION.match(line)
        if definition:
            label = definition.group(1)
            if label in links:
                errors.append((number, f'link [{label}] is defined twice'))
            links[label] = number
            continue  # definitions belong to the file, not to the last section
        if current is not None:
            current.body.append(line)
            if line.strip() and not heading and current.categories:
                current.categories[-1][2] = True
    if title is None:
        errors.append((1, 'the file is empty'))
    return sections, links, errors


def section_for(line, number, errors):
    if line == f'## [{UNRELEASED}]':
        return Section(UNRELEASED, number)
    match = RELEASE_HEADING.match(line)
    if not match:
        errors.append((number, 'a version heading must read "## [X.Y.Z] - YYYY-MM-DD" or "## [Unreleased]"'))
        return Section(line[3:].strip(), number)
    try:
        date = datetime.date.fromisoformat(match.group(2))
    except ValueError:
        errors.append((number, f'{match.group(2)} is not a date'))
        date = None
    return Section(match.group(1), number, date)


def check(text):
    """Return every structural problem as "line N: message", in file order."""
    sections, links, errors = parse(text)
    malformed = {line for line, _ in errors}
    if not sections or sections[0].label != UNRELEASED:
        errors.append((sections[0].line if sections else 1, f'the first section must be "## [{UNRELEASED}]"'))
    seen, previous = set(), None
    for section in sections:
        if section.label in seen:
            errors.append((section.line, f'[{section.label}] appears twice'))
        seen.add(section.label)
        for name, line, has_content in section.categories:
            if not has_content:
                errors.append((line, f'"### {name}" in [{section.label}] is empty'))
        if section.label not in links and section.line not in malformed:
            errors.append((section.line, f'[{section.label}] has no link definition; add "[{section.label}]: <url>" at the end of the file'))
        if section.label == UNRELEASED or section.line in malformed:
            continue
        if not section.text():
            errors.append((section.line, f'[{section.label}] has no content'))
        if previous is not None:
            if version_key(section.label) >= version_key(previous.label):
                errors.append((section.line, f'[{section.label}] must come before [{previous.label}]: newest first'))
            if section.date and previous.date and section.date > previous.date:
                errors.append((section.line, f'[{section.label}] is dated after the newer [{previous.label}]'))
        previous = section
    for label, line in links.items():
        if (label == UNRELEASED or re.fullmatch(VERSION, label)) and label not in seen:
            errors.append((line, f'link [{label}] has no section'))
    return [f'line {line}: {message}' for line, message in sorted(errors)]


def extract(text, version):
    """Return the body of one version's section, without its heading."""
    problems = check(text)
    if problems:
        raise ChangelogError('the changelog is malformed:\n  ' + '\n  '.join(problems))
    wanted = UNRELEASED if version.lower() == UNRELEASED.lower() else version.removeprefix('v')
    for section in parse(text)[0]:
        if section.label == wanted:
            if not section.text():
                raise ChangelogError(f'[{wanted}] has no content')
            return section.text() + '\n'
    raise ChangelogError(f'no entry for {wanted}: add "## [{wanted}] - YYYY-MM-DD" under "## [{UNRELEASED}]"')


def main(argv=None):
    parser = argparse.ArgumentParser(description='Validate CHANGELOG.md and extract the section of one version.')
    parser.add_argument('--file', type=Path, default=DEFAULT_FILE, help='changelog to read (default: the repository\'s)')
    commands = parser.add_subparsers(dest='command', required=True)
    commands.add_parser('check', help='check the structure')
    extractor = commands.add_parser('extract', help='print the section of one version')
    extractor.add_argument('version', help='X.Y.Z, vX.Y.Z or "unreleased"')
    extractor.add_argument('-o', '--output', type=Path, help='write the section here instead of stdout')
    args = parser.parse_args(argv)
    try:
        text = args.file.read_text(encoding='utf-8')
    except OSError as err:
        print(f'changelog: {err}', file=sys.stderr)
        return 1
    if args.command == 'check':
        problems = check(text)
        for problem in problems:
            print(f'{args.file}: {problem}', file=sys.stderr)
        return 1 if problems else 0
    try:
        body = extract(text, args.version)
    except ChangelogError as err:
        print(f'changelog: {err}', file=sys.stderr)
        return 1
    if args.output:
        args.output.write_text(body, encoding='utf-8')
    else:
        sys.stdout.write(body)
    return 0


if __name__ == '__main__':
    sys.exit(main())
