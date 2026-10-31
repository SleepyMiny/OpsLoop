"""Offline packaging of fixed-target helpers; no approval, credentials or API calls."""
import argparse
import hashlib
import json
import ipaddress
import os
from pathlib import Path
import stat
import re
from urllib.parse import urlsplit
import uuid


TEMPLATES = Path(__file__).with_name('host_helpers')
MODULE = Path(__file__).resolve().parents[1]
GO_FILES = {'policyHash/main.go': 'policy_hash.go.txt',
            'grant/main.go': 'grant.go.txt', 'quoteProbe/main.go': 'quote_probe.go.txt'}


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def absolute_path(value):
    """Inspect metadata only, including existing parents; future paths are allowed."""
    if not isinstance(value, str) or not value or any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise ValueError('path must be a nonempty string without control characters')
    path = Path(value)
    if path.anchor != '/' or '..' in path.parts or str(path) != value or path == Path('/'):
        raise ValueError('path must be absolute and normalized')
    for candidate in (path, *path.parents):
        try:
            mode = candidate.lstat().st_mode
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(mode):
            raise ValueError('symlink paths are not accepted')
        if candidate != path and not stat.S_ISDIR(mode):
            raise ValueError('path parent must be a directory')
    return path


def literal(value):
    # JSON string escapes are valid Go/Python string literals. Reject control
    # characters above, and preserve Unicode instead of surrogate-pair escapes.
    return json.dumps(value, ensure_ascii=False)


def replace_once(source, before, after):
    if source.count(before) != 1:
        raise ValueError('template replacement must match exactly once')
    return source.replace(before, after)


def https_origin(value):
    """Accept only canonical HTTPS origins, never URL paths or credentials."""
    if not isinstance(value, str) or not value or len(value) > 300 or not value.isascii():
        raise ValueError('invalid HTTPS origin')
    if any(ord(c) <= 32 or ord(c) == 127 for c in value):
        raise ValueError('invalid HTTPS origin')
    parsed = urlsplit(value)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.username is not None or parsed.password is not None or parsed.path or parsed.query or parsed.fragment:
        raise ValueError('HTTPS origin only')
    host = parsed.hostname
    if '%' in host:
        raise ValueError('scoped or encoded origin host is not accepted')
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        if len(host) > 253 or re.fullmatch(r'[0-9.]+', host) or any(
            not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', label)
            for label in host.split('.')
        ):
            raise ValueError('invalid origin hostname')
        canonical_host = host
    else:
        canonical_host = '[' + str(address) + ']' if address.version == 6 else str(address)
    port = parsed.port
    if port is not None and not 1 <= port <= 65535:
        raise ValueError('invalid origin port')
    canonical = 'https://' + canonical_host + (':' + str(port) if port is not None else '')
    if value != canonical:
        raise ValueError('origin must be canonical')
    return value


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate target configuration field')
        result[key] = value
    return result


def read_target(path, expected_sha):
    if not isinstance(expected_sha, str) or not re.fullmatch(r'[0-9a-f]{64}', expected_sha):
        raise ValueError('explicit target configuration SHA256 required')
    # O_NONBLOCK avoids waiting on a FIFO; inspect the opened descriptor before
    # reading. O_NOFOLLOW also rejects a final-component symlink swapped in late.
    # Walk from the filesystem root through held directory descriptors so a
    # concurrent parent symlink replacement cannot redirect the private input.
    parts = Path(path).parts
    directory = os.open(parts[0], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in parts[1:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory)
            directory = child
        fd = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    finally:
        os.close(directory)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('target configuration must be owner-held regular mode 0600')
        raw = stream.read(16385)
    if len(raw) > 16384 or sha(raw) != expected_sha:
        raise ValueError('target configuration hash or size mismatch')
    target = json.loads(raw.decode('utf-8'), object_pairs_hook=unique_object)
    fields = {'schema', 'apiOrigin', 'clockClusterUID', 'transportOrigins'}
    if not isinstance(target, dict) or set(target) != fields or target['schema'] != 'opsloop-host-helper-build-target/v1':
        raise ValueError('invalid target configuration schema')
    https_origin(target['apiOrigin'])
    uid = target['clockClusterUID']
    if not isinstance(uid, str) or str(uuid.UUID(uid)) != uid or uuid.UUID(uid).int == 0:
        raise ValueError('cluster UID must be a canonical nonzero UUID')
    origins = target['transportOrigins']
    if not isinstance(origins, list) or not 1 <= len(origins) <= 2:
        raise ValueError('transport origins must contain one or two exact origins')
    for origin in origins:
        https_origin(origin)
    if len(set(origins)) != len(origins) or origins[0] != target['apiOrigin']:
        raise ValueError('transport origins must be unique with the pinned API origin first')
    return target


def generate(output_root, kubeconfig, kubectl, target_config, target_config_sha256):
    output = absolute_path(output_root)
    config = absolute_path(kubeconfig)
    client = absolute_path(kubectl)
    module = absolute_path(str(MODULE))
    templates = absolute_path(str(TEMPLATES))
    target_path = absolute_path(target_config)
    if target_path.is_relative_to(module) or target_path in (config, client):
        raise ValueError('private target configuration must be separate and outside the module')
    target = read_target(target_path, target_config_sha256)
    if output == module or not output.is_relative_to(module) or output.is_relative_to(module / 'tools') or output.is_relative_to(module / 'tests'):
        raise ValueError('new output directory must be inside module and outside tools/tests')
    if config == client or config.is_relative_to(output) or client.is_relative_to(output):
        raise ValueError('external input paths must be distinct and outside generated source')
    for external in (config, client):
        if external.exists() and not external.is_file():
            raise ValueError('external input must be a file path')
    if output.exists():
        raise ValueError('output directory already exists; no overwrite or adoption')
    provenance_path = absolute_path(str(templates / 'provenance.json'))
    provenance_raw = provenance_path.read_bytes()
    provenance = json.loads(provenance_raw)
    expected = set(GO_FILES.values()) | {'audit.py.txt', 'kubernetes.py.txt'}
    if provenance.get('schema') != 'opsloop-host-helper-template-provenance/v1' or set(provenance.get('templates', {})) != expected:
        raise ValueError('invalid template provenance')
    sources = {}
    for name in sorted(expected):
        raw = absolute_path(str(templates / name)).read_bytes()
        if sha(raw) != provenance['templates'][name]['templateSHA256']:
            raise ValueError('template source hash mismatch')
        sources[name] = raw.decode('utf-8')
    generated = {}
    for name, template in GO_FILES.items():
        source = sources[template]
        if name != 'policyHash/main.go':
            source = replace_once(source, '__CONFIG_LITERAL__', literal(str(config)))
            source = replace_once(source, '__API_ORIGIN_LITERAL__', literal(target['apiOrigin']))
        if name == 'grant/main.go':
            source = replace_once(source, '__CLUSTER_UID_LITERAL__', literal(target['clockClusterUID']))
        generated[name] = source.encode('utf-8')
    transport = replace_once(sources['kubernetes.py.txt'], '__CLIENT_LITERAL__', literal(str(client)))
    transport = replace_once(transport, '__CONFIG_LITERAL__', literal(str(config)))
    transport = replace_once(transport, '__ORIGINS_LITERAL__', literal(target['transportOrigins']))
    generated['kubernetes.py'] = transport.encode('utf-8')
    generated['audit.py'] = sources['audit.py.txt'].encode('utf-8')
    # Bind the current module used for Go internal/controller and API imports.
    dependencies = [module / 'go.mod', module / 'go.sum']
    for directory in ('api', 'internal'):
        dependencies.extend(sorted((module / directory).rglob('*.go')))
    source_pins = {}
    for path in dependencies:
        checked = absolute_path(str(path))
        source_pins[str(path.relative_to(module))] = sha(checked.read_bytes())
    manifest = {
        'schema': 'opsloop-host-helper-source-release/v1',
        'status': 'GENERATED_SOURCE_ONLY_NOT_OPERATIONALLY_APPROVED',
        'moduleRoot': str(module), 'outputRoot': str(output),
        'configuration': {'kubeconfig': str(config), 'kubectl': str(client)},
        'targetConfigSHA256': target_config_sha256,
        'fixedTarget': {'apiOrigin': target['apiOrigin'],
                        'clockClusterUID': target['clockClusterUID'],
                        'transportOrigins': target['transportOrigins'],
                        'namespace': 'opsloop-demo-b'},
        'generatorSHA256': sha(Path(__file__).read_bytes()),
        'templateProvenanceSHA256': sha(provenance_raw),
        'templates': provenance['templates'], 'moduleSourcePins': source_pins,
        'files': {name: sha(raw) for name, raw in sorted(generated.items())},
        'buildContract': {'cwd': str(module), 'environment': {'CGO_ENABLED': '0', 'GOOS': 'linux'},
                          'packages': {key: './' + str(output.relative_to(module) / key)
                                       for key in ('policyHash', 'grant', 'quoteProbe')},
                          'flags': ['-trimpath', '-ldflags=-s -w'],
                          'requiredReceipt': ['GOARCH', 'Go executable SHA256', 'this manifest SHA256',
                                              'exact argv', 'exit code', 'output binary SHA256']},
        'approval': 'New generated helper and binary pins require a new approved host plan. No runtime permission is issued.',
    }
    output.mkdir(mode=0o700)  # Parent must already exist; never create install roots.
    for name, raw in generated.items():
        path = output / name
        path.parent.mkdir(mode=0o700, exist_ok=True)
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb') as stream:
            stream.write(raw)
    with os.fdopen(os.open(output / 'manifest.json', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as stream:
        json.dump(manifest, stream, indent=2, ensure_ascii=False)
        stream.write('\n')
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output-root', required=True)
    parser.add_argument('--kubeconfig', required=True)
    parser.add_argument('--kubectl', required=True)
    parser.add_argument('--target-config', required=True)
    parser.add_argument('--target-config-sha256', required=True)
    args = parser.parse_args()
    try:
        result = generate(args.output_root, args.kubeconfig, args.kubectl, args.target_config, args.target_config_sha256)
    except (ValueError, OSError, KeyError, UnicodeError) as error:
        parser.exit(2, 'host helper generation rejected: ' + type(error).__name__ + '\n')
    print(json.dumps({'status': result['status'], 'manifest': str(Path(args.output_root) / 'manifest.json')}))


if __name__ == '__main__':
    main()
