# SyncBase Go PDFium Gate

프로덕션과 동일한 Go `go-pdfium v1.19.6` WebAssembly parser를 `syncbase-pdf-corpus-v1` 전체에 적용한다. 페이지별 정규화 text SHA-256, 20회 결정성, 암호화·손상·이미지 전용 PDF 거부, 100MiB/500페이지 경계를 검증한다.

`go -C qualification/pdf-gate/go test ./...`는 저장소에 포함된 최소
`testdata`만 사용하므로 새 clone과 CI에서도 독립적으로 실행된다. 전체 15개
corpus Gate는 아래 생성 절차를 사용한다.

## Fixture 생성

```sh
python3 qualification/pdf-gate/generate_fixtures.py \
  --output output/pdf/syncbase-pdf-corpus-v1 \
  --temp tmp/pdfs/syncbase-pdf-corpus-v1
```

## 프로덕션 후보 실행

```sh
go -C qualification/pdf-gate/go run . \
  -manifest ../../../output/pdf/syncbase-pdf-corpus-v1/manifest.json \
  -output ../../../build/reports/syncbase/pdf-gate/go-pdfium-wasm-v1.19.6.json
```

`-iterations 1`은 harness smoke에만 사용한다. Gate 판정은 manifest의 20회를 사용한다.

## 증거 봉인

```sh
python3 qualification/pdf-gate/finalize_evidence.py
```

봉인기는 Java/PDFBox 또는 폐기된 `ledongthuc/pdf` 결과를 읽지 않는다. raw evidence의 OS/arch를 그대로 기록하므로 macOS 결과를 Linux 증거로 승격할 수 없다. Linux/amd64의 GQ-2는 해당 환경에서 위 두 명령을 다시 실행해야 PASS가 된다.

현재 프로덕션 후보의 로컬 20회 corpus 결과는 `15/15 PASS`다. 운영 승인에는 별도의 Linux/amd64 실행 증거가 필요하다.
