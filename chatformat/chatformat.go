// Package chatformat reads what an agent wrote (markdown) once, into parts: paragraphs, headings, lists, quotes,
// code, rules and tables, so every chat app draws the same reply from the same parts. A chat app gives a Renderer
// (how it draws each part) and Fits (what it can show: tables, how wide a line may be); what to do with a table it
// can't draw is decided here, once: a lined-up fixed-width block when it fits, else one short block per row.
package chatformat

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// Inline is a run of text and its marks.
type Inline struct {
	Text   string `json:"text"`
	Bold   bool   `json:"bold,omitempty"`
	Italic bool   `json:"italic,omitempty"`
	Code   bool   `json:"code,omitempty"`
	Strike bool   `json:"strike,omitempty"`
	Link   string `json:"link,omitempty"`
}

// Kind is what a part is.
type Kind string

const (
	Paragraph Kind = "paragraph"
	Heading   Kind = "heading"
	List      Kind = "list"
	Quote     Kind = "quote"
	Code      Kind = "code"
	Rule      Kind = "rule"
	Table     Kind = "table"
)

// Item is one entry of a list; Level is how deep it is nested (0 for the outer list).
type Item struct {
	Level   int      `json:"level,omitempty"`
	Ordered bool     `json:"ordered,omitempty"` // in a numbered list
	Number  int      `json:"number,omitempty"`  // in a numbered list: its number
	Inlines []Inline `json:"inlines"`
}

// Grid is a table: its header, its rows (plain text) and which columns hold numbers, prices or amounts.
type Grid struct {
	Header []string   `json:"header"`
	Rows   [][]string `json:"rows"`
	Right  []bool     `json:"right"` // a column of numbers: lined up on the right
}

// Block is one part of a reply.
type Block struct {
	Kind    Kind     `json:"kind"`
	Inlines []Inline `json:"inlines,omitempty"` // paragraph, heading, quote
	Level   int      `json:"level,omitempty"`   // heading
	Ordered bool     `json:"ordered,omitempty"` // list
	Items   []Item   `json:"items,omitempty"`   // list
	Lang    string   `json:"lang,omitempty"`    // code
	Text    string   `json:"text,omitempty"`    // code
	Table   *Grid    `json:"table,omitempty"`
}

var md = goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify))

// Parse reads markdown into its parts.
func Parse(markdown string) []Block {
	src := []byte(markdown)
	doc := md.Parser().Parse(text.NewReader(src))
	var out []Block
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		out = append(out, blocks(n, src)...)
	}
	return out
}

func blocks(n ast.Node, src []byte) []Block {
	switch v := n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		if in := inlines(n, src, Inline{}); len(in) > 0 {
			return []Block{{Kind: Paragraph, Inlines: in}}
		}
	case *ast.Heading:
		return []Block{{Kind: Heading, Level: v.Level, Inlines: inlines(n, src, Inline{})}}
	case *ast.List:
		b := Block{Kind: List, Ordered: v.IsOrdered()}
		listItems(v, src, 0, &b.Items)
		return []Block{b}
	case *ast.Blockquote:
		var in []Inline
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if len(in) > 0 {
				in = append(in, Inline{Text: "\n"})
			}
			in = append(in, inlines(c, src, Inline{})...)
		}
		return []Block{{Kind: Quote, Inlines: in}}
	case *ast.FencedCodeBlock:
		return []Block{{Kind: Code, Lang: string(v.Language(src)), Text: lines(n, src)}}
	case *ast.CodeBlock:
		return []Block{{Kind: Code, Text: lines(n, src)}}
	case *ast.ThematicBreak:
		return []Block{{Kind: Rule}}
	case *ast.HTMLBlock:
		if t := strings.TrimSpace(lines(n, src)); t != "" {
			return []Block{{Kind: Paragraph, Inlines: []Inline{{Text: t}}}}
		}
	case *extast.Table:
		return []Block{{Kind: Table, Table: grid(v, src)}}
	}
	return nil
}

func listItems(l *ast.List, src []byte, level int, out *[]Item) {
	number := l.Start
	for li := l.FirstChild(); li != nil; li = li.NextSibling() {
		item := Item{Level: level, Ordered: l.IsOrdered()}
		if l.IsOrdered() {
			item.Number = number
			number++
		}
		var nested []*ast.List
		for c := li.FirstChild(); c != nil; c = c.NextSibling() {
			if sub, ok := c.(*ast.List); ok {
				nested = append(nested, sub)
				continue
			}
			if len(item.Inlines) > 0 {
				item.Inlines = append(item.Inlines, Inline{Text: "\n"})
			}
			item.Inlines = append(item.Inlines, inlines(c, src, Inline{})...)
		}
		*out = append(*out, item)
		for _, sub := range nested {
			listItems(sub, src, level+1, out)
		}
	}
}

func lines(n ast.Node, src []byte) string {
	var b strings.Builder
	ls := n.Lines()
	for i := 0; i < ls.Len(); i++ {
		seg := ls.At(i)
		b.Write(seg.Value(src))
	}
	return strings.TrimRight(b.String(), "\n")
}

// inlines reads a node's inline content, carrying the marks of what it is inside.
func inlines(n ast.Node, src []byte, mark Inline) []Inline {
	var out []Inline
	add := func(t string, m Inline) {
		if t == "" {
			return
		}
		m.Text = t
		if k := len(out) - 1; k >= 0 && sameMarks(out[k], m) {
			out[k].Text += t
			return
		}
		out = append(out, m)
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			add(string(v.Segment.Value(src)), mark)
			if v.HardLineBreak() || v.SoftLineBreak() {
				add("\n", mark)
			}
		case *ast.String:
			add(string(v.Value), mark)
		case *ast.CodeSpan:
			m := mark
			m.Code = true
			add(plain(c, src), m)
		case *ast.Emphasis:
			m := mark
			if v.Level >= 2 {
				m.Bold = true
			} else {
				m.Italic = true
			}
			for _, in := range inlines(c, src, m) {
				add(in.Text, in)
			}
		case *extast.Strikethrough:
			m := mark
			m.Strike = true
			for _, in := range inlines(c, src, m) {
				add(in.Text, in)
			}
		case *ast.Link:
			m := mark
			m.Link = string(v.Destination)
			for _, in := range inlines(c, src, m) {
				add(in.Text, in)
			}
		case *ast.AutoLink:
			m := mark
			m.Link = string(v.URL(src))
			add(string(v.Label(src)), m)
		case *ast.Image:
			add(plain(c, src), mark)
		case *ast.RawHTML:
			var b strings.Builder
			for i := 0; i < v.Segments.Len(); i++ {
				seg := v.Segments.At(i)
				b.Write(seg.Value(src))
			}
			add(b.String(), mark)
		default:
			for _, in := range inlines(c, src, mark) {
				add(in.Text, in)
			}
		}
	}
	// A line break at the very end says nothing.
	if k := len(out) - 1; k >= 0 {
		out[k].Text = strings.TrimRight(out[k].Text, "\n")
		if out[k].Text == "" {
			out = out[:k]
		}
	}
	return out
}

func sameMarks(a, b Inline) bool {
	return a.Bold == b.Bold && a.Italic == b.Italic && a.Code == b.Code && a.Strike == b.Strike && a.Link == b.Link
}

// plain is a node's text with no marks.
func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	for _, in := range inlines(n, src, Inline{}) {
		b.WriteString(in.Text)
	}
	return b.String()
}

// PlainText is inlines as text, without their marks.
func PlainText(in []Inline) string {
	var b strings.Builder
	for _, x := range in {
		b.WriteString(x.Text)
	}
	return b.String()
}

// numeric is a number, an amount or a measure: "142", "£10.1m", "48%", "14.9°C", "12 km/h", "3 days".
var numeric = regexp.MustCompile(`^[-+−]?[£$€₦¥]?\s?[-+−]?\d[\d,.]*(\s?(%|°[CF]?|[a-zA-Z/°]{1,5}))?$`)

func grid(t *extast.Table, src []byte) *Grid {
	g := &Grid{}
	for row := t.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, strings.TrimSpace(plain(cell, src)))
		}
		if _, header := row.(*extast.TableHeader); header {
			g.Header = cells
		} else {
			g.Rows = append(g.Rows, cells)
		}
	}
	cols := len(g.Header)
	for _, r := range g.Rows {
		cols = max(cols, len(r))
	}
	g.Right = make([]bool, cols)
	for c := 0; c < cols; c++ {
		seen := false
		right := true
		for _, r := range g.Rows {
			if c >= len(r) || r[c] == "" || r[c] == "-" || r[c] == "—" {
				continue
			}
			seen = true
			if !numeric.MatchString(r[c]) {
				right = false
			}
		}
		g.Right[c] = seen && right
	}
	return g
}

// Renderer is how a chat app draws each part (in its own markup, escaping as it must).
type Renderer interface {
	Inlines(in []Inline) string
	Paragraph(text string) string
	Heading(text string) string
	// List draws a list's items; each already drawn with Inlines, with its level and, in a numbered list, its number.
	List(items []Item, drawn []string) string
	Quote(text string) string
	Code(lang, text string) string
	Rule() string
	// Table draws a table, for a chat app that can (Fits.Tables).
	Table(g *Grid) string
	// Fixed draws text in a fixed-width block, exactly as it is: a lined-up table.
	Fixed(text string) string
	Join(parts []string) string
}

// Fits is what a chat app can show.
type Fits struct {
	Tables bool // draws tables itself
	Width  int  // characters a fixed-width line may take before it scrolls (a phone about 36, a computer about 60)
}

// Render draws markdown for a chat app.
func Render(markdown string, r Renderer, f Fits) string {
	return RenderBlocks(Parse(markdown), r, f)
}

// RenderBlocks draws parts for a chat app.
func RenderBlocks(bs []Block, r Renderer, f Fits) string {
	var parts []string
	for _, b := range bs {
		switch b.Kind {
		case Paragraph:
			parts = append(parts, r.Paragraph(r.Inlines(b.Inlines)))
		case Heading:
			parts = append(parts, r.Heading(r.Inlines(b.Inlines)))
		case List:
			drawn := make([]string, len(b.Items))
			for i, it := range b.Items {
				drawn[i] = r.Inlines(it.Inlines)
			}
			parts = append(parts, r.List(b.Items, drawn))
		case Quote:
			parts = append(parts, r.Quote(r.Inlines(b.Inlines)))
		case Code:
			parts = append(parts, r.Code(b.Lang, b.Text))
		case Rule:
			parts = append(parts, r.Rule())
		case Table:
			parts = append(parts, table(b.Table, r, f)...)
		}
	}
	return r.Join(parts)
}

// table is a table for a chat app: drawn as one where it can be, else lined up when it fits, else a block per row.
func table(g *Grid, r Renderer, f Fits) []string {
	if g == nil {
		return nil
	}
	if f.Tables {
		return []string{r.Table(g)}
	}
	if lined := Aligned(g); f.Width <= 0 || widest(lined) <= f.Width {
		return []string{r.Fixed(lined)}
	}
	var out []string
	for _, row := range g.Rows {
		out = append(out, r.Paragraph(r.Inlines(RowLine(g, row))))
	}
	return out
}

// Aligned is a table as lined-up text: columns two spaces apart, numbers on the right.
func Aligned(g *Grid) string {
	all := append([][]string{g.Header}, g.Rows...)
	width := make([]int, len(g.Right))
	for _, r := range all {
		for c, cell := range r {
			if c < len(width) {
				width[c] = max(width[c], utf8.RuneCountInString(cell))
			}
		}
	}
	var b strings.Builder
	for i, r := range all {
		if i > 0 {
			b.WriteString("\n")
		}
		var line strings.Builder
		for c := range width {
			cell := ""
			if c < len(r) {
				cell = r[c]
			}
			pad := strings.Repeat(" ", width[c]-utf8.RuneCountInString(cell))
			if c > 0 {
				line.WriteString("  ")
			}
			if g.Right[c] {
				line.WriteString(pad + cell)
			} else {
				line.WriteString(cell + pad)
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
	}
	return b.String()
}

// RowLine is one row as a short line: its first cell in bold, then each other "header: value", "·" between.
func RowLine(g *Grid, row []string) []Inline {
	if len(row) == 0 {
		return nil
	}
	out := []Inline{{Text: row[0], Bold: true}}
	for c := 1; c < len(row); c++ {
		if strings.TrimSpace(row[c]) == "" {
			continue
		}
		label := ""
		if c < len(g.Header) && strings.TrimSpace(g.Header[c]) != "" {
			label = g.Header[c] + ": "
		}
		out = append(out, Inline{Text: " · " + label + row[c]})
	}
	return out
}

func widest(s string) int {
	w := 0
	for _, l := range strings.Split(s, "\n") {
		w = max(w, utf8.RuneCountInString(l))
	}
	return w
}
