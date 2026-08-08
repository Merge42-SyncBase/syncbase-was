#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import unicodedata
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont
from pypdf import PdfReader, PdfWriter
from reportlab.lib.pagesizes import A4
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.ttfonts import TTFont
from reportlab.pdfgen import canvas


FIXTURE_ID = "syncbase-pdf-corpus-v1"
FONT_PATH = Path("/System/Library/Fonts/Supplemental/AppleGothic.ttf")


def normalize(text: str) -> str:
    normalized = unicodedata.normalize("NFC", text).replace("\r\n", "\n").replace("\r", "\n").replace("\x00", "")
    return "\n".join(line.strip() for line in normalized.splitlines() if line.strip())


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_text(value: str) -> str:
    return sha256_bytes(value.encode("utf-8"))


def write_text_pdf(path: Path, pages: list[list[str]], *, columns: bool = False) -> list[str]:
    pdf = canvas.Canvas(str(path), pagesize=A4, invariant=1, pageCompression=1)
    pdf.setAuthor("SyncBase")
    pdf.setCreator("SyncBase PDF Gate")
    pdf.setTitle(path.stem)
    expected: list[str] = []
    for lines in pages:
        pdf.setFont("AppleGothic", 11)
        if columns:
            half = len(lines) // 2
            left, right = lines[:half], lines[half:]
            y = A4[1] - 72
            output_lines: list[str] = []
            for index in range(max(len(left), len(right))):
                row: list[str] = []
                if index < len(left):
                    pdf.drawString(54, y, left[index])
                    row.append(left[index])
                if index < len(right):
                    pdf.drawString(320, y, right[index])
                    row.append(right[index])
                output_lines.append(" ".join(row))
                y -= 26
            expected.append(normalize("\n".join(output_lines)))
        else:
            y = A4[1] - 72
            for line in lines:
                pdf.drawString(54, y, line)
                y -= 26
            expected.append(normalize("\n".join(lines)))
        pdf.showPage()
    pdf.save()
    return expected


def write_image_only_pdf(path: Path, temp_dir: Path) -> None:
    image_path = temp_dir / "image-only.png"
    image = Image.new("RGB", (1200, 800), "white")
    draw = ImageDraw.Draw(image)
    font = ImageFont.truetype(str(FONT_PATH), 34)
    draw.text((70, 300), "이 문장은 이미지이며 추출 가능한 PDF 텍스트가 아닙니다.", fill="black", font=font)
    image.save(image_path)
    pdf = canvas.Canvas(str(path), pagesize=A4, invariant=1, pageCompression=1)
    pdf.drawImage(str(image_path), 45, 220, width=500, height=333, preserveAspectRatio=True)
    pdf.showPage()
    pdf.save()


def write_encrypted_pdf(path: Path, temp_dir: Path) -> None:
    plain = temp_dir / "encrypted-source.pdf"
    write_text_pdf(plain, [["암호화된 PDF는 SyncBase MVP에서 지원하지 않습니다."]])
    reader = PdfReader(str(plain))
    writer = PdfWriter()
    for page in reader.pages:
        writer.add_page(page)
    writer.encrypt("syncbase-secret")
    with path.open("wb") as output:
        writer.write(output)


def write_corrupt_pdf(path: Path, source: Path) -> None:
    data = source.read_bytes()
    path.write_bytes(data[: max(128, len(data) // 2)])


def write_size_boundary_pdf(path: Path, temp_dir: Path) -> list[str]:
    source = temp_dir / "size-source.pdf"
    expected = write_text_pdf(source, [["95MiB 입력 경계 PDF", "페이지 근거는 1페이지를 유지해야 합니다."]])
    reader = PdfReader(str(source))
    writer = PdfWriter()
    writer.add_page(reader.pages[0])
    writer.add_metadata({"/SyncBasePadding": "X" * (95 * 1024 * 1024)})
    with path.open("wb") as output:
        writer.write(output)
    if path.stat().st_size >= 100 * 1024 * 1024:
        raise RuntimeError(f"size boundary fixture exceeds 100MiB: {path.stat().st_size}")
    return expected


def add_fixture(fixtures: list[dict], root: Path, fixture_id: str, file_name: str, expected: list[str] | None) -> None:
    path = root / file_name
    fixtures.append(
        {
            "id": fixture_id,
            "file": file_name,
            "file_sha256": sha256_bytes(path.read_bytes()),
            "size_bytes": path.stat().st_size,
            "expectation": "TEXT_PDF" if expected is not None else "INVALID_INPUT",
            "page_sha256": [sha256_text(page) for page in expected] if expected is not None else [],
        }
    )


def generate(output: Path, temp_dir: Path) -> None:
    if not FONT_PATH.exists():
        raise RuntimeError(f"required font is missing: {FONT_PATH}")
    output.mkdir(parents=True, exist_ok=True)
    temp_dir.mkdir(parents=True, exist_ok=True)
    pdfmetrics.registerFont(TTFont("AppleGothic", str(FONT_PATH)))
    fixtures: list[dict] = []

    documents: list[tuple[str, str, list[list[str]], bool]] = [
        ("ko-policy", "ko-policy.pdf", [["정보보안 정책", "비밀번호는 90일마다 변경합니다.", "최신 개정일: 2026-07-22"]], False),
        ("ko-contract", "ko-contract.pdf", [["계약 검토 지침", "전자서명과 보존 기간을 확인합니다.", "담당 부서: 법무팀"]], False),
        ("ko-operations", "ko-operations.pdf", [["운영 장애 대응", "Primary 전환 후 검색을 다시 확인합니다.", "커밋 데이터 유실은 허용하지 않습니다."]], False),
        ("ko-punctuation", "ko-punctuation.pdf", [["문장부호 검증: 쉼표, 마침표, 괄호(테스트).", "금액 1,234,567원과 날짜 2026-07-22를 보존합니다."]], False),
        ("en-guide", "en-guide.pdf", [["SyncBase Operations Guide", "The active document version remains searchable.", "Evidence includes the exact PDF page."]], False),
        ("en-release", "en-release.pdf", [["Release Checklist", "Pin every runtime and model artifact.", "Reject unverified database capabilities."]], False),
        ("mixed-security", "mixed-security.pdf", [["보안 정책 Security Policy", "MCP token은 로그에 기록하지 않습니다.", "RTO target: 30 seconds"]], False),
        ("mixed-product", "mixed-product.pdf", [["SyncBase 문서 자동화", "Upload - Parse - Chunk - Embed - Search", "최신 version만 검색합니다."]], False),
        ("columns-ko", "columns-ko.pdf", [["왼쪽 항목 1", "왼쪽 항목 2", "왼쪽 항목 3", "오른쪽 항목 A", "오른쪽 항목 B", "오른쪽 항목 C"]], True),
        ("columns-mixed", "columns-mixed.pdf", [["문서 ID", "버전 번호", "페이지", "document_id", "version", "page_number"]], True),
    ]
    for fixture_id, file_name, pages, columns in documents:
        expected = write_text_pdf(output / file_name, pages, columns=columns)
        add_fixture(fixtures, output, fixture_id, file_name, expected)

    boundary_expected = write_size_boundary_pdf(output / "size-95mib.pdf", temp_dir)
    add_fixture(fixtures, output, "size-95mib", "size-95mib.pdf", boundary_expected)

    page_lines = [[f"페이지 {page:03d} - SyncBase 500페이지 경계 검증"] for page in range(1, 501)]
    page_expected = write_text_pdf(output / "pages-500.pdf", page_lines)
    add_fixture(fixtures, output, "pages-500", "pages-500.pdf", page_expected)

    write_encrypted_pdf(output / "invalid-encrypted.pdf", temp_dir)
    add_fixture(fixtures, output, "invalid-encrypted", "invalid-encrypted.pdf", None)

    write_corrupt_pdf(output / "invalid-corrupt.pdf", output / "ko-policy.pdf")
    add_fixture(fixtures, output, "invalid-corrupt", "invalid-corrupt.pdf", None)

    write_image_only_pdf(output / "invalid-image-only.pdf", temp_dir)
    add_fixture(fixtures, output, "invalid-image-only", "invalid-image-only.pdf", None)

    manifest = {
        "fixture_id": FIXTURE_ID,
        "generator": "qualification/pdf-gate/generate_fixtures.py",
        "font": {"path": str(FONT_PATH), "sha256": sha256_bytes(FONT_PATH.read_bytes())},
        "iterations": 20,
        "normalization": "NFC; CRLF/CR to LF; strip lines; drop empty lines; join with LF",
        "fixtures": fixtures,
    }
    (output / "manifest.json").write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--temp", type=Path, required=True)
    args = parser.parse_args()
    if args.temp.exists():
        shutil.rmtree(args.temp)
    generate(args.output, args.temp)


if __name__ == "__main__":
    main()
