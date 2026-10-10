package tui

import (
	"strings"
	"testing"
)

func TestRenderDispatchSummarySplitsAgentFromTask(t *testing.T) {
	cases := []struct{ summary, wantTask string }{
		{"writer (Viết chương 5)", "(Viết chương 5)"},
		{"writer（写第 5 章）", "（写第 5 章）"}, // old event replayed from the log, han:keep
	}
	for _, c := range cases {
		out := renderDispatchSummary(c.summary, 80)
		if !strings.HasPrefix(out, agentDisplayName("writer")) {
			t.Errorf("%q: tên agent phải đứng đầu: %q", c.summary, out)
		}
		if !strings.Contains(out, c.wantTask) {
			t.Errorf("%q: phần nhiệm vụ phải được giữ nguyên chữ hoa/thường: %q", c.summary, out)
		}
	}
}
