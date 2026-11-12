// Chapter 9.9 — what a reviewer asks of a design document before reading the design.
//
//	part 1  two documents for the same feature, checked for the things that a review needs
//	part 2  the arithmetic behind the second document's cost, with one assumption measured
//
//	go run ./phase-9-shipping/code/ch09-designcheck                 the two documents in docs/
//	go run ./phase-9-shipping/code/ch09-designcheck my-design.md    a document of your own
//
// The checks are crude on purpose: headings, digits and a list of words. They cannot tell a
// good design from a bad one. They find the documents that cannot be reviewed at all.
package main

import (
	"embed"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
)

//go:embed docs/*.md
var docs embed.FS

// A document, cut into its sections: the text under each "## " heading.
type document struct {
	title    string
	head     string            // what comes before the first section: status, author, approver
	sections map[string]string // a heading, in lower case → the text under it
	text     string
}

func read(text string) document {
	d := document{sections: map[string]string{}, text: text}
	name := ""
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "# "):
			d.title = strings.TrimPrefix(line, "# ")
		case strings.HasPrefix(line, "## "):
			name = strings.ToLower(strings.TrimPrefix(line, "## "))
		case name == "":
			d.head += line + "\n"
		default:
			d.sections[name] += line + "\n"
		}
	}
	return d
}

// section returns the text under the first heading that contains one of the words.
func (d document) section(words ...string) (string, bool) {
	names := make([]string, 0, len(d.sections))
	for name := range d.sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, w := range words {
			if strings.Contains(name, w) {
				return d.sections[name], true
			}
		}
	}
	return "", false
}

var (
	item  = regexp.MustCompile(`(?m)^(- |\d+\. |\| )`) // a list item, or a row of a table
	digit = regexp.MustCompile(`\d`)
	// Words that claim a property and give no number for it.
	vague = regexp.MustCompile(`(?i)\b(scalable|flexible|robust|seamless|performant|fast|simple|intuitive|highly available|` +
		`high throughput|fault tolerance|best practice|best-in-class|enterprise-grade|future-proof|industry standard|as needed|going forward)\b`)
)

// items returns the list items and table rows of a section, without a table's two heading rows.
func items(text string) (out []string) {
	for _, line := range strings.Split(text, "\n") {
		if item.MatchString(line) && !strings.HasPrefix(line, "|---") && !strings.HasPrefix(line, "| Option") && !strings.HasPrefix(line, "| When") {
			out = append(out, line)
		}
	}
	return out
}

// A check is one thing that a reviewer needs from the document before the design can be judged.
var checks = []struct {
	what string
	run  func(d document) (bool, string)
}{
	{"says what problem it solves", func(d document) (bool, string) {
		text, ok := d.section("problem", "context", "motivation")
		return ok && digit.MatchString(text), found(ok, digit.MatchString(text), "a problem with no number in it")
	}},
	{"has goals, and non-goals", func(d document) (bool, string) {
		_, goals := d.sections["goals"]
		_, non := d.section("non-goals")
		return goals && non, found(goals && non, true, "")
	}},
	{"gives its requirements as numbers", func(d document) (bool, string) {
		text, ok := d.section("requirements")
		if !ok {
			return false, found(false, true, "")
		}
		all, with := items(text), 0
		for _, it := range all {
			if digit.MatchString(it) {
				with++
			}
		}
		return with == len(all) && with > 0, fmt.Sprintf("%d of %d requirements have a number", with, len(all))
	}},
	{"shows the design in a picture", func(d document) (bool, string) {
		ok := strings.Contains(d.text, "```mermaid") || strings.Contains(d.text, "![")
		return ok, found(ok, true, "")
	}},
	{"weighs alternatives, one of them to do nothing", func(d document) (bool, string) {
		text, ok := d.section("alternatives")
		if !ok {
			return false, found(false, true, "")
		}
		n, nothing := len(items(text)), strings.Contains(strings.ToLower(text), "do nothing")
		return n >= 3 && nothing, fmt.Sprintf("%d options", n)
	}},
	{"says what the design is worse at", func(d document) (bool, string) {
		text, ok := d.section("worse", "trade-off", "drawback")
		return ok && len(items(text)) > 0, found(ok, true, "")
	}},
	{"says what happens when things fail", func(d document) (bool, string) {
		text, ok := d.section("failure", "fail")
		return ok && len(items(text)) >= 3, found(ok, false, fmt.Sprintf("%d failures answered", len(items(text))))
	}},
	{"says how it is rolled out, and undone", func(d document) (bool, string) {
		text, ok := d.section("rollout", "roll out", "migration")
		undo := regexp.MustCompile(`(?i)\b(undo|roll back|rollback|turn(ed)? off)\b`).MatchString(text)
		return ok && undo, found(ok, undo, "a rollout, and no way back")
	}},
	{"says what it costs", func(d document) (bool, string) {
		text, ok := d.section("cost")
		money := regexp.MustCompile(`[£$€]\s?\d`).MatchString(text)
		return ok && money, found(ok, money, "a cost section with no sum of money in it")
	}},
	{"lists what is not yet known", func(d document) (bool, string) {
		text, ok := d.section("open question", "unknown")
		return ok, found(ok, false, fmt.Sprintf("%d questions", len(items(text))))
	}},
	{"names who decides, and by when", func(d document) (bool, string) {
		head := strings.ToLower(d.head)
		ok := strings.Contains(head, "approver:") && strings.Contains(head, "decision needed by:")
		return ok, found(ok, true, "")
	}},
	{"can be read in one sitting", func(d document) (bool, string) {
		words := len(strings.Fields(d.text))
		minutes := int(math.Ceil(float64(words) / 230)) // an unhurried reader
		time := fmt.Sprintf("about %d minutes", minutes)
		if minutes == 1 {
			time = "about a minute"
		}
		return minutes <= 20, fmt.Sprintf("%s words: %s to read", commas(float64(words)), time)
	}},
	{"claims nothing without a number", func(d document) (bool, string) {
		count, order := map[string]int{}, []string{}
		for _, w := range vague.FindAllString(d.text, -1) {
			w = strings.ToLower(w)
			if count[w] == 0 {
				order = append(order, w)
			}
			count[w]++
		}
		total := 0
		for i, w := range order {
			total += count[w]
			if count[w] > 1 {
				order[i] = fmt.Sprintf("%s ×%d", w, count[w])
			}
		}
		if total == 0 {
			return true, ""
		}
		if len(order) > 3 {
			order = append(order[:3], "…")
		}
		return false, fmt.Sprintf("%d: %s", total, strings.Join(order, ", "))
	}},
}

// found words the note beside a check: nothing to say, or the section is missing, or it is
// there and something is to be said about it.
func found(there, good bool, note string) string {
	switch {
	case !there:
		return "not in the document"
	case good:
		return ""
	}
	return note
}

func commas(f float64) string {
	s := fmt.Sprintf("%.0f", f)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func check(name, text string) {
	d, passed := read(text), 0
	fmt.Printf("  %s — %q\n", name, d.title)
	for _, c := range checks {
		ok, note := c.run(d)
		mark := "✗"
		if ok {
			mark, passed = "✓", passed+1
		}
		fmt.Println(strings.TrimRight(fmt.Sprintf("    %s %-48s %s", mark, c.what, note), " "))
	}
	fmt.Printf("    %d of %d\n\n", passed, len(checks))
}

func main() {
	if len(os.Args) > 1 { // documents of your own
		for _, path := range os.Args[1:] {
			text, err := os.ReadFile(path)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			check(path, string(text))
		}
		return
	}
	fmt.Println("PART 1 — two documents for one feature, and what a reviewer needs before the design can be judged.")
	fmt.Println()
	for _, name := range []string{"docs/exports-v1.md", "docs/exports-v2.md"} {
		text, _ := docs.ReadFile(name)
		check(name, string(text))
	}
	estimate()
}
