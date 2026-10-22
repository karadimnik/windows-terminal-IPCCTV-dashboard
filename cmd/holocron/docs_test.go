package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Drift guard for the documents that show commands: a renamed or removed command must fail here
// before a reader copies it. A fenced line (or an inline `code` span) that starts with holocron is
// checked against the registry (matchCommand); a line carrying "# ignore" is skipped.

var docFiles = []string{
	"docs/holocron-quickstart.md",
	"docs/holocron-cheatsheet.md",
	"docs/ai/holocron-for-agents.md",
	"README.md",
	"AGENTS.md",
}

const (
	tableBegin = "<!-- commands:begin"
	tableEnd   = "<!-- commands:end -->"
)

var (
	docSpan   = regexp.MustCompile("`([^`]+)`")
	docInvoke = regexp.MustCompile(`^(?:\$\s+)?(?:\.[\\/])?holocron(?:\.exe)?\s+(\S.*)$`)
	docSplit  = regexp.MustCompile(`\s*(?:&&|\|\||[;|])\s*`)
)

func readDoc(t testing.TB, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// docCommandProblems returns one message per holocron invocation in text whose command path is
// not in the registry.
func docCommandProblems(name, text string) []string {
	var problems []string
	check := func(lineNo int, fragment string) {
		for _, seg := range docSplit.Split(strings.TrimSpace(fragment), -1) {
			m := docInvoke.FindStringSubmatch(strings.TrimSpace(seg))
			if m == nil {
				continue
			}
			rest := m[1]
			if i := strings.Index(rest, " #"); i >= 0 {
				rest = rest[:i]
			}
			words := strings.Fields(rest)
		globals:
			for len(words) > 0 {
				switch words[0] {
				case "--config", "--root":
					if len(words) < 2 {
						words = nil
						break globals
					}
					words = words[2:]
				case "-v", "-vv", "--json":
					words = words[1:]
				default:
					break globals
				}
			}
			if len(words) == 0 || strings.HasPrefix(words[0], "<") || words[0] == "--help" || words[0] == "-h" {
				continue
			}
			if c, _ := matchCommand(words); c == nil {
				problems = append(problems, fmt.Sprintf("%s:%d: no registered command for %q", name, lineNo, strings.Join(words, " ")))
			}
		}
	}
	inFence := false
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if strings.Contains(line, "# ignore") {
			continue
		}
		if inFence {
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
				check(i+1, t)
			}
			continue
		}
		for _, s := range docSpan.FindAllStringSubmatch(line, -1) {
			check(i+1, s[1])
		}
	}
	return problems
}

func TestDocCommandsAreRegistered(t *testing.T) {
	for _, rel := range docFiles {
		for _, p := range docCommandProblems(rel, readDoc(t, rel)) {
			t.Error(p)
		}
	}
	// negative control: the checker flags a command that does not exist and accepts real ones
	doc := "```powershell\nholocron status\nholocron cam frobnicate  # not a command\nholocron cam nope # ignore\n```\nuse `holocron up --detach` or `holocron nope`.\n"
	got := docCommandProblems("control.md", doc)
	if len(got) != 2 || !strings.Contains(got[0], `"cam frobnicate"`) || !strings.Contains(got[1], `"nope"`) {
		t.Errorf("the checker misses a command that is not registered: %v", got)
	}
}

// registryTable is the command table of the agent document: one row per visible command, built
// from the registry that also prints `holocron --help`.
func registryTable() string {
	esc := func(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
	var b strings.Builder
	b.WriteString("| Command | Does |\n| --- | --- |\n")
	for _, c := range commands {
		if c.role {
			continue
		}
		u := c.path
		if c.usage != "" {
			u += " " + c.usage
		}
		fmt.Fprintf(&b, "| `%s` | %s |\n", esc(u), esc(c.about))
	}
	return b.String()
}

// The table between the markers in the agent document equals the registry. Regenerate it with
//
//	HOLOCRON_DOCS_UPDATE=1 go test ./cmd/holocron -run TestAgentDocCommandTable
func TestAgentDocCommandTable(t *testing.T) {
	const rel = "docs/ai/holocron-for-agents.md"
	text := readDoc(t, rel)
	bi := strings.Index(text, tableBegin)
	ei := strings.Index(text, tableEnd)
	if bi < 0 || ei < bi {
		t.Fatalf("%s: the markers %q ... %q are missing", rel, tableBegin, tableEnd)
	}
	start := bi + strings.Index(text[bi:], "\n") + 1
	got, want := text[start:ei], registryTable()
	if got == want {
		return
	}
	if os.Getenv("HOLOCRON_DOCS_UPDATE") == "1" {
		out := text[:start] + want + text[ei:]
		if err := os.WriteFile(filepath.Join("..", "..", filepath.FromSlash(rel)), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	cell := regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")
	rows := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, r := range cell.FindAllStringSubmatch(s, -1) {
			m[r[1]] = true
		}
		return m
	}
	g, w := rows(got), rows(want)
	for r := range w {
		if !g[r] {
			t.Errorf("%s: the registry has %q, the table does not (or words it differently)", rel, r)
		}
	}
	for r := range g {
		if !w[r] {
			t.Errorf("%s: the table has %q, the registry does not", rel, r)
		}
	}
	t.Errorf("%s: the command table differs from the registry; regenerate: HOLOCRON_DOCS_UPDATE=1 go test ./cmd/holocron -run TestAgentDocCommandTable", rel)
}
