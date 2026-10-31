"""Versioned Kubernetes hash domains. No transport, mutation or disclosure."""
from __future__ import annotations
import copy
import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat
from dataclasses import dataclass
from tools.exhibition_control import ControlError

VERSION = 'opsloop-object-hash/v1'
NORMALIZATION = 'json-sort-keys-ascii-compact-finite-v1'
RAW = 'kubernetes-raw'
DISPLAY = 'display-projection'


def require(ok, reason):
    if not ok: raise ControlError(reason)


def encoded(value):
    """All object keys retained; arrays ordered; ASCII escapes; UTF-8; no LF."""
    def valid(v):
        if isinstance(v, dict):
            require(all(isinstance(k, str) for k in v), 'HASH_NON_STRING_KEY')
            for x in v.values(): valid(x)
        elif isinstance(v, list):
            for x in v: valid(x)
        else: require(v is None or type(v) in (str, bool, int) or type(v) is float and math.isfinite(v), 'HASH_NON_JSON_VALUE')
    valid(value)
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True, allow_nan=False).encode('utf-8')


def digest(value): return hashlib.sha256(encoded(value)).hexdigest()


def parse(raw):
    def unique(pairs):
        result = {}
        for k, v in pairs:
            require(k not in result, 'HASH_DUPLICATE_KEY'); result[k] = v
        return result
    try:
        value = json.loads(raw, object_pairs_hook=unique, parse_constant=lambda _: require(False, 'HASH_NON_FINITE'))
        encoded(value)
        return value
    except (ValueError, UnicodeError) as exc:
        if isinstance(exc, ControlError): raise
        raise ControlError('HASH_JSON_INVALID') from None


def identity(obj):
    require(isinstance(obj, dict) and isinstance(obj.get('metadata'), dict), 'HASH_OBJECT_REQUIRED')
    m = obj['metadata']
    result = {k: obj.get(k) for k in ('apiVersion', 'kind')}
    result.update({k: m.get(k) for k in ('namespace', 'name', 'uid')})
    require(all(isinstance(v, str) and v for v in result.values()), 'HASH_OBJECT_IDENTITY_REQUIRED')
    return result


def describe(obj, fields, *, representation=RAW):
    require(fields in ('/', '/spec') and representation in (RAW, DISPLAY), 'HASH_DOMAIN_INVALID')
    value = obj if fields == '/' else obj.get('spec')
    require(isinstance(value, dict), 'HASH_FIELDS_REQUIRED')
    return {'schema': VERSION, 'object': identity(obj), 'representation': representation,
            'fields': fields, 'normalization': NORMALIZATION, 'algorithm': 'sha256', 'sha256': digest(value)}


def validate(pin, *, fields, representation=RAW):
    require(isinstance(pin, dict) and set(pin) == {'schema','object','representation','fields','normalization','algorithm','sha256'}, 'HASH_CONTRACT_REQUIRED')
    require(pin['schema'] == VERSION and pin['normalization'] == NORMALIZATION and pin['algorithm'] == 'sha256', 'HASH_CONTRACT_VERSION')
    require(pin['fields'] == fields and pin['representation'] == representation, 'HASH_REPRESENTATION_MISMATCH')
    require(isinstance(pin['object'], dict) and set(pin['object']) == {'apiVersion','kind','namespace','name','uid'}
            and all(isinstance(v,str) and v for v in pin['object'].values()), 'HASH_OBJECT_IDENTITY_REQUIRED')
    require(isinstance(pin['sha256'],str) and re.fullmatch('[0-9a-f]{64}',pin['sha256']), 'HASH_DIGEST_REQUIRED')
    return pin


def matches(obj, pin, *, fields, representation=RAW):
    validate(pin, fields=fields, representation=representation)
    require(identity(obj) == pin['object'], 'HASH_OBJECT_IDENTITY_CHANGED')
    return describe(obj, fields, representation=representation) == pin


def verify(obj, pin, *, fields, representation=RAW):
    require(matches(obj, pin, fields=fields, representation=representation), 'HASH_CONTENT_CHANGED')
    return obj


def bound_pin(cfg, name, *, fields):
    pin = validate(cfg.get(name+'Hash'), fields=fields)
    require(cfg.get(name+'SHA256') == pin['sha256'], 'HASH_ALIAS_MISMATCH')
    return pin


def private_read(path, checksum):
    p = Path(path)
    require(p.is_absolute() and not any(x.is_symlink() for x in (p,*p.parents)), 'HASH_UNSAFE_SOURCE')
    st = p.stat()
    require(stat.S_ISREG(st.st_mode) and st.st_uid == os.getuid() and not st.st_mode & 0o077 and st.st_size <= 16*1024*1024, 'HASH_PRIVATE_SOURCE_REQUIRED')
    raw = p.read_bytes()
    require(hashlib.sha256(raw).hexdigest() == checksum, 'HASH_SOURCE_BYTES_CHANGED')
    return parse(raw)


@dataclass(frozen=True)
class Observation:
    obj: dict
    representation: str
    response_sha256: str
    observed_at: str
    path: str


def load_capture(path, checksum):
    """Trusted host-capture manifest is externally pinned, never reconstructed from a view."""
    manifest = private_read(path, checksum)
    require(isinstance(manifest.get('records'), list) and manifest.get('mutations') == 0, 'HASH_CAPTURE_REQUIRED')
    result = {}
    for row in manifest['records']:
        require(row.get('method') == 'GET' and row.get('exitCode') == 0 and row.get('representation') == RAW
                and row.get('historicalOriginal') is False and row.get('observedAt'), 'HASH_RAW_CAPTURE_REQUIRED')
        obj = private_read(row['pathToRaw'], row['responseBytesSHA256'])
        meta = identity(obj)
        require(meta['uid'] == row['uid'] and obj['metadata'].get('resourceVersion') == row['resourceVersion'], 'HASH_CAPTURE_IDENTITY_CHANGED')
        require(row['name'] not in result, 'HASH_CAPTURE_DUPLICATE')
        result[row['name']] = Observation(obj, RAW, row['responseBytesSHA256'], row['observedAt'], row['path'])
    return result


def raw_pin(observation, fields):
    require(isinstance(observation, Observation) and observation.representation == RAW, 'HASH_RAW_OBSERVATION_REQUIRED')
    return describe(observation.obj, fields)


def project(obj):
    """Display-only projection. Never used to validate originals or construct a baseline."""
    def clean(v):
        if isinstance(v,list): return [clean(x) for x in v]
        if not isinstance(v,dict): return v
        result = {}
        for k,x in v.items():
            if k in ('managedFields','data','binaryData','stringData'): continue
            if k == 'env':
                result[k] = [{'name':e.get('name'),'value':'REDACTED'} for e in x]; continue
            if k == 'annotations':
                result[k] = {a:'REDACTED' for a in x}; continue
            if k in ('message','command'):
                result[k+'SHA256'] = digest(x); continue
            result[k] = clean(x)
        return result
    return clean(copy.deepcopy(obj))
