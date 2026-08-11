# SyncBase WAS

Go modular monolith의 도메인, PDF 파싱, 문서 처리, PostgreSQL 저장 및 React Web용 JSON API를 소유한다.

- 실행 파일: `cmd/web`(API), `cmd/worker`, `cmd/migrate`
- 내부 모듈: `internal/modules`
- 변동성 adapter: `internal/adapters`
- MCP에 공개하는 유일한 Go seam: `searchruntime`
- 프론트 및 임베딩 구현은 sibling submodule을 통해 주입된다.

superproject checkout에서 검증한다.

```sh
go test ./...
go vet ./...
```

## React Web API

`cmd/web`은 MCP 토큰과 데이터베이스를 브라우저에 노출하지 않는 same-origin
`/api/v1` 계약을 제공한다. 로그인 성공 후 HttpOnly 세션 쿠키가 설정되며, 응답의
`csrfToken`은 이후 변경 요청의 `X-CSRF-Token` 헤더로만 사용한다.

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v1/session` | 관리자 로그인 |
| `GET`, `DELETE` | `/api/v1/session` | 세션 조회·로그아웃 |
| `GET`, `POST` | `/api/v1/documents` | 문서 목록·PDF 등록 |
| `GET` | `/api/v1/documents/{id}` | 문서 및 버전 처리 상태 |
| `POST` | `/api/v1/documents/{id}/versions` | 새 PDF 버전 등록 |
| `POST` | `/api/v1/uploads/preflight` | 서버 PDF 검사·SHA-256·페이지 수 |
| `GET` | `/api/v1/uploads/recovery` | request key 기반 응답 유실 복구 |
| `POST` | `/api/v1/processing-runs/{id}/retry` | 소진된 일시 오류의 재시도 |
| `GET` | `/api/v1/search` | WAS가 MCP `search_documents`를 호출하는 근거 검색 |
| `GET` | `/api/v1/documents/{id}/versions/{n}/source` | 정확한 문서·버전·페이지 근거 메타데이터 |
| `GET` | `/api/v1/documents/{id}/versions/{n}/raw.pdf` | 인증된 원본 PDF |

모든 검색 결과는 `document_id`, `version_id`, `document_version`, `page_number`,
`snippet`, `source_url`을 포함한다. PostgreSQL/OpenSQL 저장소는 활성 버전만 검색하며,
낮은 버전의 늦은 완료는 `SUPERSEDED`로 끝나므로 이전 검색 결과를 되살릴 수 없다.
