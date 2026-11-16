"""prepare-release.py names the Unreleased changelog section after a version."""
import contextlib,datetime,importlib.util,io,tempfile,unittest
from pathlib import Path
HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location('prepare',HERE/'prepare-release.py')
p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p)
LINK='https://github.com/AitorConS/jerboa/tree/main/docs/releases/v'

LOG=f'''# Changelog

Preamble.

## [Unreleased]

### Fixed

- A fix not released yet (#3).

## [1.1.0] - 2026-02-10

### Changed

- Something (#2).

## [1.0.0] - 2026-01-05

### Added

- The first thing (#1).

[Unreleased]: https://github.com/AitorConS/jerboa/commits/main
[1.1.0]: {LINK}1.1.0
[1.0.0]: https://github.com/AitorConS/jerboa/releases/tag/v1.0.0
'''

@unittest.skipUnless((HERE/'changelog.py').exists(),'scripts/release/changelog.py is not in this checkout')
class PrepareRelease(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name)
        (self.root/'CHANGELOG.md').write_text(LOG);(self.root/'VERSION.md').write_text('1.1.0\n')
        self.changelog=p.load_changelog()

    def run_main(self,*args):
        out,err=io.StringIO(),io.StringIO()
        with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):
            code=p.main([*args,'--root',str(self.root)])
        return code,out.getvalue(),err.getvalue()

    def test_unreleased_becomes_the_version(self):
        code,out,err=self.run_main('1.2.0','--date','2026-03-01')
        self.assertEqual(code,0,err)
        text=(self.root/'CHANGELOG.md').read_text()
        self.assertEqual(text,LOG.replace('## [Unreleased]\n','## [Unreleased]\n\n## [1.2.0] - 2026-03-01\n')
                         .replace('commits/main\n',f'commits/main\n[1.2.0]: {LINK}1.2.0\n'))
        self.assertEqual((self.root/'VERSION.md').read_text(),'1.2.0\n')
        self.assertEqual(self.changelog.check(text),[])
        # The entry is what the release publishes as its notes; Unreleased is empty again.
        self.assertEqual(self.changelog.extract(text,'1.2.0'),'### Fixed\n\n- A fix not released yet (#3).\n')
        with self.assertRaises(self.changelog.ChangelogError):self.changelog.extract(text,'unreleased')
        # Existing entries are untouched.
        for version in ['1.1.0','1.0.0']:
            self.assertEqual(self.changelog.extract(text,version),self.changelog.extract(LOG,version))
        # A second run changes nothing.
        code,out,_=self.run_main('1.2.0','--date','2026-03-02')
        self.assertEqual(code,0);self.assertIn('already has an entry',out)
        self.assertEqual((self.root/'CHANGELOG.md').read_text(),text)

    def test_date_defaults_to_today_in_utc(self):
        self.assertEqual(self.run_main('1.2.0')[0],0)
        today=datetime.datetime.now(datetime.timezone.utc).date().isoformat()
        self.assertIn(f'## [1.2.0] - {today}\n',(self.root/'CHANGELOG.md').read_text())

    def test_version_file_is_written_for_an_entry_that_exists(self):
        (self.root/'VERSION.md').write_text('1.0.0\n')
        self.assertEqual(self.run_main('1.1.0')[0],0)
        self.assertEqual((self.root/'VERSION.md').read_text(),'1.1.0\n')
        self.assertEqual((self.root/'CHANGELOG.md').read_text(),LOG)

    def test_refusals_change_nothing(self):
        cases=[(['v1.2.0'],LOG,'not a version'),(['1.2.0-rc1'],LOG,'not a version'),
               (['1.0.5'],LOG,'not newer than 1.1.0'),(['1.2.0','--date','2026-02-09'],LOG,'before 2026-02-10'),
               (['1.2.0'],LOG.replace('### Fixed\n\n- A fix not released yet (#3).\n\n',''),'Unreleased section is empty'),
               (['1.2.0'],LOG.replace('# Changelog','# Notes'),'malformed'),
               (['1.2.0'],LOG.replace('### Fixed','### Mended'),'malformed')]
        for args,log,message in cases:
            with self.subTest(args=args,message=message):
                (self.root/'CHANGELOG.md').write_text(log)
                code,_,err=self.run_main(*args)
                self.assertEqual(code,1);self.assertIn(message,err)
                self.assertEqual((self.root/'CHANGELOG.md').read_text(),log)
                self.assertEqual((self.root/'VERSION.md').read_text(),'1.1.0\n')
        (self.root/'CHANGELOG.md').unlink()
        self.assertEqual(self.run_main('1.2.0')[0],1)

    def test_link_follows_the_newest_release_or_the_default(self):
        log=LOG.replace(f'[1.1.0]: {LINK}1.1.0','[1.1.0]: https://example.org/notes')
        result=p.prepare(log,'1.2.0',datetime.date(2026,3,1),self.changelog)
        self.assertIn(f'[Unreleased]: https://github.com/AitorConS/jerboa/commits/main\n[1.2.0]: {LINK}1.2.0\n[1.1.0]: https://example.org/notes\n',result)
        log=LOG.replace(f'{LINK}1.1.0','https://example.org/r/1.1.0/notes')
        self.assertIn('[1.2.0]: https://example.org/r/1.2.0/notes\n',p.prepare(log,'1.2.0',datetime.date(2026,3,1),self.changelog))

if __name__=='__main__':unittest.main()
