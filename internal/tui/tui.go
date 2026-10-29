// Package tui is the installer's terminal interface: colours, panels, a step timeline with a spinner and arrow-key menus.
// It uses nothing but the standard library so the agent stays one static binary. Without a terminal (pipes, CI) it prints plain
// lines and reads answers line by line, so every prompt can be scripted.
package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrAborted is returned when the user presses Ctrl+C or Esc at a prompt.
var ErrAborted = errors.New("aborted")

type Option struct {
	Label string
	Hint  string
}

type UI struct {
	In    *bufio.Reader
	Out   io.Writer
	inF   *os.File // set when In is a real terminal
	tty   bool
	color bool
	width int
	mu    sync.Mutex
}

// New builds a UI. Colours and interactive menus are on only when both ends are terminals and NO_COLOR is unset.
func New(in *os.File, out *os.File) *UI {
	u := &UI{In: bufio.NewReader(in), Out: out, width: 80}
	if isTTY(in) && isTTY(out) {
		u.tty, u.inF = true, in
		u.width = termWidth(out)
		u.color = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	return u
}

// NewPlain builds a UI over arbitrary streams (tests): no colours, line-based input.
func NewPlain(in io.Reader, out io.Writer) *UI {
	return &UI{In: bufio.NewReader(in), Out: out, width: 80}
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Interactive reports whether menus and spinners are live.
func (u *UI) Interactive() bool { return u.tty }

// --- colour -------------------------------------------------------------------------------------------------------------------------

const (
	brand = "38;5;202" // IRNoti orange (#FF3300 on 256-colour terminals)
	dim   = "2"
	bold  = "1"
	green = "32"
	red   = "31"
	amber = "33"
	cyan  = "36"
)

func (u *UI) c(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *UI) printf(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.Out, format, a...)
}

func visible(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}

// --- static output --------------------------------------------------------------------------------------------------------------------

var logo = []string{
	` ___ ____  _   _       _   _ `,
	`|_ _|  _ \| \ | | ___ | |_(_)`,
	` | || |_) |  \| |/ _ \| __| |`,
	` | ||  _ <| |\  | (_) | |_| |`,
	`|___|_| \_\_| \_|\___/ \__|_|`,
}

func (u *UI) Banner(title, version string) {
	u.printf("\n")
	for _, l := range logo {
		u.printf("  %s\n", u.c(brand+";"+bold, l))
	}
	u.printf("\n  %s  %s\n", u.c(bold, title), u.c(dim, version))
	u.printf("  %s\n\n", u.c(dim, strings.Repeat("─", min(u.width-4, 60))))
}

func (u *UI) Section(title string) {
	u.printf("\n  %s %s\n", u.c(brand, "◆"), u.c(bold, title))
}

func (u *UI) Info(format string, a ...any) {
	u.printf("  %s %s\n", u.c(cyan, "•"), fmt.Sprintf(format, a...))
}
func (u *UI) OK(format string, a ...any) {
	u.printf("  %s %s\n", u.c(green, "✔"), fmt.Sprintf(format, a...))
}
func (u *UI) Warn(format string, a ...any) {
	u.printf("  %s %s\n", u.c(amber, "!"), fmt.Sprintf(format, a...))
}
func (u *UI) Fail(format string, a ...any) {
	u.printf("  %s %s\n", u.c(red, "✘"), fmt.Sprintf(format, a...))
}
func (u *UI) Dim(format string, a ...any) { u.printf("    %s\n", u.c(dim, fmt.Sprintf(format, a...))) }

// Panel draws a titled box of key/value rows.
func (u *UI) Panel(title string, rows [][2]string) {
	kw, vw := 0, 0
	for _, r := range rows {
		kw = max(kw, visible(r[0]))
		vw = max(vw, visible(r[1]))
	}
	inner := max(kw+3+vw, visible(title)+2)
	inner = min(inner, u.width-6)
	top := "╭─ " + title + " " + strings.Repeat("─", max(inner-visible(title)-1, 0)) + "╮"
	u.printf("  %s\n", u.c(brand, top))
	for _, r := range rows {
		line := fmt.Sprintf("%s%s   %s", u.c(dim, r[0]), strings.Repeat(" ", kw-visible(r[0])), r[1])
		u.printf("  %s %s%s %s\n", u.c(brand, "│"), line, strings.Repeat(" ", max(inner-visible(line), 0)), u.c(brand, "│"))
	}
	u.printf("  %s\n", u.c(brand, "╰"+strings.Repeat("─", inner+2)+"╯"))
}

// --- prompts ---------------------------------------------------------------------------------------------------------------------------

func (u *UI) readLine() (string, error) {
	s, err := u.In.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", ErrAborted
	}
	return strings.TrimSpace(s), nil
}

func (u *UI) prompt(label, def, hint string) {
	d := ""
	if def != "" {
		d = u.c(dim, " ("+def+")")
	}
	u.printf("\n  %s %s%s\n", u.c(brand, "?"), u.c(bold, label), d)
	if hint != "" {
		u.printf("    %s\n", u.c(dim, hint))
	}
	u.printf("  %s ", u.c(brand, "›"))
}

// Input asks for a line; Enter keeps def. validate may be nil. It asks again until validate accepts.
func (u *UI) Input(label, def, hint string, validate func(string) error) (string, error) {
	for {
		u.prompt(label, def, hint)
		s, err := u.readLine()
		if err != nil {
			return "", err
		}
		if s == "" {
			s = def
		}
		if validate != nil {
			if verr := validate(s); verr != nil {
				u.Fail("%v", verr)
				if !u.tty && u.In.Buffered() == 0 {
					return "", verr // scripted input: do not loop forever on a bad answer
				}
				continue
			}
		}
		return s, nil
	}
}

// Secret is Input without echo.
func (u *UI) Secret(label, hint string, validate func(string) error) (string, error) {
	for {
		u.prompt(label, "", hint)
		var s string
		var err error
		if u.tty {
			s, err = u.readSecret()
			u.printf("\n")
		} else {
			s, err = u.readLine()
		}
		if err != nil {
			return "", err
		}
		if validate != nil {
			if verr := validate(s); verr != nil {
				u.Fail("%v", verr)
				if !u.tty && u.In.Buffered() == 0 {
					return "", verr
				}
				continue
			}
		}
		return s, nil
	}
}

func (u *UI) readSecret() (string, error) {
	restore, err := makeRaw(u.inF)
	if err != nil {
		return u.readLine()
	}
	defer restore()
	var buf []rune
	for {
		r, _, err := u.In.ReadRune()
		if err != nil {
			return "", ErrAborted
		}
		switch r {
		case '\r', '\n':
			return strings.TrimSpace(string(buf)), nil
		case 3, 27:
			return "", ErrAborted
		case 127, 8:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				u.printf("\b \b")
			}
		default:
			if r >= 32 {
				buf = append(buf, r)
				u.printf("%s", u.c(dim, "•"))
			}
		}
	}
}

func (u *UI) Confirm(label string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		u.printf("\n  %s %s %s ", u.c(brand, "?"), u.c(bold, label), u.c(dim, hint))
		s, err := u.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if !u.tty && u.In.Buffered() == 0 {
			return def, nil
		}
	}
}

// Select shows a menu and returns the chosen index. On a terminal: arrows/j/k move, Enter picks, digits jump. Otherwise: type a number.
func (u *UI) Select(label string, opts []Option, def int) (int, error) {
	if !u.tty {
		return u.selectPlain(label, opts, def)
	}
	restore, err := makeRaw(u.inF)
	if err != nil {
		return u.selectPlain(label, opts, def)
	}
	defer restore()
	cur := def
	draw := func(first bool) {
		if !first {
			u.printf("\x1b[%dA", len(opts))
		}
		for i, o := range opts {
			mark, text := "  ", o.Label
			if i == cur {
				mark, text = u.c(brand, "❯ "), u.c(bold, o.Label)
			}
			hint := ""
			if o.Hint != "" {
				hint = "  " + u.c(dim, o.Hint)
			}
			u.printf("\x1b[2K  %s%s%s\n", mark, text, hint)
		}
	}
	u.printf("\n  %s %s %s\n", u.c(brand, "?"), u.c(bold, label), u.c(dim, "(↑/↓, Enter)"))
	draw(true)
	for {
		r, _, err := u.In.ReadRune()
		if err != nil {
			return 0, ErrAborted
		}
		switch r {
		case '\r', '\n':
			return cur, nil
		case 3:
			return 0, ErrAborted
		case 'k':
			cur = (cur + len(opts) - 1) % len(opts)
		case 'j':
			cur = (cur + 1) % len(opts)
		case 27:
			if u.In.Buffered() == 0 {
				return 0, ErrAborted
			}
			u.In.ReadRune() // '['
			a, _, _ := u.In.ReadRune()
			switch a {
			case 'A':
				cur = (cur + len(opts) - 1) % len(opts)
			case 'B':
				cur = (cur + 1) % len(opts)
			}
		default:
			if r >= '1' && int(r-'1') < len(opts) {
				cur = int(r - '1')
			}
		}
		draw(false)
	}
}

func (u *UI) selectPlain(label string, opts []Option, def int) (int, error) {
	u.printf("\n  %s %s\n", u.c(brand, "?"), u.c(bold, label))
	for i, o := range opts {
		u.printf("    %d) %s", i+1, o.Label)
		if o.Hint != "" {
			u.printf("  — %s", o.Hint)
		}
		u.printf("\n")
	}
	for {
		u.printf("  %s [%d] ", u.c(brand, "›"), def+1)
		s, err := u.readLine()
		if err != nil {
			return 0, err
		}
		if s == "" {
			return def, nil
		}
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 1 && n <= len(opts) {
			return n - 1, nil
		}
		u.Fail("enter a number from 1 to %d", len(opts))
		if u.In.Buffered() == 0 && !u.tty {
			return def, nil
		}
	}
}

// --- tasks -----------------------------------------------------------------------------------------------------------------------------

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Task runs fn under a spinner. fn reports what it is doing through log; the last line is shown next to the spinner and the
// whole output is printed if fn fails. step/total draw the timeline counter ("[3/7]").
func (u *UI) Task(step, total int, title string, fn func(log func(string)) error) error {
	counter := u.c(dim, fmt.Sprintf("[%d/%d]", step, total))
	var (
		mu    sync.Mutex
		last  string
		lines []string
	)
	log := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		mu.Lock()
		last = s
		lines = append(lines, s)
		if len(lines) > 400 {
			lines = lines[len(lines)-400:]
		}
		mu.Unlock()
	}
	if !u.tty {
		u.printf("  %s %s …\n", counter, title)
		err := fn(func(s string) { log(s) })
		u.finish(counter, title, err, lines)
		return err
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			mu.Lock()
			l := last
			mu.Unlock()
			if w := u.width - visible(title) - 18; w > 8 && utf8.RuneCountInString(l) > w {
				l = string([]rune(l)[:w-1]) + "…"
			}
			u.printf("\r\x1b[2K  %s %s %s  %s", counter, u.c(brand, frames[i%len(frames)]), title, u.c(dim, l))
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	err := fn(log)
	close(done)
	wg.Wait()
	u.printf("\r\x1b[2K")
	u.finish(counter, title, err, lines)
	return err
}

func (u *UI) finish(counter, title string, err error, lines []string) {
	if err == nil {
		u.printf("  %s %s %s\n", counter, u.c(green, "✔"), title)
		return
	}
	u.printf("  %s %s %s\n", counter, u.c(red, "✘"), title)
	start := max(len(lines)-15, 0)
	for _, l := range lines[start:] {
		u.printf("      %s\n", u.c(dim, l))
	}
}

// Skipped prints a step that was not needed.
func (u *UI) Skipped(step, total int, title, why string) {
	u.printf("  %s %s %s %s\n", u.c(dim, fmt.Sprintf("[%d/%d]", step, total)), u.c(dim, "–"), title, u.c(dim, "("+why+")"))
}
