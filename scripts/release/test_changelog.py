"""The changelog keeps its structure, and a version without an entry is refused."""
import contextlib,importlib.util,io,tempfile,unittest
from pathlib import Path
HERE=Path(__file__).resolve().parent
ROOT=HERE.parents[1]
spec=importlib.util.spec_from_file_location('changelog',HERE/'changelog.py')
c=importlib.util.module_from_spec(spec);spec.loader.exec_module(c)

GOOD='''# Changelog

Preamble.

## [Unreleased]

### Added

- A thing not released yet (#3).

## [1.1.0] - 2026-02-10

Read this before upgrading.

### Changed

- **Breaking:** something (#2).

```sh
## not a heading
```

### Fixed

- A bug.

## [1.0.0] - 2026-01-31

### Added

- First release (#1).

[Unreleased]: https://example.test/commits/main
[1.1.0]: https://example.test/v1.1.0
[1.0.0]: https://example.test/v1.0.0
'''

class ChangelogTests(unittest.TestCase):
    def problems(self,old,new):
        self.assertIn(old,GOOD)
        return '\n'.join(c.check(GOOD.replace(old,new)))

    def test_well_formed_file_passes(self):
        self.assertEqual(c.check(GOOD),[])
        self.assertEqual(c.check(GOOD.replace('### Added\n\n- A thing not released yet (#3).\n\n','')),[],'Unreleased may be empty')
        self.assertEqual(self.problems('## [1.0.0] - 2026-01-31','## [1.0.0] - 2026-01-31 [YANKED]'),'')

    def test_title_and_unreleased_are_required(self):
        self.assertRegex(self.problems('# Changelog','# Release notes'),'line 1: .*# Changelog')
        self.assertRegex(self.problems('## [Unreleased]\n','## [2.0.0] - 2026-03-01\n\nText.\n'),'first section must be')
        self.assertRegex(c.check('')[0],'empty')
        self.assertRegex(self.problems('## [1.1.0] - 2026-02-10','## [Unreleased]'),'appears twice')

    def test_version_headings(self):
        for heading in ['## 1.1.0 - 2026-02-10','## [1.1.0]','## [v1.1.0] - 2026-02-10','## [1.1] - 2026-02-10','## [1.1.0] - 10/02/2026','## [1.1.0] – 2026-02-10']:
            with self.subTest(heading=heading):
                self.assertRegex(self.problems('## [1.1.0] - 2026-02-10',heading),'line 11: a version heading must read')
        self.assertRegex(self.problems('2026-02-10','2026-02-30'),'2026-02-30 is not a date')

    def test_newest_first(self):
        self.assertRegex(self.problems('## [1.1.0] - 2026-02-10','## [0.9.0] - 2026-02-10'),r'\[1.0.0\] must come before \[0.9.0\]')
        self.assertRegex(self.problems('## [1.1.0] - 2026-02-10','## [1.0.0] - 2026-02-10'),'appears twice')
        self.assertRegex(self.problems('2026-02-10','2026-01-01'),r'\[1.0.0\] is dated after the newer \[1.1.0\]')
        # 0.10.0 is newer than 0.9.0, and a release is newer than its pre-release.
        self.assertLess(c.version_key('0.9.0'),c.version_key('0.10.0'))
        self.assertLess(c.version_key('1.0.0-rc.1'),c.version_key('1.0.0'))

    def test_categories(self):
        self.assertRegex(self.problems('### Fixed','### Bug fixes'),'"### Bug fixes" is not one of')
        self.assertRegex(self.problems('### Fixed','### Changed'),r'"### Changed" appears twice in \[1.1.0\]')
        self.assertRegex(self.problems('### Fixed\n\n- A bug.\n','### Fixed\n'),r'"### Fixed" in \[1.1.0\] is empty')
        self.assertRegex(self.problems('## [Unreleased]\n','### Added\n\n- Early.\n\n## [Unreleased]\n'),'must come under a version')
        self.assertRegex(self.problems('## [1.0.0] - 2026-01-31','# Older'),'only the title')

    def test_released_versions_say_something(self):
        self.assertRegex(self.problems('### Added\n\n- First release (#1).\n',''),r'\[1.0.0\] has no content')

    def test_link_definitions(self):
        self.assertRegex(self.problems('[1.1.0]: https://example.test/v1.1.0\n',''),r'line 11: \[1.1.0\] has no link definition')
        self.assertRegex(self.problems('[1.0.0]: https://example.test/v1.0.0\n','[1.0.0]: https://example.test/v1.0.0\n[0.9.0]: https://example.test/v0.9.0\n'),r'link \[0.9.0\] has no section')
        self.assertRegex(self.problems('[1.0.0]: https://example.test/v1.0.0\n','[1.0.0]: https://example.test/v1.0.0\n[1.0.0]: https://example.test/other\n'),'defined twice')
        # Other reference links are the author's business.
        self.assertEqual(self.problems('[1.0.0]: https://example.test/v1.0.0\n','[1.0.0]: https://example.test/v1.0.0\n[guide]: https://example.test/guide\n'),'')

    def test_extract(self):
        body=c.extract(GOOD,'1.1.0')
        self.assertTrue(body.startswith('Read this before upgrading.\n\n### Changed\n'))
        self.assertTrue(body.endswith('### Fixed\n\n- A bug.\n'))
        self.assertIn('## not a heading',body,'a fenced line is content, not a heading')
        self.assertEqual(c.extract(GOOD,'v1.1.0'),body)
        # The last section stops before the link definitions.
        self.assertEqual(c.extract(GOOD,'1.0.0'),'### Added\n\n- First release (#1).\n')
        self.assertEqual(c.extract(GOOD,'unreleased'),'### Added\n\n- A thing not released yet (#3).\n')

    def test_extract_refuses(self):
        with self.assertRaisesRegex(c.ChangelogError,'no entry for 1.2.0'):
            c.extract(GOOD,'1.2.0')
        with self.assertRaisesRegex(c.ChangelogError,'no entry for 1.1'):
            c.extract(GOOD,'1.1')
        with self.assertRaisesRegex(c.ChangelogError,r'\[Unreleased\] has no content'):
            c.extract(GOOD.replace('### Added\n\n- A thing not released yet (#3).\n\n',''),'Unreleased')
        with self.assertRaisesRegex(c.ChangelogError,'malformed'):
            c.extract(GOOD.replace('### Fixed','### Bug fixes'),'1.0.0')

    def test_command_line(self):
        with tempfile.TemporaryDirectory() as tmp:
            good,bad,out=Path(tmp)/'good.md',Path(tmp)/'bad.md',Path(tmp)/'notes.md'
            good.write_text(GOOD);bad.write_text(GOOD.replace('### Fixed','### Bug fixes'))
            def run(*argv):
                stdout,stderr=io.StringIO(),io.StringIO()
                with contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):
                    code=c.main(list(argv))
                return code,stdout.getvalue(),stderr.getvalue()
            self.assertEqual(run('--file',str(good),'check'),(0,'',''))
            code,_,stderr=run('--file',str(bad),'check')
            self.assertEqual(code,1);self.assertIn('bad.md: line 23: "### Bug fixes"',stderr)
            self.assertEqual(run('--file',str(good),'extract','1.0.0'),(0,'### Added\n\n- First release (#1).\n',''))
            self.assertEqual(run('--file',str(good),'extract','1.0.0','-o',str(out))[:2],(0,''))
            self.assertEqual(out.read_text(),'### Added\n\n- First release (#1).\n')
            for argv in [('--file',str(good),'extract','9.9.9'),('--file',str(bad),'extract','1.0.0'),('--file',str(Path(tmp)/'missing.md'),'check')]:
                with self.subTest(argv=argv[1:]):
                    code,stdout,stderr=run(*argv)
                    self.assertEqual((code,stdout),(1,''));self.assertTrue(stderr)

    def test_repository_changelog(self):
        text=(ROOT/'CHANGELOG.md').read_text(encoding='utf-8')
        self.assertEqual(c.check(text),[])
        released=[s for s in c.parse(text)[0] if s.label!=c.UNRELEASED]
        self.assertTrue(released)
        # Every released entry can be turned into release notes.
        for section in released:
            self.assertTrue(c.extract(text,section.label).strip(),section.label)

if __name__=='__main__':unittest.main()
