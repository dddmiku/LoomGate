package taskengine

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSafeSummaryTruncationIsBoundedAndIdempotent(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("查", 200), strings.Repeat("查", 1000),
		strings.Repeat("单", 168) + "xyz", strings.Repeat("单", 169) + "中文",
		strings.Repeat("a ", 255), strings.Repeat("a ", 256), strings.Repeat("a ", 257),
		strings.Repeat("a ", 256) + "中", strings.Repeat("中a ", 200),
	} {
		once := safeStepSummary(text, "", "")
		if len(once) > 512 || !utf8.ValidString(once) {
			t.Fatalf("summary byte bound/UTF-8 invalid: bytes=%d", len(once))
		}
		if twice := safeStepSummary(once, "", ""); twice != once {
			t.Fatal("summary changed after a second normalization")
		}
	}
}
