"""Offline Linux runtime packaging contract. Uses no credentials or network."""
import importlib.util
import os
from pathlib import Path
import tarfile
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('linux_runtime_bundle', REPO / 'gateway/scripts/runtime_bundle.py')
bundle = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bundle)

RUNTIME_FILES = (
    'bridge.py', 'managed.py', 'stream_bridge.py', 'runtime.js', 'tasks_runtime.js',
    'work_runtime.js', 'stream_runtime.js', 'probe_runtime.js', 'probe_cleanup.js',
    'requirements.txt',
)


class LinuxBundleTests(unittest.TestCase):
    def test_linux_archive_contains_real_restricted_runtime_and_dependency_manifest(self):
        scratch = Path(os.environ.get('LOOMGATE_TEST_TMP', 'D:/LoomGate-work/tmp' if os.name == 'nt'
                                      else str(REPO.parent / 'LoomGate-work/tmp')))
        scratch.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='doubao-package-', dir=scratch) as directory:
            stage = Path(directory) / 'stage'
            stage.mkdir()
            for name in bundle.FILES:
                target = stage / name
                target.parent.mkdir(parents=True, exist_ok=True)
                if name.startswith('doubao-runtime/'):
                    target.write_bytes((REPO / 'tools' / name).read_bytes())
                else:
                    target.write_bytes(b'synthetic package input\n')
            private = stage / 'doubao-runtime/profile'
            private.mkdir(parents=True, exist_ok=True)
            (private / 'synthetic-state').write_text('excluded fixture', encoding='utf-8')
            archive = Path(directory) / 'runtime.tar.gz'
            manifest = bundle.package(stage, 'dev', 'amd64', archive)
            with tarfile.open(archive) as packed:
                for name in RUNTIME_FILES:
                    member = 'doubao-runtime/' + name
                    self.assertIn(member, packed.getnames(), 'Linux runtime missing ' + member)
                    self.assertIn(member, manifest['files'])
                    self.assertEqual(packed.extractfile(member).read(), (REPO / 'tools' / member).read_bytes())
                    self.assertEqual(packed.getmember(member).mode, 0o644)
                self.assertNotIn('doubao-runtime/profile/synthetic-state', packed.getnames())


if __name__ == '__main__':
    unittest.main()
