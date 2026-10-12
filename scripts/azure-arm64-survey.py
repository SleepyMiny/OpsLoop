#!/usr/bin/env python3
"""Read-only Azure ARM64 survey. No cloud mutations; exit 2 means blocked/unknown.

Only filtered nonsecret JSON is cached, partitioned by a hash of the subscription
ID. Azure CLI may maintain its normal authentication/session files; this script
never reads tokens. Catalog/quota checks cannot prove allocation capacity.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time

REGIONS = ["koreacentral", "japaneast", "japanwest"]
SKUS = ["Standard_B2pls_v2", "Standard_B2ps_v2", "Standard_D2pls_v6",
        "Standard_D2pls_v5", "Standard_D2plds_v6", "Standard_D2ps_v6", "Standard_D2ps_v5"]
PROVIDERS = ["Microsoft.Compute", "Microsoft.Network", "Microsoft.Storage", "Microsoft.ContainerRegistry"]
FAMILIES = ["cores", "virtualMachines", "standardBpsv2Family", "StandardDplsv6Family",
            "standardDPLSv5Family", "StandardDpldsv6Family", "StandardDpsv6Family", "standardDPSv5Family"]
READ_COMMANDS = {("account", "show"), ("provider", "list"),
                 ("vm", "list-usage"), ("vm", "list-skus"), ("vm", "image", "show")}


def utc_now():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def az_json(args):
    if not any(tuple(args[:len(prefix)]) == prefix for prefix in READ_COMMANDS):
        raise ValueError("Only allowlisted Azure read commands are supported")
    try:
        result = subprocess.run(["az", *args, "--only-show-errors", "--output", "json"],
                                capture_output=True, text=True, timeout=180, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise RuntimeError("Azure read command unavailable or timed out") from None
    if result.returncode:
        # CLI stderr can contain subscription/resource identifiers. Do not publish it.
        raise RuntimeError("Azure read command failed; inspect Azure CLI locally")
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError:
        raise RuntimeError("Azure read command returned invalid JSON") from None


def cached_read(cache_dir, args, ttl, refresh):
    key = hashlib.sha256(json.dumps(args).encode()).hexdigest()
    path = cache_dir / (key + ".json")
    if not refresh:
        try:
            saved = json.loads(path.read_text())
            age = time.time() - saved["epoch"]
            if 0 <= age <= ttl:
                return {"data": saved["data"], "observed_at": saved["observed_at"], "cached": True}
        except (OSError, ValueError, KeyError, TypeError):
            pass
    stamp = utc_now()
    try:
        data = az_json(args)
    except RuntimeError as error:
        return {"data": None, "observed_at": stamp, "cached": False, "error": str(error)}
    saved = {"epoch": time.time(), "observed_at": stamp, "data": data}
    try:
        path.write_text(json.dumps(saved, indent=2) + "\n")
        path.chmod(0o600)
    except OSError:
        return {"data": data, "observed_at": stamp, "cached": False, "error": "Observation succeeded but cache could not be written"}
    return {"data": data, "observed_at": stamp, "cached": False}


def quota_check(usages, name, needed):
    item = next((u for u in (usages or []) if str(u.get("name", "")).casefold() == name.casefold()), None)
    if item is None:
        return {"status": "unknown", "reason": "quota entry absent"}
    try:
        current, limit = int(item["current"]), int(item["limit"])
    except (KeyError, TypeError, ValueError):
        return {"status": "unknown", "reason": "quota values invalid"}
    return {"status": "pass" if limit-current >= needed else "blocked",
            "current": current, "limit": limit, "headroom": limit-current, "required": needed}


def restriction_check(sku, region, zone):
    blocked, excluded_zones, unknown = [], set(), False
    for restriction in sku.get("restrictions", []):
        info = restriction.get("restrictionInfo") or {}
        kind = restriction.get("type")
        locations = info.get("locations") or (restriction.get("values", []) if kind == "Location" else [])
        if locations and region.casefold() not in [str(v).casefold() for v in locations]:
            continue
        if kind == "Location":
            blocked.append("location restriction: " + str(restriction.get("reasonCode", "unknown")))
        elif kind == "Zone":
            zones = info.get("zones") or restriction.get("values", [])
            excluded_zones.update(str(z) for z in zones)
            if zone and str(zone) in excluded_zones:
                blocked.append("requested zone restricted")
            if not zones:
                unknown = True
        else:
            unknown = True
    catalog_zones = sorted({str(z) for loc in sku.get("locationInfo", [])
                            if str(loc.get("location", "")).casefold() == region.casefold()
                            for z in loc.get("zones", [])})
    if zone and str(zone) not in catalog_zones:
        blocked.append("requested zone absent from SKU catalog")
    return {"status": "blocked" if blocked else "unknown" if unknown else "pass",
            "reasons": blocked, "catalog_zones": catalog_zones,
            "restricted_zones": sorted(excluded_zones), "requested_zone": zone}


def candidate_check(sku, region, usages, image, providers, zone, disk_gib):
    capabilities = {c["name"]: c.get("value") for c in sku.get("capabilities", [])}
    checks = {}
    checks["architecture"] = "pass" if capabilities.get("CpuArchitectureType", "").casefold() == "arm64" else "blocked"
    checks["generation"] = "pass" if "V2" in capabilities.get("HyperVGenerations", "").split(",") else "blocked"
    try:
        cpus = int(capabilities["vCPUs"])
    except (KeyError, TypeError, ValueError):
        cpus = None
    try:
        checks["minimum_resources"] = "pass" if cpus is not None and cpus >= 2 and float(capabilities["MemoryGB"]) >= 4 else "blocked"
    except (KeyError, TypeError, ValueError):
        checks["minimum_resources"] = "unknown"
    checks["accelerated_networking"] = ("pass" if capabilities.get("AcceleratedNetworkingEnabled") == "True" else
                                         "blocked" if capabilities.get("AcceleratedNetworkingEnabled") == "False" else "unknown")
    checks["regional_quota"] = quota_check(usages, "cores", cpus) if cpus else {"status": "unknown"}
    checks["family_quota"] = quota_check(usages, sku.get("family", ""), cpus) if cpus else {"status": "unknown"}
    checks["vm_count_quota"] = quota_check(usages, "virtualMachines", 1)
    checks["restrictions"] = restriction_check(sku, region, zone)
    if not image:
        checks["image"] = "unknown"
    else:
        checks["image"] = "pass" if (str(image.get("architecture", "")).casefold() == "arm64" and
                                      image.get("generation") == "V2" and image.get("plan") is None) else "blocked"
    try:
        checks["disk_size"] = "pass" if disk_gib >= int(image["disk_gib"]) else "blocked"
    except (KeyError, TypeError, ValueError):
        checks["disk_size"] = "unknown"
    registered = {p.get("namespace"): p.get("state") for p in (providers or [])}
    checks["providers"] = {p: ("pass" if registered.get(p) == "Registered" else
                                 "unknown" if p not in registered else "blocked") for p in PROVIDERS}
    statuses = [v["status"] if isinstance(v, dict) and "status" in v else v
                for k, v in checks.items() if k != "providers"] + list(checks["providers"].values())
    return {"sku": sku["name"], "family": sku.get("family"), "capabilities": capabilities,
            "checks": checks, "read_checks_status": "blocked" if "blocked" in statuses else
            "unknown" if "unknown" in statuses else "pass",
            "capacity": "unverified; catalog and quota do not guarantee allocation"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--region", action="append", help="Repeat for regions; default Korea Central/Japan East/Japan West")
    parser.add_argument("--subscription", help="Explicit existing subscription name or ID; never changes az default")
    parser.add_argument("--zone", choices=["1", "2", "3"], help="Optional selected zone; omission means regional deployment")
    parser.add_argument("--image-version", default="24.04.202609040")
    parser.add_argument("--disk-gib", type=int, default=32)
    parser.add_argument("--cache-dir", type=Path, default=Path(".opsloop/azure-survey"))
    parser.add_argument("--cache-ttl-seconds", type=int, default=900)
    parser.add_argument("--refresh", action="store_true", help="Bypass cached observations before deployment review")
    parser.add_argument("--output", type=Path, help="Optional distilled JSON destination; no subscription IDs")
    options = parser.parse_args()
    regions = options.region or REGIONS
    if any(not re.fullmatch(r"[a-z0-9]+", r) for r in regions):
        parser.error("Use Azure region identifiers such as koreacentral")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", options.image_version):
        parser.error("An exact numeric image version is required; latest is forbidden")
    if options.disk_gib <= 0 or options.cache_ttl_seconds < 0:
        parser.error("disk must be positive; cache TTL must be nonnegative")
    account_args = ["account", "show"]
    if options.subscription:
        account_args += ["--subscription", options.subscription]
    account_args += ["--query", "{id:id,name:name,state:state,isDefault:isDefault}"]
    try:
        account = az_json(account_args)
        subscription_id = account.pop("id")
    except (RuntimeError, KeyError) as error:
        print(json.dumps({"deployment_gate": False, "error": str(error)}))
        return 2
    cache = options.cache_dir / hashlib.sha256(subscription_id.encode()).hexdigest()[:16]
    try:
        cache.mkdir(parents=True, exist_ok=True, mode=0o700)
        cache.chmod(0o700)
    except OSError:
        print(json.dumps({"deployment_gate": False, "error": "Cache directory is not writable"}))
        return 2
    def read(args):
        return cached_read(cache, args + ["--subscription", subscription_id], options.cache_ttl_seconds, options.refresh)
    providers = read(["provider", "list", "--query", "[?" + " || ".join("namespace=='"+p+"'" for p in PROVIDERS) + "].{namespace:namespace,state:registrationState}"])
    urn = "Canonical:ubuntu-24_04-lts:server-arm64:" + options.image_version
    def region_read(region):
        usage = read(["vm", "list-usage", "--location", region, "--query", "[?" + " || ".join("name.value=='"+f+"'" for f in FAMILIES) + "].{name:name.value,label:name.localizedValue,current:currentValue,limit:limit}"])
        skus = read(["vm", "list-skus", "--location", region, "--resource-type", "virtualMachines", "--all", "--query", "[?" + " || ".join("name=='"+s+"'" for s in SKUS) + "].{name:name,family:family,locationInfo:locationInfo,capabilities:capabilities,restrictions:restrictions}"])
        image = read(["vm", "image", "show", "--location", region, "--urn", urn, "--query", "{version:name,architecture:architecture,generation:hyperVGeneration,plan:plan,disk_gib:osDiskImage.sizeInGb}"])
        candidates = [candidate_check(s, region, usage["data"], image["data"], providers["data"], options.zone, options.disk_gib) for s in (skus["data"] or [])]
        missing = sorted(set(SKUS) - {s["sku"] for s in candidates})
        return {"region": region, "quota": usage, "image": image, "sku_observed_at": skus["observed_at"],
                "sku_cached": skus["cached"], "sku_error": skus.get("error"), "missing_skus": missing, "candidates": candidates}
    with ThreadPoolExecutor(max_workers=3) as pool:
        results = list(pool.map(region_read, regions))
    report = {"generated_at": utc_now(), "subscription": account, "providers": providers,
              "image_urn": urn, "disk_gib": options.disk_gib, "regions": results,
              "deployment_gate": False,
              "remaining_checks": ["actual allocation capacity", "policy and RBAC", "subscription credit and expiry",
                                   "cost ceiling and cleanup ownership", "ACR name availability", "remote backend readiness",
                                   "network/CIDR and bootstrap approvals"],
              "note": "Read-only observations are prerequisites; never deployment authorization. Cached data retains original timestamp."}
    payload = json.dumps(report, indent=2) + "\n"
    if options.output:
        try:
            options.output.parent.mkdir(parents=True, exist_ok=True)
            options.output.write_text(payload)
        except OSError:
            print(json.dumps({"deployment_gate": False, "error": "Output destination is not writable"}))
            return 2
    else:
        sys.stdout.write(payload)
    # Unknown remaining checks always keep preflight nonzero, even if catalog checks pass.
    return 2


if __name__ == "__main__":
    sys.exit(main())
