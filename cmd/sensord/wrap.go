package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Help text is written as plain paragraphs and reflowed to the terminal's
// width when printed, since phone terminals are often narrower than 80
// columns and terminals don't re-wrap at word boundaries.

// termWidth is the width of the terminal on stdout, or 80 when it isn't one.
func termWidth() int {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		return 80
	}
	return min(int(ws.Col), 100)
}

// reflow rewraps text to width. Unindented lines are joined into paragraphs;
// an indented line starts an item (a list entry or example) that keeps its
// indentation, and more deeply indented lines continue it, wrapping with a
// hanging indent. Blank lines are kept.
func reflow(text string, width int) string {
	var out []string
	var para []string
	var item []string
	itemIndent, hang := 0, 0
	key := "" // "key   explanation" items: the key column
	flush := func() {
		if len(para) > 0 {
			out = append(out, wrapWords(strings.Join(para, " "), width, 0, 0)...)
			para = nil
		}
		if len(item) > 0 {
			text := strings.Join(item, " ")
			if key == "" {
				out = append(out, wrapWords(text, width, itemIndent, hang)...)
			} else if hang <= width/2 {
				// key in its column, explanation wrapped under itself
				lines := wrapWords(text, width, hang, hang)
				lines[0] = strings.Repeat(" ", itemIndent) + key + lines[0][itemIndent+len(key):]
				out = append(out, lines...)
			} else {
				// too narrow for two columns: explanation under the key
				out = append(out, strings.Repeat(" ", itemIndent)+key)
				out = append(out, wrapWords(text, width, itemIndent+4, itemIndent+4)...)
			}
			item, key = nil, ""
		}
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		switch {
		case strings.TrimSpace(line) == "":
			flush()
			out = append(out, "")
		case indent == 0:
			if len(item) > 0 {
				flush()
			}
			para = append(para, trimmed)
		case len(item) > 0 && indent > itemIndent:
			item = append(item, trimmed) // continuation of the current item
			hang = indent
		default:
			flush()
			item, itemIndent, hang = []string{trimmed}, indent, indent+4
			// "key   explanation" items: keep the key column, hang the
			// explanation under itself
			if i := strings.Index(trimmed, "  "); i > 0 && !strings.Contains(trimmed[:i], "#") {
				j := i
				for j < len(trimmed) && trimmed[j] == ' ' {
					j++
				}
				key, item, hang = trimmed[:i], []string{trimmed[j:]}, indent+j
			}
		}
	}
	flush()
	return strings.Join(out, "\n")
}

// wrapWords wraps s to width, the first line indented by first spaces and
// the rest by rest. A hanging indent too deep for the width is reduced.
func wrapWords(s string, width, first, rest int) []string {
	if rest > width/2 {
		rest = min(first+4, width/2)
	}
	var out []string
	line := strings.Repeat(" ", first)
	empty := true
	for _, w := range strings.Fields(s) {
		if !empty && len(line)+1+len(w) > width {
			out = append(out, line)
			line, empty = strings.Repeat(" ", rest), true
		}
		if !empty {
			line += " "
		}
		line += w
		empty = false
	}
	return append(out, line)
}

// columns prints "left  right" rows aligned when they fit the width, and
// stacked (right under left, indented) when they don't.
func columns(rows [][2]string, width int) string {
	lw := 0
	for _, r := range rows {
		lw = max(lw, len(r[0]))
	}
	var b strings.Builder
	for _, r := range rows {
		if lw+3+len(r[1]) <= width {
			b.WriteString(r[0] + strings.Repeat(" ", lw-len(r[0])+3) + r[1] + "\n")
			continue
		}
		b.WriteString(r[0] + "\n")
		for _, l := range wrapWords(r[1], width, 6, 6) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}
