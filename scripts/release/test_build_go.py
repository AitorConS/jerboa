import importlib.util,json,re,unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('build_go',Path(__file__).with_name('build-go.py'))
b=importlib.util.module_from_spec(spec);spec.loader.exec_module(b)
HERE=Path(__file__).parent

class FloorTests(unittest.TestCase):
    def test_older_version_is_raised_to_the_floor(self):
        self.assertEqual(b.floor_for('v0.60.0',['v0.62.0']),'v0.62.0')

    def test_version_at_or_above_the_floor_is_kept(self):
        self.assertIsNone(b.floor_for('v0.62.0',['v0.62.0']))
        self.assertIsNone(b.floor_for('v0.63.1',['v0.62.0']))

    def test_pre_release_of_the_floor_is_raised(self):
        self.assertEqual(b.floor_for('v0.62.0-rc.1',['v0.62.0']),'v0.62.0')

    def test_release_line_gets_its_own_fix(self):
        floors=['v3.5.33','v3.6.14','v3.7.1']
        self.assertEqual(b.floor_for('v3.5.20',floors),'v3.5.33')
        self.assertEqual(b.floor_for('v3.6.0',floors),'v3.6.14')
        self.assertIsNone(b.floor_for('v3.6.16',floors))
        self.assertEqual(b.floor_for('v3.4.9',floors),'v3.7.1')

    def test_newer_line_than_any_floor_is_kept(self):
        self.assertIsNone(b.floor_for('v3.8.0',['v3.5.33','v3.7.1']))

    def test_old_pseudo_version_is_raised(self):
        self.assertEqual(b.floor_for('v0.0.0-20250101000000-abcdef123456',['v0.62.0']),'v0.62.0')

    def test_unparsed_versions_are_left_alone(self):
        self.assertIsNone(b.floor_for('master',['v0.62.0']))

    def test_floor_file_parses(self):
        floors=b.floors()
        self.assertIn('golang.org/x/net',floors)
        for module,versions in floors.items():
            self.assertTrue(versions,module)
            for v in versions:self.assertIsNotNone(b.version_key(v),f'{module} {v}')

class RecipeTests(unittest.TestCase):
    KEYS={'main','dir','ldflags','tags','cgo','env','assets','node','prepare'}

    def test_go_recipes_are_complete(self):
        recipes=[r for r in json.loads((HERE/'packages.json').read_text()) if 'go' in r]
        self.assertTrue(recipes)
        for r in recipes:
            name=f"{r['package']}:{r['version']}"
            self.assertEqual(r['build_script'],'scripts/build-go.sh',name)
            self.assertLessEqual(set(r['go']),self.KEYS,name)
            self.assertRegex(r['sha256'],'^[0-9a-f]{64}$',name)
            upstream=r.get('upstream_version',r['version'])
            self.assertTrue(r['version']==upstream or r['version'].startswith(upstream+'-'),name)
            for asset in r['go'].get('assets',[]):
                self.assertTrue(asset['url'].startswith('https://'),name)
                self.assertRegex(asset['sha256'],'^[0-9a-f]{64}$',name)
                self.assertFalse(re.search(r'(^|/)\.\.(/|$)',asset['dest']),name)

if __name__=='__main__':unittest.main()
