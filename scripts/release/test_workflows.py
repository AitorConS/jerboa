"""Contract tests for release ownership without third-party YAML dependencies."""
import importlib.util,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
class WorkflowContracts(unittest.TestCase):
 def test_only_promoter_advances_stable(self):
  workflows=ROOT/'.github/workflows'
  # The main CI and builder must not contain upload commands or invoke promote.
  for name in ['main.yml','validate.yml','release.yml']:
   text=(workflows/name).read_text()
   self.assertNotIn('candidate.py promote',text)
   self.assertNotIn('aws s3 cp',text)
   self.assertNotIn('publish-packages:',text)
  self.assertIn('cancel-in-progress: false',(workflows/'promote.yml').read_text())
  self.assertIn('environment: release',(workflows/'promote.yml').read_text())
 def test_scope_handles_docs_and_kernel(self):
  spec=importlib.util.spec_from_file_location('scope',Path(__file__).with_name('ci-select.py'))
  scope=importlib.util.module_from_spec(spec);spec.loader.exec_module(scope)
  self.assertEqual(scope.classify(['docs/foo.md']),(False,False))
  self.assertEqual(scope.classify(['internal/api/client.go']),(True,False))
  self.assertEqual(scope.classify(['kernel/platform/pc/main.c']),(True,True))
  self.assertEqual(scope.classify(['tests/integration/lifecycle_test.go']),(True,True))
  self.assertEqual(scope.classify(['tests/e2e/build_test.go']),(True,True))
  self.assertEqual(scope.classify(['.github/workflows/release.yml']),(True,True))
if __name__=='__main__':unittest.main()
