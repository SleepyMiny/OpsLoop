"""Fail-closed Demo B evidence handoff; no cluster or model credentials."""

from .seal import EvidenceError, build_bundle, seal_bundle, verify_sealed

__all__ = ["EvidenceError", "build_bundle", "seal_bundle", "verify_sealed"]
