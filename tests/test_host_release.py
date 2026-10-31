"""Offline helper packaging: exact transformations and fail-closed input paths."""
import ast
import hashlib
import json
import os
import stat
import subprocess
import sys
from types import SimpleNamespace
from pathlib import Path
import tempfile
import unittest
from unittest import mock

from tools import host_release as release


class HostReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.module = self.root / 'module'
        self.module.mkdir()
        (self.module / 'go.mod').write_text('module example.invalid/fixture\n')
        (self.module / 'go.sum').write_text('')
        (self.module / 'api').mkdir()
        (self.module / 'api/types.go').write_text('package api\n')
        (self.module / 'internal').mkdir()
        self.target = self.root / 'target.json'
        self.target_data = {
            'schema': 'opsloop-host-helper-build-target/v1',
            'apiOrigin': 'https://192.0.2.10:6443',
            'clockClusterUID': '11111111-2222-4333-8444-555555555555',
            'transportOrigins': ['https://192.0.2.10:6443', 'https://198.51.100.20:6443'],
        }
        self.write_target()
        self.patch = mock.patch.object(release, 'MODULE', self.module)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.addCleanup(self.temp.cleanup)

    def write_target(self, data=None, raw=None):
        self.target.write_bytes(raw if raw is not None else json.dumps(
            self.target_data if data is None else data).encode())
        self.target.chmod(0o600)
        self.target_sha = release.sha(self.target.read_bytes())

    def generate(self, config=None, client=None, output=None, target=None, target_sha=None):
        return release.generate(str(output or self.module / 'generated'),
                                str(config or self.root / 'private/config'),
                                str(client or self.root / 'bin/kubectl'),
                                str(target or self.target),
                                self.target_sha if target_sha is None else target_sha)

    def test_only_approved_go_literal_changes_and_exact_audit(self):
        result = self.generate()
        output = self.module / 'generated'
        for name, template in release.GO_FILES.items():
            before = (release.TEMPLATES / template).read_bytes()
            after = (output / name).read_bytes()
            expected = before
            if name != 'policyHash/main.go':
                expected = expected.replace(b'__CONFIG_LITERAL__', release.literal(str(self.root / 'private/config')).encode())
                expected = expected.replace(b'__API_ORIGIN_LITERAL__', release.literal(self.target_data['apiOrigin']).encode())
            if name == 'grant/main.go':
                expected = expected.replace(b'__CLUSTER_UID_LITERAL__', release.literal(self.target_data['clockClusterUID']).encode())
            self.assertEqual(after, expected)
            self.assertEqual(hashlib.sha256(after).hexdigest(), result['files'][name])
        self.assertEqual((output / 'audit.py').read_bytes(), (release.TEMPLATES / 'audit.py.txt').read_bytes())
        self.assertIn('api/types.go', result['moduleSourcePins'])
        self.assertIn('NOT_OPERATIONALLY_APPROVED', result['status'])
        self.assertEqual(result['targetConfigSHA256'], self.target_sha)
        self.assertEqual(result['fixedTarget']['apiOrigin'], self.target_data['apiOrigin'])
        self.assertEqual(result['fixedTarget']['clockClusterUID'], self.target_data['clockClusterUID'])
        self.assertEqual(result['fixedTarget']['namespace'], 'opsloop-demo-b')
        self.assertEqual(result['fixedTarget']['transportOrigins'], self.target_data['transportOrigins'])
        self.assertIn('new approved host plan', result['approval'])
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o700)
        for path in output.rglob('*'):
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700 if path.is_dir() else 0o600)

    def test_escaped_paths_are_literals_not_code(self):
        config = self.root / 'private/quo"te\\unicode-한글'
        client = self.root / "bin/quo'te $(never) `never`"
        self.generate(config, client)
        tree = ast.parse((self.module / 'generated/kubernetes.py').read_text())
        values = {n.targets[0].id: ast.literal_eval(n.value) for n in tree.body
                  if isinstance(n, ast.Assign) and isinstance(n.targets[0], ast.Name)}
        self.assertEqual(values['CONFIG'], str(config))
        self.assertEqual(values['CLIENT'], str(client))
        self.assertEqual(values['ORIGINS'], self.target_data['transportOrigins'])
        self.assertEqual([n.name for n in tree.body if isinstance(n, ast.FunctionDef)], ['identity'])
        grant = (self.module / 'generated/grant/main.go').read_text()
        self.assertIn('const clockConfig = ' + release.literal(str(config)), grant)

    def test_does_not_read_external_inputs(self):
        config = self.root / 'config'
        client = self.root / 'kubectl'
        config.write_text('PRIVATE_SENTINEL')
        client.write_text('MUST_NOT_EXECUTE')
        original = Path.read_bytes
        def guarded(path):
            if path in (config, client):
                raise AssertionError('external input was read')
            return original(path)
        with mock.patch.object(Path, 'read_bytes', guarded):
            self.generate(config, client)
        self.assertNotIn('PRIVATE_SENTINEL', (self.module / 'generated/manifest.json').read_text())

    def test_bad_paths_rejected_before_output(self):
        for value in ('relative', '/', '//tmp/double-root', '/tmp/../elsewhere', '/tmp//double', '/tmp/new\nline', '/tmp/tab\t', '/tmp/nul\0'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.generate(config=value)
        self.assertFalse((self.module / 'generated').exists())

    def test_symlinks_and_nonfile_inputs_rejected(self):
        directory = self.root / 'real'
        directory.mkdir()
        link = self.root / 'link'
        link.symlink_to(directory, target_is_directory=True)
        for path in (link / 'missing', directory):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.generate(config=path)
        dangling = self.root / 'dangling'
        dangling.symlink_to(self.root / 'missing')
        with self.assertRaises(ValueError):
            self.generate(client=dangling)

    def test_output_ownership_and_no_overwrite(self):
        for output in (self.root / 'outside', self.module, self.module / 'tools/out', self.module / 'tests/out'):
            with self.subTest(output=output), self.assertRaises(ValueError):
                self.generate(output=output)
        with self.assertRaises(ValueError):
            self.generate(config=self.module / 'generated/config')
        self.generate()
        before = (self.module / 'generated/manifest.json').read_bytes()
        with self.assertRaises(ValueError):
            self.generate()
        self.assertEqual(before, (self.module / 'generated/manifest.json').read_bytes())

    def test_template_tamper_and_replacement_ambiguity(self):
        import shutil
        templates = self.root / 'templates'
        shutil.copytree(release.TEMPLATES, templates)
        (templates / 'grant.go.txt').write_text('tampered')
        with mock.patch.object(release, 'TEMPLATES', templates), self.assertRaises(ValueError):
            self.generate()
        self.assertFalse((self.module / 'generated').exists())
        for source in ('absent', 'same same'):
            with self.assertRaises(ValueError):
                release.replace_once(source, 'same', 'replacement')

    def test_missing_bad_hash_or_changed_target_rejected_without_output(self):
        for digest in ('', '0' * 64, 'A' * 64, 'f' * 63, 3):
            with self.subTest(digest=digest), self.assertRaises(ValueError):
                self.generate(target_sha=digest)
        with self.assertRaises(FileNotFoundError):
            self.generate(target=self.root / 'absent')
        self.target.write_bytes(self.target.read_bytes() + b' ')
        with self.assertRaises(ValueError):
            self.generate()
        self.assertFalse((self.module / 'generated').exists())

    def test_target_permissions_owner_and_symlinks_rejected_without_output(self):
        for mode in (0o644, 0o400, 0o660, 0o1600):
            self.target.chmod(mode)
            with self.subTest(mode=mode), self.assertRaises(ValueError):
                self.generate()
        self.target.chmod(0o600)
        info = self.target.stat()
        with mock.patch.object(release.os, 'fstat', return_value=SimpleNamespace(
                st_mode=info.st_mode, st_uid=os.getuid() + 1)), self.assertRaises(ValueError):
            self.generate()
        link = self.root / 'target-link'
        link.symlink_to(self.target)
        with self.assertRaises(ValueError):
            self.generate(target=link)
        parent_link = self.root / 'parent-link'
        parent_link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(ValueError):
            self.generate(target=parent_link / self.target.name)
        dangling = self.root / 'target-dangling'
        dangling.symlink_to(self.root / 'absent')
        with self.assertRaises(ValueError):
            self.generate(target=dangling)
        fifo = self.root / 'target-fifo'
        os.mkfifo(fifo, 0o600)
        with self.assertRaises(ValueError):
            self.generate(target=fifo)
        with self.assertRaises((ValueError, OSError)):
            self.generate(target=self.root)
        self.assertFalse((self.module / 'generated').exists())

    def test_exact_target_schema_duplicates_and_bounded_read(self):
        invalid = [[], None, {}, dict(self.target_data, unknown=True),
                   dict(self.target_data, schema='other'),
                   {k: v for k, v in self.target_data.items() if k != 'apiOrigin'}]
        for data in invalid:
            self.write_target(raw=json.dumps(data).encode())
            with self.subTest(data=data), self.assertRaises(ValueError):
                self.generate()
        raw = json.dumps(self.target_data)
        for bad in (raw[:-1] + ',"apiOrigin":"https://203.0.113.5"}',
                    '{"nested":{"duplicate":1,"duplicate":2}}',
                    raw + ' {}', ' ' * 16385, '{', '[NaN]'):
            self.write_target(raw=bad.encode())
            with self.subTest(bad=bad[:40]), self.assertRaises(ValueError):
                self.generate()
        self.assertFalse((self.module / 'generated').exists())

    def test_invalid_origins_and_allowlist_fail_closed(self):
        for origin in ('http://192.0.2.10', 'https://user@192.0.2.10',
                       'https://192.0.2.10/', 'https://192.0.2.10/path',
                       'https://192.0.2.10?q=1', 'https://192.0.2.10#x',
                       'https://192.0.2.10?', 'https://192.0.2.10#',
                       'https://192.0.2.10:0', 'https://192.0.2.10:65536',
                       'https://192.0.2.10:06443', 'https://192.0.2.10:',
                       'HTTPS://192.0.2.10', 'https://EXAMPLE.invalid',
                       'https://*.example.invalid', 'https://example.invalid.',
                       'https://192.0.2.010', 'https://[fe80::1%zone]',
                       'https://exa_mple.invalid', 'https://',
                       'https://192.0.2.10\n', 'https://192.0.2.10\x7f',
                       'https://192.0.2.10\\evil', 'https://";panic(1)//',
                       'https://한글.invalid', None, 123, {}):
            self.write_target(dict(self.target_data, apiOrigin=origin, transportOrigins=[origin]))
            with self.subTest(origin=origin), self.assertRaises(ValueError):
                self.generate()
        primary = self.target_data['apiOrigin']
        for origins in ([], [primary, primary], [primary] * 3, primary, {},
                        ['https://203.0.113.10'], ['https://203.0.113.10', primary],
                        [primary, 'http://203.0.113.10'], [primary, {}]):
            self.write_target(dict(self.target_data, transportOrigins=origins))
            with self.subTest(origins=origins), self.assertRaises(ValueError):
                self.generate()
        self.assertFalse((self.module / 'generated').exists())

    def test_uid_validation_and_private_target_location(self):
        for uid in ('', 'not-a-uuid', '00000000-0000-0000-0000-000000000000',
                    'AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE',
                    '11111111222243338444555555555555',
                    '11111111-2222-4333-8444-555555555555\n',
                    '";panic(1)//', 123, None):
            self.write_target(dict(self.target_data, clockClusterUID=uid))
            with self.subTest(uid=uid), self.assertRaises(ValueError):
                self.generate()
        self.write_target()
        inside = self.module / 'target.json'
        inside.write_bytes(self.target.read_bytes())
        inside.chmod(0o600)
        with self.assertRaises(ValueError):
            self.generate(target=inside)
        with self.assertRaises(ValueError):
            self.generate(config=self.target)
        with self.assertRaises(ValueError):
            self.generate(client=self.target)
        self.assertFalse((self.module / 'generated').exists())

    def test_double_slash_aliases_rejected_before_output(self):
        inside = self.module / 'target.json'
        inside.write_bytes(self.target.read_bytes())
        inside.chmod(0o600)
        # POSIX // aliases can name the same file while pathlib containment
        # compares distinct anchors. Reject them before any boundary checks.
        for arguments in ({'target': '/' + str(inside)},
                          {'target': '/' + str(self.target)},
                          {'config': '/' + str(self.target)},
                          {'client': '/' + str(self.target)},
                          {'output': '/' + str(self.module / 'generated')}):
            with self.subTest(arguments=arguments), self.assertRaises(ValueError):
                self.generate(**arguments)
            self.assertFalse((self.module / 'generated').exists())
        self.generate()
        self.assertTrue((self.module / 'generated/manifest.json').is_file())

    def test_cli_requires_both_target_arguments_and_redacts_bad_input(self):
        command = [sys.executable, '-B', str(Path(release.__file__)),
                   '--output-root', str(self.module / 'generated'),
                   '--kubeconfig', str(self.root / 'private/config'),
                   '--kubectl', str(self.root / 'bin/kubectl')]
        for extra in ([], ['--target-config', str(self.target)],
                      ['--target-config-sha256', self.target_sha],
                      ['--target-config', str(self.target), '--target-config-sha256', 'PRIVATE_BAD_HASH']):
            result = subprocess.run(command + extra, capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 2)
            self.assertEqual(result.stdout, '')
            self.assertNotIn('PRIVATE_BAD_HASH', result.stderr)
        self.assertFalse((self.module / 'generated').exists())

    def test_canonical_https_host_and_single_origin_supported(self):
        for origin in ('https://192.0.2.10:6443', 'https://api.example.invalid',
                       'https://[2001:db8::1]:6443'):
            self.assertEqual(release.https_origin(origin), origin)
        self.write_target(dict(self.target_data, transportOrigins=[self.target_data['apiOrigin']]))
        self.generate()


if __name__ == '__main__':
    unittest.main()
