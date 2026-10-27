package sounds

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func item(id, name, ext, body string) (Item, []byte) {
	return Item{ID: id, Name: name, Ext: ext, Sha256: sum([]byte(body)), Size: len(body)}, []byte(body)
}

func newSyncer(t *testing.T, store map[string][]byte) (*Syncer, *int) {
	t.Helper()
	calls := 0
	return &Syncer{
		Dir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fetch: func(_ context.Context, id string) ([]byte, error) {
			calls++
			if b, ok := store[id]; ok {
				return b, nil
			}
			return nil, errors.New("gone")
		},
	}, &calls
}

func TestSyncDownloadsKeepsAndPrunes(t *testing.T) {
	a, ab := item("1", "u7/welcome", "wav", "AAAA")
	b, bb := item("2", "u7/menu", "ulaw", "BBBBBB")
	s, calls := newSyncer(t, map[string][]byte{"1": ab, "2": bb})
	manual := filepath.Join(s.Dir, "irnoti", "mine.wav")
	_ = os.MkdirAll(filepath.Dir(manual), 0o755)
	_ = os.WriteFile(manual, []byte("operator's own file"), 0o644)

	changed, err := s.Sync(context.Background(), Manifest{Version: "v1", Files: []Item{a, b}})
	if err != nil || !changed {
		t.Fatalf("first sync: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(filepath.Join(s.Dir, Subdir, "u7", "welcome.wav")); string(got) != "AAAA" {
		t.Fatalf("file not written: %q", got)
	}

	*calls = 0
	if changed, err := s.Sync(context.Background(), Manifest{Version: "v1", Files: []Item{a, b}}); err != nil || changed || *calls != 0 {
		t.Fatalf("an unchanged manifest must download nothing: changed=%v err=%v calls=%d", changed, err, *calls)
	}

	changed, err = s.Sync(context.Background(), Manifest{Version: "v2", Files: []Item{a}})
	if err != nil || !changed {
		t.Fatalf("prune: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, Subdir, "u7", "menu.ulaw")); !os.IsNotExist(err) {
		t.Fatal("a removed prompt must be deleted")
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatal("files outside the managed directory must never be touched")
	}

	if _, err := s.Sync(context.Background(), Manifest{Version: "v3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, Subdir, "u7")); !os.IsNotExist(err) {
		t.Fatal("empty directories should be removed")
	}
}

func TestSyncRejectsBadChecksumAndKeepsOldFile(t *testing.T) {
	a, _ := item("1", "u7/welcome", "wav", "AAAA")
	s, _ := newSyncer(t, map[string][]byte{"1": []byte("TAMPERED")})
	path := filepath.Join(s.Dir, Subdir, "u7", "welcome.wav")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("old"), 0o644)
	if _, err := s.Sync(context.Background(), Manifest{Version: "v1", Files: []Item{a}}); err == nil {
		t.Fatal("a mismatching download must be reported")
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("a bad download must not replace the file: %q", got)
	}
}

func TestOneFailureDoesNotBlockTheRest(t *testing.T) {
	a, _ := item("1", "u7/gone", "wav", "AAAA")
	b, bb := item("2", "u7/ok", "wav", "BBBB")
	s, _ := newSyncer(t, map[string][]byte{"2": bb})
	changed, err := s.Sync(context.Background(), Manifest{Version: "v1", Files: []Item{a, b}})
	if err == nil || !changed {
		t.Fatalf("want an error and a change, got changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, Subdir, "u7", "ok.wav")); err != nil {
		t.Fatal("the good prompt should still be installed")
	}
}

func TestValidate(t *testing.T) {
	ok, _ := item("1", "u7/welcome", "wav", "x")
	for name, m := range map[string]Manifest{
		"traversal": {Files: []Item{{ID: "1", Name: "u7/../../etc/passwd", Ext: "wav", Sha256: ok.Sha256, Size: 1}}},
		"no user":   {Files: []Item{{ID: "1", Name: "welcome", Ext: "wav", Sha256: ok.Sha256, Size: 1}}},
		"absolute":  {Files: []Item{{ID: "1", Name: "/u7/x", Ext: "wav", Sha256: ok.Sha256, Size: 1}}},
		"ext":       {Files: []Item{{ID: "1", Name: "u7/x", Ext: "sh", Sha256: ok.Sha256, Size: 1}}},
		"sha":       {Files: []Item{{ID: "1", Name: "u7/x", Ext: "wav", Sha256: "zz", Size: 1}}},
		"size":      {Files: []Item{{ID: "1", Name: "u7/x", Ext: "wav", Sha256: ok.Sha256, Size: maxBytes + 1}}},
		"id":        {Files: []Item{{ID: "../1", Name: "u7/x", Ext: "wav", Sha256: ok.Sha256, Size: 1}}},
		"dup":       {Files: []Item{ok, ok}},
	} {
		if Validate(m) == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	if err := Validate(Manifest{Files: []Item{ok}}); err != nil {
		t.Fatal(err)
	}
}
