package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func plain(in string) (*UI, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return NewPlain(strings.NewReader(in), out), out
}

func TestInputDefaultAndValidation(t *testing.T) {
	u, _ := plain("\nbad\n42\n")
	got, err := u.Input("x", "dflt", "", nil)
	if err != nil || got != "dflt" {
		t.Fatalf("default: %q %v", got, err)
	}
	num := func(s string) error {
		if s != "42" {
			return errors.New("not 42")
		}
		return nil
	}
	got, err = u.Input("n", "", "", num)
	if err != nil || got != "42" {
		t.Fatalf("validate loop: %q %v", got, err)
	}
}

func TestInputEOFAborts(t *testing.T) {
	u, _ := plain("")
	if _, err := u.Input("x", "", "", nil); !errors.Is(err, ErrAborted) {
		t.Fatalf("want ErrAborted, got %v", err)
	}
}

func TestSelectAndConfirmPlain(t *testing.T) {
	u, out := plain("2\n\ny\nn\n\n")
	opts := []Option{{Label: "a"}, {Label: "b", Hint: "h"}}
	if i, err := u.Select("pick", opts, 0); err != nil || i != 1 {
		t.Fatalf("select 2: %d %v", i, err)
	}
	if i, _ := u.Select("pick", opts, 0); i != 0 {
		t.Fatalf("select default: %d", i)
	}
	if ok, _ := u.Confirm("q", false); !ok {
		t.Fatal("y should confirm")
	}
	if ok, _ := u.Confirm("q", true); ok {
		t.Fatal("n should decline")
	}
	if ok, _ := u.Confirm("q", true); !ok {
		t.Fatal("empty should take the default")
	}
	if !strings.Contains(out.String(), "b  — h") {
		t.Errorf("hint missing:\n%s", out.String())
	}
}

func TestTaskReportsFailureLines(t *testing.T) {
	u, out := plain("")
	err := u.Task(2, 5, "build", func(log func(string)) error {
		log("compiling")
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("error lost")
	}
	s := out.String()
	if !strings.Contains(s, "[2/5]") || !strings.Contains(s, "✘") || !strings.Contains(s, "compiling") {
		t.Errorf("failure output incomplete:\n%s", s)
	}
}

func TestPanelRowsHaveEqualWidth(t *testing.T) {
	u, out := plain("")
	u.Panel("Title", [][2]string{{"a", "short"}, {"longer key", "a much longer value ✔"}})
	var w int
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		n := visible(l)
		if w == 0 {
			w = n
		}
		if n != w {
			t.Errorf("line width %d != %d: %q", n, w, l)
		}
	}
}
