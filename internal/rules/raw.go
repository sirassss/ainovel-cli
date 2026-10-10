package rules

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RawSource is a raw source awaiting normalization (the whole text of a rules file).
//
// After cutting YAML, a rules file is just a plain natural-language prompt; normalization only needs the original text and no longer parses front matter.
type RawSource struct {
	Label string     // source label, enters Snapshot.Sources (e.g. global:my-style.md)
	Kind  SourceKind // priority tier
	Text  string     // raw file content
}

// RawFileSources enumerates the .md files under the rules directories in Global -> Project order and returns their raw text.
//
// Same scanning conventions as readDirFromDisk (top-level .md, lexicographic order, hidden files skipped), but YAML is not parsed;
// the whole text is handed to the normalizer as-is. System defaults / startup prompt / in-run requirements are provided separately by the service.
func RawFileSources(opts LoadOptions) []RawSource {
	var out []RawSource
	out = append(out, rawDir(opts.HomeRulesDir, SourceGlobal)...)
	out = append(out, rawDir(opts.ProjectRulesDir, SourceProject)...)
	return out
}

func rawDir(dir string, kind SourceKind) []RawSource {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing directory is the normal case and is silently skipped; but errors such as permissions or a path that is actually a file must leave a trace -
		// otherwise the user writes rules that take no effect at all with zero feedback, which is very costly to debug (see known_rules_path_stale_readme).
		if !os.IsNotExist(err) {
			slog.Warn("reading rules directory failed, skipped", "module", "rules", "dir", dir, "err", err)
		}
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var out []RawSource
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("reading rules file failed, skipped", "module", "rules", "file", path, "err", err)
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		out = append(out, RawSource{
			Label: kind.String() + ":" + name,
			Kind:  kind,
			Text:  text,
		})
	}
	return out
}
