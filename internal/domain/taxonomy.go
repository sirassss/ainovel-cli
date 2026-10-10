package domain

import "slices"

var (
	hookTypes       = []string{"crisis", "mystery", "desire", "emotion", "choice"}
	dominantStrands = []string{"quest", "fire", "constellation"}
)

// HookTypes returns the chapter hook categories. It returns a copy, so callers cannot modify the domain vocabulary.
func HookTypes() []string { return slices.Clone(hookTypes) }

// DominantStrands returns the categories of a chapter's dominant narrative strand.
func DominantStrands() []string { return slices.Clone(dominantStrands) }

func ValidHookType(value string) bool       { return slices.Contains(hookTypes, value) }
func ValidDominantStrand(value string) bool { return slices.Contains(dominantStrands, value) }
