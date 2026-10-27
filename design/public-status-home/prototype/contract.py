"""Offline contract model only; not a deployed Collector or Cloudflare Worker.

Uses stdlib, synthetic fixtures and local SQLite. Does not access a cluster,
the network, a credential store or Cloudflare. Production ports need their own
Go/WebCrypto/D1/HTTP integration and browser tests.
"""
import copy
import hashlib
import hmac
import json
import math
from pathlib import Path

MAX_INT = 2**53 - 1
NODES = {"control": "laptop-vm", "physical": "physical-server", "cloud": "azure-vm"}
ORIGIN = "https://opsloop.sleepyminy44.workers.dev"
PATH = "/api/collect/status"
MAX_BODY = 16 * 1024


def obj(properties):
    return {"type": "object", "properties": properties,
            "required": list(properties), "additionalProperties": False}


def nullable(schema):
    return {"anyOf": [schema, {"type": "null"}]}


def integer(low=0, high=MAX_INT):
    return {"type": "integer", "minimum": low, "maximum": high}


def enum(*values):
    return {"enum": list(values)}


STAMP = nullable(integer(1))
SAMPLE = obj({"value": nullable({"type": "number", "minimum": 0, "maximum": 1}),
              "observedAt": STAMP})
SOURCE = obj({"state": enum("ok", "partial", "unavailable"), "observedAt": STAMP,
              "errorCode": enum(None, "TIMEOUT", "UNAUTHORIZED", "UNREACHABLE",
                                "MISSING", "AMBIGUOUS", "INVALID")})
NODE = obj({"id": enum(*NODES), "site": enum(*NODES.values()),
            "os": enum(None, "linux"), "arch": enum(None, "amd64", "arm64"),
            "readiness": enum(None, "Ready", "NotReady", "Unknown"),
            "readyObservedAt": STAMP,
            "cpu": SAMPLE, "memory": SAMPLE,
            "exporter": obj({"value": enum(None, 0, 1), "observedAt": STAMP})})
WORKLOAD = obj({"id": enum("controller", "demo-api"),
                "desired": nullable(integer(0, 100)), "ready": nullable(integer(0, 100)),
                "observedAt": STAMP})
SCHEMA = {"$schema": "https://json-schema.org/draft/2020-12/schema",
          **obj({"schemaVersion": {"const": 1}, "source": {"const": "observed"},
                 "collectedAt": integer(1),
                 "sources": obj({"kubernetes": SOURCE, "prometheus": SOURCE}),
                 "nodes": {"type": "array", "items": NODE, "minItems": 3, "maxItems": 3},
                 "workloads": {"type": "array", "items": WORKLOAD, "minItems": 2, "maxItems": 2},
                 "policy": obj({"mode": enum(None, "ObserveOnly", "Enforce"),
                                "enabled": nullable({"type": "boolean"}),
                                "monitoring": nullable({"type": "boolean"}),
                                "incidentCount": nullable(integer(0, 10000)),
                                "activeIncidentCount": nullable(integer(0, 10000)),
                                "observedAt": STAMP})})}


def check_schema(value, schema):
    """Only the JSON Schema subset emitted above; not a general schema engine."""
    if "anyOf" in schema:
        for option in schema["anyOf"]:
            try:
                check_schema(value, option)
                return
            except ValueError:
                pass
        raise ValueError("nullable type mismatch")
    if "const" in schema and (type(value) is not type(schema["const"]) or value != schema["const"]):
        raise ValueError("constant mismatch")
    if "enum" in schema and not any(type(value) is type(v) and value == v for v in schema["enum"]):
        raise ValueError("enum mismatch")
    kind = schema.get("type")
    valid = {"object": type(value) is dict, "array": type(value) is list,
             "integer": type(value) is int,
             "number": type(value) is int or (type(value) is float and math.isfinite(value)),
             "boolean": type(value) is bool, "null": value is None}
    if kind is not None and not valid[kind]:
        raise ValueError("type mismatch")
    if kind == "object":
        if set(value) != set(schema["properties"]):
            raise ValueError("unknown/missing field")
        for k, s in schema["properties"].items():
            check_schema(value[k], s)
    if kind == "array":
        if not schema["minItems"] <= len(value) <= schema["maxItems"]:
            raise ValueError("array bound")
        for item in value:
            check_schema(item, schema["items"])
    for k, test in (("minimum", lambda v: value >= v), ("maximum", lambda v: value <= v)):
        if k in schema and not test(schema[k]):
            raise ValueError("numeric bound")


def validate(snapshot):
    check_schema(snapshot, SCHEMA)
    at = snapshot["collectedAt"]
    def stamp(t):
        if t is not None and t > at + 5000:
            raise ValueError("future measurement")
    for s in snapshot["sources"].values():
        stamp(s["observedAt"])
        if s["state"] == "ok" and (s["observedAt"] is None or s["errorCode"] is not None):
            raise ValueError("unsubstantiated source success")
        if s["state"] != "ok" and s["errorCode"] is None:
            raise ValueError("missing source failure category")
        if s["state"] == "unavailable" and s["observedAt"] is not None:
            raise ValueError("unavailable query assigned fresh timestamp")
    if {n["id"] for n in snapshot["nodes"]} != set(NODES):
        raise ValueError("duplicate/missing public node")
    for node in snapshot["nodes"]:
        if node["site"] != NODES[node["id"]]:
            raise ValueError("node alias/site mismatch")
        stamp(node["readyObservedAt"])
        if (node["readiness"] is None) != (node["readyObservedAt"] is None):
            raise ValueError("unpaired node condition time")
        for key in ("cpu", "memory", "exporter"):
            sample = node[key]
            stamp(sample["observedAt"])
            if (sample["value"] is None) != (sample["observedAt"] is None):
                raise ValueError("unpaired metric time")
    if {w["id"] for w in snapshot["workloads"]} != {"controller", "demo-api"}:
        raise ValueError("duplicate/missing workload")
    for w in snapshot["workloads"]:
        stamp(w["observedAt"])
        if (w["observedAt"] is None) != (w["ready"] is None and w["desired"] is None):
            raise ValueError("unpaired workload observation")
    stamp(snapshot["policy"]["observedAt"])
    p = snapshot["policy"]
    if p["incidentCount"] is not None and p["activeIncidentCount"] is not None and p["activeIncidentCount"] > p["incidentCount"]:
        raise ValueError("active count exceeds observed incident count")
    if p["observedAt"] is None and any(p[k] is not None for k in p if k != "observedAt"):
        raise ValueError("policy values without observation")
    if snapshot["sources"]["kubernetes"]["state"] == "unavailable":
        if any(n[k] is not None for n in snapshot["nodes"] for k in ("os", "arch", "readiness", "readyObservedAt")):
            raise ValueError("failed Kubernetes query presented as observation")
        if any(w["observedAt"] is not None for w in snapshot["workloads"]) or p["observedAt"] is not None:
            raise ValueError("failed Kubernetes query reused current objects")
    if snapshot["sources"]["prometheus"]["state"] == "unavailable":
        if any(n[k]["value"] is not None for n in snapshot["nodes"] for k in ("cpu", "memory", "exporter")):
            raise ValueError("failed Prometheus query presented as observation")


def project(trusted_observations):
    """Synthetic adapter input: alias-keyed observations, NOT a BFF JSON endpoint.

    Production reads typed API responses and applies the same explicit construction.
    Unknown fields are never copied, and aliases are local configuration only.
    """
    r = trusted_observations
    pick = lambda o, keys: {k: copy.deepcopy(o[k]) for k in keys}
    out = pick(r, ("schemaVersion", "source", "collectedAt"))
    out["sources"] = {k: pick(r["sources"][k], SOURCE["properties"]) for k in ("kubernetes", "prometheus")}
    out["nodes"] = []
    for node in r["nodes"]:
        n = pick(node, ("id", "site", "os", "arch", "readiness", "readyObservedAt"))
        for key in ("cpu", "memory", "exporter"):
            n[key] = pick(node[key], ("value", "observedAt"))
        out["nodes"].append(n)
    out["workloads"] = [pick(w, WORKLOAD["properties"]) for w in r["workloads"]]
    out["policy"] = pick(r["policy"], SCHEMA["properties"]["policy"]["properties"])
    validate(out)
    return out


def data_state(s, now):
    """Freshness is independent of health; old Ready is never current healthy."""
    validate(s)
    if now < s["collectedAt"] - 5000:
        return "unavailable", "unknown"
    if now - s["collectedAt"] > 75000:
        return "stale", "unknown"
    fresh = lambda t, age: t is not None and -5000 <= now-t <= age
    complete = all(x["state"] == "ok" and fresh(x["observedAt"], 75000) for x in s["sources"].values())
    complete &= all(fresh(n["readyObservedAt"], 90000) for n in s["nodes"])
    complete &= all(n["os"] == "linux" and n["arch"] in ("amd64", "arm64") for n in s["nodes"])
    complete &= all(n[k]["value"] is not None and fresh(n[k]["observedAt"], 90000)
                    for n in s["nodes"] for k in ("cpu", "memory", "exporter"))
    complete &= all(w["ready"] is not None and w["desired"] is not None and fresh(w["observedAt"], 75000) for w in s["workloads"])
    p = s["policy"]
    complete &= all(p[k] is not None for k in ("mode", "enabled", "monitoring", "incidentCount", "activeIncidentCount")) and fresh(p["observedAt"], 75000)
    healthy = all(n["readiness"] == "Ready" and n["exporter"]["value"] == 1 for n in s["nodes"])
    healthy &= all(w["desired"] is not None and w["desired"] > 0 and w["ready"] == w["desired"] for w in s["workloads"])
    healthy &= p["mode"] == "ObserveOnly" and p["enabled"] is True and p["monitoring"] is True and p["activeIncidentCount"] == 0
    return ("current", "healthy" if healthy else "degraded") if complete else ("partial", "unknown")


def canonical(collector, key_id, seq, sent, raw):
    return "\n".join(("OPSLOOP-STATUS-V1", ORIGIN, "POST", PATH, collector, key_id,
                      str(seq), str(sent), hashlib.sha256(raw).hexdigest())).encode()


def sign(key, collector, key_id, seq, sent, raw):
    return hmac.new(key, canonical(collector, key_id, seq, sent, raw), hashlib.sha256).hexdigest()


def unique_pairs(pairs):
    out = {}
    for k, v in pairs:
        if k in out:
            raise ValueError("duplicate JSON key")
        out[k] = v
    return out


def accept(connection, *, key, signature, seq, sent, raw, now,
           collector="home-v1", key_id="test-key", origin=ORIGIN, path=PATH, method="POST"):
    if (origin, path, method, collector, key_id) != (ORIGIN, PATH, "POST", "home-v1", "test-key"):
        raise ValueError("unapproved route/identity")
    if type(seq) is not int or not 0 < seq <= MAX_INT or type(sent) is not int or not 0 < sent <= MAX_INT:
        raise ValueError("invalid request counter/time")
    if not -5000 <= now-sent <= 120000 or len(raw) > MAX_BODY:
        raise ValueError("expired/future/oversize request")
    if not hmac.compare_digest(signature, sign(key, collector, key_id, seq, sent, raw)):
        raise ValueError("authentication failed")
    data = json.loads(raw.decode("utf8"), object_pairs_hook=unique_pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))
    validate(data)
    if not -5000 <= sent-data["collectedAt"] <= 30000:
        raise ValueError("delayed/future collection envelope")
    # One conditional primary-database statement: do not SELECT then blind write.
    row = connection.execute("""
        UPDATE collector_current SET seq=?, sent_at_ms=?, collected_at_ms=?,
          received_at_ms=?, snapshot_json=?
        WHERE collector_id=? AND seq<? AND received_at_ms<=?
          AND collected_at_ms<=?
        RETURNING seq
    """, (seq, sent, data["collectedAt"], now, raw.decode(), collector, seq,
          now-15000, data["collectedAt"])).fetchone()
    connection.commit()
    if row is None:
        raise ValueError("replay/order/rate or unknown collector")
    return data


if __name__ == "__main__":
    target = Path(__file__).resolve().parents[1] / "public-status.schema.json"
    target.write_text(json.dumps(SCHEMA, indent=2) + "\n")
