#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def artifact(path: Path, role: str) -> dict:
    return {"path": str(path.relative_to(ROOT)), "role": role, "sha256": sha256(path)}


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def release_manifest(environment: dict) -> tuple[Path, str]:
    files = [
        ROOT / "go.mod",
        ROOT / "go.sum",
        ROOT / "internal/pdf/parser.go",
        ROOT / "qualification/pdf-gate/go/go.mod",
        ROOT / "qualification/pdf-gate/go/go.sum",
        ROOT / "qualification/pdf-gate/go/main.go",
        ROOT / "qualification/pdf-gate/generate_fixtures.py",
        ROOT / "output/pdf/syncbase-pdf-corpus-v1/manifest.json",
    ]
    manifest = {
        "candidate": "go-pdfium-webassembly-v1.19.6",
        "environment": environment,
        "files": [artifact(path, "release-input") for path in files],
        "go_pdf": "github.com/klippa-app/go-pdfium@v1.19.6/webassembly",
        "schema_version": 2,
        "source_revision": "NO_VCS",
    }
    path = ROOT / "build/qualification/pdf-gate/release-manifest-go-pdfium.json"
    write_json(path, manifest)
    return path, sha256(path)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--raw",
        type=Path,
        default=ROOT / "build/reports/syncbase/pdf-gate/go-pdfium-wasm-v1.19.6.json",
    )
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    raw_path = args.raw.resolve()
    raw = json.loads(raw_path.read_text(encoding="utf-8"))
    if raw.get("candidate") != "go-pdfium-webassembly-v1.19.6":
        raise RuntimeError("raw evidence is not the production Go PDFium candidate")
    environment = raw["environment"]
    release_path, release_hash = release_manifest(environment)
    checks = []
    for item in raw["results"]:
        checks.append(
            {
                "actual": {
                    "document_sha256": item.get("document_sha256"),
                    "failure_count": len(item["failures"]),
                    "observed_pages": item["observed_pages"],
                    "observed_page_sha256": item["observed_page_sha256"],
                },
                "expectation": item["expectation"],
                "id": f"pdf:{item['id']}",
                "verdict": item["verdict"],
            }
        )
    run_id = (
        f"{raw['started_at'][:10]}-{environment['os']}-{environment['arch']}-"
        "go-pdfium-wasm-v1.19.6"
    ).lower().replace(" ", "-")
    output = args.output or ROOT / f"evidence/gq/GQ-2/{run_id}/result.json"
    result = {
        "artifacts": [
            artifact(raw_path, "raw-production-parser-evidence"),
            artifact(release_path, "release-manifest"),
            artifact(ROOT / "output/pdf/syncbase-pdf-corpus-v1/manifest.json", "fixture-manifest"),
        ],
        "candidate": raw["candidate"],
        "checks": checks,
        "ended_at": raw["finished_at"],
        "environment": environment,
        "evidence_validity": "VALID",
        "fixture_id": raw["fixture_id"],
        "fixture_manifest_sha256": raw["fixture_manifest_sha256"],
        "gate_id": "GQ-2",
        "iterations": raw["iterations"],
        "metrics": {"parser_elapsed_ms": raw.get("total_elapsed_ms")},
        "overall_verdict": raw["overall_verdict"],
        "raw_result_sha256": sha256(raw_path),
        "release_manifest_sha256": release_hash,
        "run_id": run_id,
        "schema_version": 2,
        "source_revision": "NO_VCS",
        "started_at": raw["started_at"],
    }
    canonical = json.dumps(result, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode()
    result["result_sha256"] = hashlib.sha256(canonical).hexdigest()
    write_json(output, result)
    print(output)


if __name__ == "__main__":
    main()
