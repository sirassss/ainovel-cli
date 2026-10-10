package domain

import (
	"fmt"
	"unicode/utf8"
)

// ReviewInterval is the global review interval (triggered once every N chapters).
const ReviewInterval = 5

// ShouldReview decides from the number of completed chapters whether a global review is needed (short / medium-length mode).
func ShouldReview(completedCount int) (bool, string) {
	if completedCount > 0 && completedCount%ReviewInterval == 0 {
		return true, fmt.Sprintf("Đã hoàn thành %d chương, kích hoạt đánh giá toàn cục", completedCount)
	}
	return false, ""
}

// ShouldArcReview decides in long-form mode whether an arc-level / volume-level review is needed.
func ShouldArcReview(isArcEnd, isVolumeEnd bool, volume, arc int) (bool, string) {
	if isVolumeEnd {
		return true, fmt.Sprintf("Tập %d cung %d kết thúc (cuối tập), kích hoạt đánh giá cấp cung + cấp tập", volume, arc)
	}
	if isArcEnd {
		return true, fmt.Sprintf("Tập %d cung %d kết thúc, kích hoạt đánh giá cấp cung", volume, arc)
	}
	return false, ""
}

// WordCount counts words by rune.
func WordCount(content string) int {
	return utf8.RuneCountInString(content)
}
