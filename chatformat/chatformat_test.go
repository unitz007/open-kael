package chatformat

import (
	"fmt"
	"strings"
	"testing"
)

// plainR draws parts as plain text, to see what Render decides.
type plainR struct{}

func (plainR) Inlines(in []Inline) string {
	var b strings.Builder
	for _, x := range in {
		if x.Bold {
			b.WriteString("*" + x.Text + "*")
		} else {
			b.WriteString(x.Text)
		}
	}
	return b.String()
}
func (plainR) Paragraph(t string) string { return t }
func (plainR) Heading(t string) string   { return "# " + t }
func (plainR) List(items []Item, drawn []string) string {
	var out []string
	for i, it := range items {
		mark := "-"
		if it.Ordered {
			mark = fmt.Sprintf("%d.", it.Number)
		}
		out = append(out, strings.Repeat("  ", it.Level)+mark+" "+drawn[i])
	}
	return strings.Join(out, "\n")
}
func (plainR) Quote(t string) string      { return "> " + t }
func (plainR) Code(_, t string) string    { return "[code]" + t }
func (plainR) Rule() string               { return "---" }
func (plainR) Table(g *Grid) string       { return fmt.Sprintf("[table %d×%d]", len(g.Header), len(g.Rows)) }
func (plainR) Fixed(t string) string      { return "[fixed]\n" + t }
func (plainR) Join(parts []string) string { return strings.Join(parts, "\n") }

const players = "Here are your top players:\n\n### This season\n| Player | Team | Points | Price |\n|---|---|---|---|\n| Saka | Arsenal | 142 | £10.1m |\n| Palmer | Chelsea | 138 | £10.8m |\n| Haaland | Man City | 131 | £14.2m |\n\n- Saka is the **best** value\n- Haaland costs the most"

func TestParse_PartsAndATable(t *testing.T) {
	bs := Parse(players)
	kinds := []string{}
	for _, b := range bs {
		kinds = append(kinds, string(b.Kind))
	}
	if strings.Join(kinds, ",") != "paragraph,heading,table,list" {
		t.Fatalf("parts: %v", kinds)
	}
	g := bs[2].Table
	if strings.Join(g.Header, "|") != "Player|Team|Points|Price" || len(g.Rows) != 3 || g.Rows[2][1] != "Man City" {
		t.Errorf("table: %+v", g)
	}
	if fmt.Sprint(g.Right) != "[false false true true]" {
		t.Errorf("numbers and prices on the right: %v", g.Right)
	}
	if in := bs[3].Items[0].Inlines; len(in) != 3 || !in[1].Bold || in[1].Text != "best" {
		t.Errorf("marks kept: %+v", in)
	}
}

func TestRender_ATableDrawnLinedUpOrARowEach(t *testing.T) {
	if got := Render(players, plainR{}, Fits{Tables: true}); !strings.Contains(got, "[table 4×3]") {
		t.Errorf("drawn by the chat app: %q", got)
	}
	if got := Render(players, plainR{}, Fits{Width: 36}); !strings.Contains(got, "[fixed]\nPlayer") {
		t.Fatalf("lined up when it fits: %q", got)
	}
	lined := Aligned(Parse(players)[2].Table)
	if lined != "Player   Team      Points   Price\nSaka     Arsenal      142  £10.1m\nPalmer   Chelsea      138  £10.8m\nHaaland  Man City     131  £14.2m" {
		t.Errorf("lined up:\n%s", lined)
	}
	narrow := Render(players, plainR{}, Fits{Width: 20})
	if !strings.Contains(narrow, "*Saka* · Team: Arsenal · Points: 142 · Price: £10.1m\n*Palmer*") {
		t.Errorf("a row each when it doesn't fit: %q", narrow)
	}
}

func TestRender_ListsKeepTheirNumbersAndNesting(t *testing.T) {
	cases := map[string]string{
		"2. second\n3. third":               "2. second\n3. third",
		"- a\n  1. inner\n  2. inner2\n- b": "- a\n  1. inner\n  2. inner2\n- b",
		"Steps:\n\n1. one\n2. two":          "Steps:\n1. one\n2. two",
	}
	for in, want := range cases {
		if got := Render(in, plainR{}, Fits{}); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestParse_LinksCodeQuotesAndLineBreaks(t *testing.T) {
	bs := Parse("See [the docs](https://x.io) or https://y.io and `go run`\nnext line\n\n> quoted\n\n```go\nfmt.Println(1)\n```")
	p := bs[0].Inlines
	var links, code int
	for _, in := range p {
		if in.Link != "" {
			links++
		}
		if in.Code {
			code++
		}
	}
	if links != 2 || code != 1 || !strings.Contains(PlainText(p), "go run\nnext line") {
		t.Errorf("%+v", p)
	}
	if bs[1].Kind != Quote || PlainText(bs[1].Inlines) != "quoted" || bs[2].Kind != Code || bs[2].Lang != "go" || bs[2].Text != "fmt.Println(1)" {
		t.Errorf("%+v", bs[1:])
	}
}

func TestNumbersAndMeasuresGoRight(t *testing.T) {
	g := Parse("| Day | High | Wind | Rain | Note |\n|---|---|---|---|---|\n| Fri | 14.9°C | 12 km/h | 0% | Overcast |\n| Sat | 9°F | 8 mph | 15% | Fog 2 |")[0].Table
	if fmt.Sprint(g.Right) != "[false true true true false]" {
		t.Errorf("%v", g.Right)
	}
}

func TestRender_AWideTableLinksToItInFull(t *testing.T) {
	wide := "| Player | Team | Pos | Pts | Price | Form | Next | Owned |\n|---|---|---|---|---|---|---|---|\n| Saka | Arsenal | MID | 142 | £10.1m | 7.2 | BUR | 48% |"
	linked := 0
	f := Fits{Width: 36, TableLink: func(g *Grid) string { linked++; return "https://x.test/t/1" }}
	if got := Render(wide, linkR{}, f); !strings.HasSuffix(got, "\n[See the full table ›](https://x.test/t/1)") || linked != 1 {
		t.Errorf("%q", got)
	}
	narrow := "| Player | Team | Points | Price | Form |\n|---|---|---|---|---|\n| Saka | Arsenal | 142 | £10.1m | 7.2 |"
	if got := Render(narrow, linkR{}, Fits{Width: 20, TableLink: f.TableLink}); strings.Contains(got, "full table") {
		t.Errorf("six columns or fewer: a line per row is enough: %q", got)
	}
}

// linkR draws links as markdown, to see them.
type linkR struct{ plainR }

func (linkR) Inlines(in []Inline) string {
	var b strings.Builder
	for _, x := range in {
		if x.Link != "" {
			b.WriteString("[" + x.Text + "](" + x.Link + ")")
		} else {
			b.WriteString(x.Text)
		}
	}
	return b.String()
}
