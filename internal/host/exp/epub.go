package exp

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// renderEPUB packages the chapter collection into an EPUB 3 byte stream.
//
// Package structure (OEBPS is the OPS package container):
//
//	mimetype                    (must be the first zip entry + Method=Store, uncompressed)
//	META-INF/container.xml      (points to OEBPS/content.opf)
//	OEBPS/content.opf           (metadata + manifest + spine)
//	OEBPS/nav.xhtml             (EPUB 3 navigation)
//	OEBPS/style.css             (minimal typesetting)
//	OEBPS/cover.xhtml           (book title, optional)
//	OEBPS/chapterNNN.xhtml      (one file per chapter)
func renderEPUB(
	lb labels,
	book domain.BookMetadata,
	chapters []int,
	titleIdx chapterTitleIndex,
	locations map[int]chapterLocation,
	bodies map[int]string,
) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// 1. mimetype must be the first zip entry + Store (uncompressed) + exact content with no BOM
	mt, err := zw.CreateHeader(&zip.FileHeader{
		Name:   "mimetype",
		Method: zip.Store,
	})
	if err != nil {
		return nil, fmt.Errorf("create mimetype: %w", err)
	}
	if _, err := mt.Write([]byte("application/epub+zip")); err != nil {
		return nil, err
	}

	if err := zipDeflate(zw, "META-INF/container.xml", containerXML); err != nil {
		return nil, err
	}
	if err := zipDeflate(zw, "OEBPS/style.css", styleCSS); err != nil {
		return nil, err
	}

	hasCover := strings.TrimSpace(book.Title) != ""
	if hasCover {
		if err := zipDeflate(zw, "OEBPS/cover.xhtml", renderCoverXHTML(lb, book.Title)); err != nil {
			return nil, err
		}
	}

	for _, ch := range chapters {
		loc, hasLoc := locations[ch]
		title := strings.TrimSpace(titleIdx[ch])
		body := stripChapterTitleHeader(strings.TrimSpace(bodies[ch]), title)
		xhtml := renderChapterXHTML(lb, ch, title, loc, hasLoc, body)
		if err := zipDeflate(zw, "OEBPS/"+chapterFileName(ch), xhtml); err != nil {
			return nil, err
		}
	}

	if err := zipDeflate(zw, "OEBPS/nav.xhtml", renderNavXHTML(lb, hasCover, chapters, titleIdx)); err != nil {
		return nil, err
	}

	if err := zipDeflate(zw, "OEBPS/content.opf", renderOPF(lb, book, hasCover, chapters)); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finalize zip: %w", err)
	}
	return buf.Bytes(), nil
}

// zipDeflate writes an ordinary (compressed) entry.
func zipDeflate(zw *zip.Writer, name, content string) error {
	w, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	_, err = w.Write([]byte(content))
	return err
}

func chapterFileName(ch int) string {
	return fmt.Sprintf("chapter%03d.xhtml", ch)
}

// chapterID is the id of a manifest item; it corresponds one-to-one with the file name.
func chapterID(ch int) string {
	return fmt.Sprintf("ch%03d", ch)
}

// Fixed templates ────────────────────────────────────────────────

const containerXML = `<?xml version="1.0" encoding="utf-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`

const styleCSS = `body { font-family: serif; line-height: 1.7; margin: 1em; }
h1.book-title { font-size: 2em; text-align: center; margin: 4em 0 1em; }
.volume-divider { font-size: 1.6em; text-align: center; margin: 4em 0 1em; font-weight: bold; }
h1.chapter-title { font-size: 1.4em; text-align: center; margin: 2em 0 1.5em; }
p { text-indent: 2em; margin: 0.5em 0; }
`

// Chapter XHTML ────────────────────────────────────────────────

func renderChapterXHTML(lb labels, ch int, title string, loc chapterLocation, hasLoc bool, body string) string {
	var b strings.Builder
	displayTitle := fmt.Sprintf(lb.chapterFmt, ch)
	if title != "" {
		displayTitle = fmt.Sprintf(lb.chapterTitleFmt, ch, title)
	}

	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xml:lang="%s">
<head>
  <title>%s</title>
  <link rel="stylesheet" type="text/css" href="style.css"/>
</head>
<body>
`, lb.lang, html.EscapeString(displayTitle))

	if hasLoc && loc.IsFirstOfVolume {
		fmt.Fprintf(&b, "  <div class=\"volume-divider\">"+html.EscapeString(lb.volumeFmt)+"</div>\n",
			loc.VolumeIdx, html.EscapeString(strings.TrimSpace(loc.VolumeTitle)))
	}

	fmt.Fprintf(&b, "  <h1 class=\"chapter-title\">%s</h1>\n", html.EscapeString(displayTitle))
	for _, para := range splitParagraphs(body) {
		fmt.Fprintf(&b, "  <p>%s</p>\n", html.EscapeString(para))
	}
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

// splitParagraphs splits into paragraphs on blank lines; multiple consecutive blank lines count as one break. The returned paragraphs are all TrimSpace'd and non-empty.
// A newline inside a paragraph (a single \n) is kept as a space within the paragraph: XHTML <p> does not preserve line breaks, and the browser wraps automatically.
func splitParagraphs(body string) []string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	parts := strings.Split(body, "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// A newline within a paragraph becomes a space, to avoid losing content when XHTML is rendered
		p = strings.ReplaceAll(p, "\n", " ")
		out = append(out, p)
	}
	return out
}

// Cover ────────────────────────────────────────────────

func renderCoverXHTML(lb labels, novelName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xml:lang="%s">
<head>
  <title>%s</title>
  <link rel="stylesheet" type="text/css" href="style.css"/>
</head>
<body>
`, lb.lang, html.EscapeString(lb.cover))
	if name := strings.TrimSpace(novelName); name != "" {
		fmt.Fprintf(&b, "  <h1 class=\"book-title\">%s</h1>\n", html.EscapeString(name))
	}
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

// nav.xhtml (EPUB 3 navigation) ────────────────────────────────────────────────

func renderNavXHTML(lb labels, hasCover bool, chapters []int, titleIdx chapterTitleIndex) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" xml:lang="%s">
<head>
  <title>%s</title>
  <link rel="stylesheet" type="text/css" href="style.css"/>
</head>
<body>
  <nav epub:type="toc">
    <h1>%s</h1>
    <ol>
`, lb.lang, html.EscapeString(lb.toc), html.EscapeString(lb.toc))
	if hasCover {
		fmt.Fprintf(&b, "      <li><a href=\"cover.xhtml\">%s</a></li>\n", html.EscapeString(lb.cover))
	}

	// A flat chapter list. Volume/arc grouping is actually less clean than a single-level table of contents in a reader (readers fold by themselves),
	// and nested ol in an EPUB 3 nav renders oddly in some readers. Keep it simple.
	for _, ch := range chapters {
		title := strings.TrimSpace(titleIdx[ch])
		display := fmt.Sprintf(lb.chapterFmt, ch)
		if title != "" {
			display = fmt.Sprintf(lb.chapterTitleFmt, ch, title)
		}
		fmt.Fprintf(&b, "      <li><a href=\"%s\">%s</a></li>\n",
			chapterFileName(ch), html.EscapeString(display))
	}

	b.WriteString(`    </ol>
  </nav>
</body>
</html>
`)
	return b.String()
}

// content.opf ────────────────────────────────────────────────

func renderOPF(lb labels, book domain.BookMetadata, hasCover bool, chapters []int) string {
	bookID := bookIdentifier(book.Title)
	modified := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	title := strings.TrimSpace(book.Title)
	if title == "" {
		title = "Untitled"
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid" xml:lang="%s">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="bookid">%s</dc:identifier>
    <dc:title>%s</dc:title>
    <dc:language>%s</dc:language>
    <dc:creator>ainovel-cli</dc:creator>
    <dc:description>%s</dc:description>
    <meta property="dcterms:modified">%s</meta>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="css" href="style.css" media-type="text/css"/>
`, lb.lang, html.EscapeString(bookID), html.EscapeString(title), lb.lang, html.EscapeString(book.Synopsis), modified)

	if hasCover {
		b.WriteString(`    <item id="cover" href="cover.xhtml" media-type="application/xhtml+xml"/>` + "\n")
	}
	for _, ch := range chapters {
		fmt.Fprintf(&b, `    <item id="%s" href="%s" media-type="application/xhtml+xml"/>`+"\n",
			chapterID(ch), chapterFileName(ch))
	}

	b.WriteString("  </manifest>\n  <spine>\n")
	if hasCover {
		b.WriteString(`    <itemref idref="cover"/>` + "\n")
	}
	b.WriteString(`    <itemref idref="nav"/>` + "\n")
	for _, ch := range chapters {
		fmt.Fprintf(&b, `    <itemref idref="%s"/>`+"\n", chapterID(ch))
	}
	b.WriteString("  </spine>\n</package>\n")
	return b.String()
}

// bookIdentifier derives a stable UUID string from the novel name.
//
// **Use only novelName, without mixing in the chapter list**: a work's identity should be tied to "which book it is", not to the "export range"
// or "how many chapters were written at export time". Re-exporting the same book keeps the ID unchanged, and readers use it to recognize an updated version
// of the same work (whether it was updated is carried by the dcterms:modified timestamp). Empty novelName sharing an ID is a
// known edge case: if the user names neither of two books, that is on them.
func bookIdentifier(novelName string) string {
	h := sha1.New()
	h.Write([]byte(novelName))
	sum := h.Sum(nil)
	// Format as a UUID style (8-4-4-4-12); strict RFC 4122 is not required, since EPUB only requires the string to be unique and stable.
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
