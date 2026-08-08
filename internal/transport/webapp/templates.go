package webapp

import (
	"fmt"
	"html/template"
	"time"

	frontend "github.com/Merge42-SyncBase/syncbase-frontend"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

func parseTemplates() (*template.Template, error) {
	functions := template.FuncMap{
		"statusLabel": statusLabel,
		"formatTime":  func(value time.Time) string { return value.Local().Format("2006.01.02 15:04") },
		"formatTimePtr": func(value *time.Time) string {
			if value == nil {
				return ""
			}
			return value.Local().Format("15:04:05")
		},
		"errorLabel": func(code string) string {
			return map[string]string{
				"INVALID_INPUT":       "PDF 형식이나 텍스트 내용을 확인해 주세요.",
				"PROFILE_MISMATCH":    "처리 프로필이 일치하지 않아 운영자 확인이 필요합니다.",
				"TRANSIENT_EXHAUSTED": "일시 오류 자동 재시도를 모두 사용했습니다.",
				"INTERNAL":            "내부 처리 오류입니다. 상관 ID와 함께 운영자에게 문의하세요.",
			}[code]
		},
		"activeVersion": func(value *int) string {
			if value == nil {
				return "—"
			}
			return fmt.Sprintf("v%d", *value)
		},
		"stages": knowledge.ProcessingStages,
		"stageLabel": func(stage knowledge.Stage) string {
			return map[knowledge.Stage]string{
				knowledge.StageMetadata: "확인", knowledge.StageParse: "텍스트 추출",
				knowledge.StageChunk: "문단 분리", knowledge.StageEmbed: "검색 특징",
				knowledge.StageStore: "저장", knowledge.StageActivate: "반영",
			}[stage]
		},
		"stageState": stageState,
		"stageStateLabel": func(state string) string {
			return map[string]string{
				"complete": "완료", "current": "진행 중", "failed": "실패", "pending": "대기",
			}[state]
		},
		"isProcessing": func(status knowledge.VersionStatus) bool {
			return status == knowledge.VersionQueued || status == knowledge.VersionProcessing
		},
	}
	return frontend.Parse(template.New("syncbase").Funcs(functions))
}

func statusLabel(status knowledge.VersionStatus) string {
	return map[knowledge.VersionStatus]string{
		knowledge.VersionQueued:     "처리 대기",
		knowledge.VersionProcessing: "처리 중",
		knowledge.VersionActive:     "검색 가능",
		knowledge.VersionFailed:     "확인 필요",
		knowledge.VersionSuperseded: "이전 버전",
	}[status]
}

func stageState(version knowledge.VersionView, stage knowledge.Stage) string {
	index := func(value knowledge.Stage) int {
		for i, candidate := range knowledge.ProcessingStages() {
			if candidate == value {
				return i
			}
		}
		return 0
	}
	if version.Status == knowledge.VersionFailed && version.Stage == stage {
		return "failed"
	}
	if version.Status == knowledge.VersionActive || version.Status == knowledge.VersionSuperseded || index(stage) < index(version.Stage) {
		return "complete"
	}
	if version.Status == knowledge.VersionProcessing && version.Stage == stage {
		return "current"
	}
	return "pending"
}
