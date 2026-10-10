package tui

import (
	"errors"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
)

func TestBootstrapExistingBookFailureStaysInWorkbench(t *testing.T) {
	m := Model{mode: modeNew, textarea: textarea.New()}
	next, cmd, handled := m.handleRuntimeMsg(bootstrapMsg{existing: true, err: errors.New("迁移失败")})
	if !handled || cmd == nil {
		t.Fatal("已有作品恢复失败仍应刷新工作台")
	}
	got := next.(Model)
	if got.mode != modeRunning {
		t.Fatalf("已有作品恢复失败后应留在工作台，得 mode=%v", got.mode)
	}
	if got.err == nil || got.err.Error() != "迁移失败" {
		t.Fatalf("工作台应展示原始错误，得 %v", got.err)
	}
}

// TestBootstrapCompletedBookLandsOnDoneWorkbench guards the startup landing point of a completed book: resumeLabel returns
// an empty label for complete, and the old behavior landed on the welcome screen, which says nothing about an existing book, so users would think the book was lost,
// and the natural home of /reopen, /export and rework input is the completed-state workbench.
func TestBootstrapCompletedBookLandsOnDoneWorkbench(t *testing.T) {
	m := Model{mode: modeNew, textarea: textarea.New()}
	next, cmd, handled := m.handleRuntimeMsg(bootstrapMsg{completed: true})
	if !handled || cmd == nil {
		t.Fatal("completed bootstrap 应被处理并返回命令")
	}
	got := next.(Model)
	if got.mode != modeDone {
		t.Fatalf("完结书应落完成态工作台，得 mode=%v", got.mode)
	}
	if got.textarea.Placeholder != donePlaceholder {
		t.Fatalf("应给出完成态引导（含 /reopen），得 %q", got.textarea.Placeholder)
	}

	// Already on the workbench (e.g. bootstrap arrives after the book completed in-session) must not be switched again.
	m = Model{mode: modeRunning, textarea: textarea.New()}
	next, _, _ = m.handleRuntimeMsg(bootstrapMsg{completed: true})
	if next.(Model).mode != modeRunning {
		t.Fatal("非欢迎页不应被 completed bootstrap 切态")
	}
}
