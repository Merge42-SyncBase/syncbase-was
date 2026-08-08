# SyncBase WAS

Go modular monolith의 도메인, PDF 파싱, 문서 처리, PostgreSQL 저장 및 관리자 Web을 소유한다.

- 실행 파일: `cmd/web`, `cmd/worker`, `cmd/migrate`
- 내부 모듈: `internal/modules`
- 변동성 adapter: `internal/adapters`
- MCP에 공개하는 유일한 Go seam: `searchruntime`
- 프론트 및 임베딩 구현은 sibling submodule을 통해 주입된다.

superproject checkout에서 검증한다.

```sh
go test ./...
go vet ./...
```
