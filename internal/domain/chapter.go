package domain

import (
	"fmt"
	"unicode/utf8"
)

// ReviewInterval 全局审阅间隔（每 N 章触发一次）。
const ReviewInterval = 5

// ShouldReview 根据已完成章节数判断是否需要全局审阅（短篇/中篇模式）。
func ShouldReview(completedCount int) (bool, string) {
	if completedCount > 0 && completedCount%ReviewInterval == 0 {
		return true, fmt.Sprintf("Đã hoàn thành %d chương, kích hoạt đánh giá toàn cục", completedCount)
	}
	return false, ""
}

// ShouldArcReview 长篇模式下判断是否需要弧级/卷级评审。
func ShouldArcReview(isArcEnd, isVolumeEnd bool, volume, arc int) (bool, string) {
	if isVolumeEnd {
		return true, fmt.Sprintf("Tập %d cung %d kết thúc (cuối tập), kích hoạt đánh giá cấp cung + cấp tập", volume, arc)
	}
	if isArcEnd {
		return true, fmt.Sprintf("Tập %d cung %d kết thúc, kích hoạt đánh giá cấp cung", volume, arc)
	}
	return false, ""
}

// WordCount 按 rune 计算字数。
func WordCount(content string) int {
	return utf8.RuneCountInString(content)
}
