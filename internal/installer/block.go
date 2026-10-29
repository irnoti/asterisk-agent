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

// StripBlockExact removes the managed block and nothing else, byte for byte (RemoveBlock also tidies blank lines). It lets a block that sits
// in the middle of a file be replaced in place with the same result every time.
func StripBlockExact(content, tag string) string {
	begin, end := markers(tag)
	i := strings.Index(content, begin)
	if i < 0 {
		return content
	}
	j := strings.Index(content[i:], end)
	if j < 0 {
		return content
	}
	tail := strings.TrimPrefix(content[i+j+len(end):], "\n")
	return content[:i] + tail
}

// InsertInSection places a managed block at the END of a named section of an INI-style Asterisk file, so its options land in the section
// Asterisk reads: a second [general] header is ignored (only the first section of a name is read), and packaged files often set the same
// options earlier in it (Debian's rtp.conf has rtpstart/rtpend; the last assignment wins). Without the section it adds one.
func InsertInSection(content, section, tag, body string) string {
	content = StripBlockExact(content, tag)
	block := strings.TrimRight(UpsertBlock("", tag, body), "\n")
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if !strings.EqualFold(strings.TrimSpace(l), "["+section+"]") {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if t := strings.TrimSpace(lines[j]); strings.HasPrefix(t, "[") {
				end = j
				break
			}
		}
		at := end // keep blank lines and comments that trail the section next to the following header
		for at > i+1 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		out := append([]string{}, lines[:at]...)
		out = append(out, block)
		return strings.Join(append(out, lines[at:]...), "\n")
	}
	return strings.TrimRight(content, "\n") + "\n\n[" + section + "]\n" + block + "\n"
}

// InsertAtSectionStart is InsertInSection for options that Asterisk reads with a first-match lookup (rtp.conf's rtpstart/rtpend): the block goes
// right after the section header, so it wins over the same options the package already set further down.
func InsertAtSectionStart(content, section, tag, body string) string {
	content = StripBlockExact(content, tag)
	block := strings.TrimRight(UpsertBlock("", tag, body), "\n")
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if strings.EqualFold(strings.TrimSpace(l), "["+section+"]") {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, block)
			return strings.Join(append(out, lines[i+1:]...), "\n")
		}
	}
	return strings.TrimRight(content, "\n") + "\n\n[" + section + "]\n" + block + "\n"
}
