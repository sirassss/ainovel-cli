package domain

import (
	"strings"
	"testing"
)

func TestOversizedArcMessageIsVietnamese(t *testing.T) {
	msg := OversizedArc("Tập 1 cung 1", 10)
	if msg == "" || !strings.Contains(msg, "10") || !strings.Contains(msg, "8") {
		t.Fatalf("thông báo phải nêu số chương vượt và giới hạn: %q", msg)
	}
	if strings.IndexFunc(msg, func(r rune) bool { return r >= 0x3000 }) >= 0 {
		t.Fatalf("thông báo không được chứa chữ Hán/dấu full-width: %q", msg)
	}
	if OversizedArc("x", 8) != "" {
		t.Fatal("đúng 8 chương phải hợp lệ")
	}
}
