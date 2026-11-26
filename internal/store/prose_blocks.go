package store

import (
	"regexp"
	"strings"
)

// The prose scanner decides which lines of a message are code, so that a mention or a room reference in code never wakes an agent or
// becomes a link. The web renderer (web/js/markdown.js, renderBlocks and readList) decides what is code on screen; the two must agree,
// so this file is a port of that block structure, reduced to what decides liveness: a fenced block, an indented block after a blank
// line, the tables whose rows are prose whatever they look like, and the lists that nest blocks of their own. A list item's lines under it (everything indented deeper that is not another item)
// are dedented by their shared minimum width and read as blocks again, exactly as the renderer does it. Everything else is a line of
// prose, in which only inline code spans and \@ hide a mention. One case table, testdata/fence_cases.json, is read by this package's
// test and by scripts/test-markdown.cjs.

// maxProseBlockDepth is the renderer's MAX_BLOCK_DEPTH: blocks nested deeper than this are shown as plain text, with no block read.
const maxProseBlockDepth = 8

// proseLine is one line being scanned: its text (dedented when it sits in a list item) and, for each byte of that text, the offset in
// the message body it came from.
type proseLine struct {
	text string
	pos  []int
}

type proseOutput struct {
	live strings.Builder
	pos  []int
}

func (o *proseOutput) newline() {
	o.live.WriteByte('\n')
	o.pos = append(o.pos, -1)
}

// code emits a line that is not live prose.
func (o *proseOutput) code() { o.newline() }

// line emits a line of prose: inline code spans and \@ are blanked, everything else is kept.
func (o *proseOutput) line(l proseLine) {
	text := l.text
	keep := func(i int) {
		o.live.WriteByte(text[i])
		o.pos = append(o.pos, l.pos[i])
	}
	blank := func(i int) {
		o.live.WriteByte(' ')
		o.pos = append(o.pos, l.pos[i])
	}
	codeRun := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '`':
			end := i + 1
			for end < len(text) && text[end] == '`' {
				end++
			}
			run := end - i
			if codeRun == run {
				codeRun = 0
			} else if codeRun == 0 && hasClosingBacktickRun(text, end, run) {
				codeRun = run
			}
			for j := i; j < end; j++ {
				blank(j)
			}
			i = end - 1
		case '\\':
			if i+1 < len(text) && text[i+1] == '@' {
				blank(i)
				blank(i + 1)
				i++
			} else {
				keep(i)
			}
		default:
			if codeRun != 0 {
				blank(i)
			} else {
				keep(i)
			}
		}
	}
	o.newline()
}

func proseBlank(l proseLine) bool { return strings.TrimSpace(l.text) == "" }

func proseIndentedCode(l proseLine) bool {
	return (strings.HasPrefix(l.text, "    ") || strings.HasPrefix(l.text, "\t")) && !proseBlank(l)
}

// proseFenceOpen reads a fence opener (up to three spaces of indent, three or more backticks or tildes, no backtick in a backtick
// fence's info) and returns its marker, or "".
func proseFenceOpen(line string) string {
	spaces := 0
	for spaces < len(line) && spaces < 4 && line[spaces] == ' ' {
		spaces++
	}
	if spaces > 3 {
		return ""
	}
	line = line[spaces:]
	if len(line) < 3 || line[0] != '`' && line[0] != '~' {
		return ""
	}
	end := 0
	for end < len(line) && line[end] == line[0] {
		end++
	}
	if end < 3 || line[0] == '`' && strings.ContainsRune(line[end:], '`') {
		return ""
	}
	return line[:end]
}

// proseFenceClose says whether line closes a fence of marker: up to three spaces, a run of the same character at least as long, and
// nothing else but blanks.
func proseFenceClose(line, marker string) bool {
	if marker == "" {
		return false
	}
	spaces := 0
	for spaces < len(line) && spaces < 4 && line[spaces] == ' ' {
		spaces++
	}
	if spaces > 3 || len(line)-spaces < len(marker) || line[spaces] != marker[0] {
		return false
	}
	end := spaces
	for end < len(line) && line[end] == marker[0] {
		end++
	}
	return end-spaces >= len(marker) && strings.Trim(line[end:], " \t\r") == ""
}

var proseListItemPattern = regexp.MustCompile(`^(\s*)(?:[-*+]|\d+\.)\s+(.*)$`)

// proseThematicBreak is the renderer's RULE: three or more of -, * or _ (blanks between allowed), alone on the line.
func proseThematicBreak(line string) bool {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	rest := strings.TrimRight(line[spaces:], " \t\r\f")
	if spaces > 3 || rest == "" || rest[0] != '-' && rest[0] != '*' && rest[0] != '_' {
		return false
	}
	count := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case rest[0]:
			count++
		case ' ', '\t', '\f':
		default:
			return false
		}
	}
	return count >= 3
}

// proseListItem reads a list item line: the indent of its marker and its content column (where its text begins).
func proseListItem(line string) (indent, column int, ok bool) {
	m := proseListItemPattern.FindStringSubmatch(line)
	if m == nil || proseThematicBreak(line) {
		return 0, 0, false
	}
	return proseWidth(m[1]), proseWidth(line[:len(line)-len(m[2])]), true
}

// proseWidth is the width of text with a tab counted as four.
func proseWidth(text string) int {
	width := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\t' {
			width += 4
		} else {
			width++
		}
	}
	return width
}

// proseLeadingWidth is the width of the leading blanks of line.
func proseLeadingWidth(line string) int {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return proseWidth(line[:i])
}

// proseStripColumn removes up to column columns of leading blanks from line (a tab counts four), as readList does before it looks for
// a fence under an item.
func proseStripColumn(line string, column int) string {
	i, width := 0, 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		if line[i] == '\t' {
			width += 4
		} else {
			width++
		}
		i++
	}
	return strings.Repeat(" ", max(width-min(width, column), 0)) + line[i:]
}

// proseDedent expands every tab to four spaces and drops the first width columns, as the renderer dedents an item's lines.
func proseDedent(l proseLine, width int) proseLine {
	if proseBlank(l) {
		return proseLine{}
	}
	var text []byte
	var pos []int
	for i := 0; i < len(l.text); i++ {
		if l.text[i] == '\t' {
			text = append(text, "    "...)
			pos = append(pos, l.pos[i], l.pos[i], l.pos[i], l.pos[i])
		} else {
			text = append(text, l.text[i])
			pos = append(pos, l.pos[i])
		}
	}
	if width > len(text) {
		width = len(text)
	}
	return proseLine{text: string(text[width:]), pos: pos[width:]}
}

// scanProseBlocks is renderBlocks: it reads lines block by block and emits one output line for each input line.
func scanProseBlocks(out *proseOutput, lines []proseLine, depth int) {
	i := 0
	for i < len(lines) {
		line := lines[i]
		if proseBlank(line) {
			out.newline()
			i++
			continue
		}
		if depth > maxProseBlockDepth {
			out.line(line)
			i++
			continue
		}
		if marker := proseFenceOpen(line.text); marker != "" {
			closer := -1
			for next := i + 1; next < len(lines); next++ {
				if proseFenceClose(lines[next].text, marker) {
					closer = next
					break
				}
			}
			if closer >= 0 {
				for ; i <= closer; i++ {
					out.code()
				}
				continue
			}
			// An unclosed fence is text as typed, and the lines after it are read as Markdown.
			out.line(line)
			i++
			continue
		}
		if proseIndentedCode(line) && (i == 0 || proseBlank(lines[i-1])) {
			for i < len(lines) && proseIndentedCode(lines[i]) {
				out.code()
				i++
			}
			continue
		}
		// The renderer reads a heading and a quote before a table, and a table before a rule or a list: a line that opens a table is
		// never an item, and the lines after it that hold a pipe are its rows, not fences or items.
		if !proseHeadingOrQuote(line.text) {
			next := ""
			if i+1 < len(lines) {
				next = lines[i+1].text
			}
			if proseTableStart(line.text, next) {
				out.line(line)
				out.line(lines[i+1])
				i += 2
				for i < len(lines) && !proseBlank(lines[i]) && strings.Contains(lines[i].text, "|") {
					out.line(lines[i])
					i++
				}
				continue
			}
		}
		if _, _, ok := proseListItem(line.text); ok {
			i = scanProseList(out, lines, i, depth)
			continue
		}
		out.line(line)
		i++
	}
}

var (
	proseHeadingPattern = regexp.MustCompile(`^#{1,6}\s+.+$`)
	proseRulePattern    = regexp.MustCompile(`^:?-+:?$`)
)

// proseHeadingOrQuote says whether the renderer reads line as a heading or a quote, both before it looks for a table.
func proseHeadingOrQuote(line string) bool {
	return strings.HasPrefix(line, ">") || proseHeadingPattern.MatchString(line)
}

// proseTableCells is cells in web/js/markdown-table.js: a row split at its unescaped pipes outside a code span.
func proseTableCells(line string) []string {
	row := strings.TrimSpace(line)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	var out []string
	var cell strings.Builder
	ticks := false
	for i := 0; i < len(row); i++ {
		switch c := row[i]; {
		case c == '\\' && i+1 < len(row) && row[i+1] == '|':
			cell.WriteByte('|')
			i++
		case c == '`':
			ticks = !ticks
			cell.WriteByte(c)
		case c == '|' && !ticks:
			out = append(out, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(c)
		}
	}
	return append(out, strings.TrimSpace(cell.String()))
}

// proseTableStart is tableSpec: a line with a pipe followed by a rule line with as many cells, each of dashes with optional colons.
func proseTableStart(line, next string) bool {
	if !strings.Contains(line, "|") {
		return false
	}
	headings, rules := proseTableCells(line), proseTableCells(next)
	if len(headings) != len(rules) {
		return false
	}
	for _, rule := range rules {
		if !proseRulePattern.MatchString(rule) {
			return false
		}
	}
	return true
}

// scanProseList is readList: the list that starts at lines[start], item by item. An item's own line is prose; the lines under it (deeper
// than its marker and not another item, a fence's lines included whatever they look like) are dedented by their shared minimum width
// and read as blocks of their own. It returns the index of the first line after the list.
func scanProseList(out *proseOutput, lines []proseLine, start, depth int) int {
	i := start
	for i < len(lines) {
		if indent, column, ok := proseListItem(lines[i].text); ok {
			head := lines[i]
			i++
			var extra []proseLine
			fence := ""
			for i < len(lines) {
				line := lines[i]
				if proseBlank(line) {
					next := i + 1
					for next < len(lines) && proseBlank(lines[next]) {
						next++
					}
					more := false
					if next < len(lines) {
						_, _, itemLine := proseListItem(lines[next].text)
						more = !itemLine && proseLeadingWidth(lines[next].text) > indent
					}
					if fence != "" || more {
						extra = append(extra, line)
						i++
						continue
					}
					break
				}
				if _, _, itemLine := proseListItem(line.text); fence == "" && (itemLine || proseLeadingWidth(line.text) <= indent) {
					break
				}
				stripped := proseStripColumn(line.text, column)
				if fence != "" {
					if proseFenceClose(stripped, fence) {
						fence = ""
					}
				} else if marker := proseFenceOpen(stripped); marker != "" {
					for next := i + 1; next < len(lines); next++ {
						if !proseBlank(lines[next]) && proseLeadingWidth(lines[next].text) <= indent {
							break
						}
						if proseFenceClose(proseStripColumn(lines[next].text, column), marker) {
							fence = marker
							break
						}
					}
				}
				extra = append(extra, line)
				i++
			}
			// An unclosed fence is text in the item; trailing blank lines are not part of it.
			trailing := 0
			for len(extra) > 0 && proseBlank(extra[len(extra)-1]) {
				extra = extra[:len(extra)-1]
				trailing++
			}
			out.line(head)
			if len(extra) > 0 {
				width := -1
				for _, line := range extra {
					if !proseBlank(line) {
						if w := proseLeadingWidth(line.text); width < 0 || w < width {
							width = w
						}
					}
				}
				dedented := make([]proseLine, len(extra))
				for n, line := range extra {
					dedented[n] = proseDedent(line, width)
				}
				scanProseBlocks(out, dedented, depth+1)
			}
			for ; trailing > 0; trailing-- {
				out.newline()
			}
			continue
		}
		if proseBlank(lines[i]) && i+1 < len(lines) {
			if _, _, ok := proseListItem(lines[i+1].text); ok {
				out.newline()
				i++
				continue
			}
		}
		break
	}
	return i
}
