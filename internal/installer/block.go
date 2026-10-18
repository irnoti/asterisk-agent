package installer

import (
	"fmt"
	"strings"
)

func markers(tag string) (string, string) {
	return fmt.Sprintf("; BEGIN irnoti-asterisk (%s) — managed, do not edit", tag), fmt.Sprintf("; END irnoti-asterisk (%s)", tag)
}

// RemoveBlock deletes a previously written managed block (and the blank line before it), leaving everything else untouched.
func RemoveBlock(content, tag string) string {
	begin, end := markers(tag)
	i := strings.Index(content, begin)
	if i < 0 {
		return content
	}
	j := strings.Index(content[i:], end)
	if j < 0 {
		return content // a half block is left for a human to look at rather than guessed at
	}
	tail := content[i+j+len(end):]
	tail = strings.TrimPrefix(tail, "\n")
	head := strings.TrimRight(content[:i], "\n")
	if head == "" {
		return tail
	}
	if tail == "" {
		return head + "\n"
	}
	return head + "\n\n" + tail
}

// UpsertBlock puts `body` between the managed markers, replacing an earlier block with the same tag, and appends otherwise.
func UpsertBlock(content, tag, body string) string {
	begin, end := markers(tag)
	block := begin + "\n" + strings.TrimRight(body, "\n") + "\n" + end + "\n"
	base := RemoveBlock(content, tag)
	if strings.TrimSpace(base) == "" {
		return block
	}
	return strings.TrimRight(base, "\n") + "\n\n" + block
}

// HasBlock reports whether the managed block with this tag is present.
func HasBlock(content, tag string) bool {
	begin, _ := markers(tag)
	return strings.Contains(content, begin)
}
