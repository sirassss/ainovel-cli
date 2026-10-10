// Package exp implements export of completed chapters.
//
// Symmetric with imp/: pure local IO, no dependence on the LLM, no change to store state. Export can run
// concurrently with the Engine (it only reads Progress + the final chapter drafts), and is a cross-cutting capability.
//
// TXT and EPUB are currently supported.
package exp

import "github.com/voocel/ainovel-cli/internal/store"

// Format identifies the export format.
type Format string

const (
	// FormatTXT is plain-text output.
	FormatTXT Format = "txt"
	// FormatEPUB is a standard EPUB 3 container (zip + xhtml).
	FormatEPUB Format = "epub"
)

// Options controls export behavior. The zero value is equivalent to "export the whole book to the default path, error if the file exists".
//
// Layout: "book title" → volume divider → chapter body. Two kinds of internal data do not enter the export: premise (a creative blueprint,
// containing backstage meta-information such as target readers / core selling points / writing taboos, for the author and engine, not a preface for readers);
// arc dividers (from the reader's view an arc is an overly fine internal structure). The book title and volume dividers are always kept.
type Options struct {
	// Format: when an empty string, it is inferred from the OutPath suffix (.txt → TXT, .epub → EPUB);
	// when OutPath is also empty it falls back to FormatTXT. SDK callers can specify it explicitly to skip inference.
	Format Format

	// OutPath is the output file path; empty means {novelDir}/{BookMetadata.Title}.{ext}.
	OutPath string

	// From / To is the chapter range, a closed interval. 0 means from chapter 1 / to the last chapter.
	// Unfinished chapters within the range are skipped and written to Result.Skipped, not treated as errors.
	From, To int

	// Overwrite is whether to overwrite when the file exists; the default is to refuse.
	Overwrite bool
}

// Deps are the dependencies Run needs. Only store; export needs no LLM, prompt or bundle.
type Deps struct {
	Store *store.Store
}

// Result is the summary of the artifacts of one successful export.
type Result struct {
	// Path is the file path actually written (absolute, or relative as passed by the caller).
	Path string
	// Chapters is the number of chapters actually written.
	Chapters int
	// Bytes is the file size in bytes (UTF-8).
	Bytes int
	// Skipped lists the chapter numbers that fall within the requested range but are unfinished.
	Skipped []int
}
