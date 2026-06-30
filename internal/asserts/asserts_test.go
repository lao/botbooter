package asserts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeT records Errorf calls so the assertion helpers can be tested without
// failing the real test.
type fakeT struct{ errs int }

func (f *fakeT) Helper()               {}
func (f *fakeT) Errorf(string, ...any) { f.errs++ }

// wantErrs checks how many failures f has recorded so far using a raw
// comparison, not one of the helpers under test, so a helper with an inverted
// branch cannot mask itself.
func wantErrs(t *testing.T, f *fakeT, want int, when string) {
	t.Helper()
	if f.errs != want {
		t.Errorf("%s: got %d Errorf calls, want %d", when, f.errs, want)
	}
}

func TestEqual(t *testing.T) {
	f := &fakeT{}
	Equal(f, 1, 1, "equal")
	wantErrs(t, f, 0, "equal inputs pass")
	Equal(f, 1, 2, "unequal")
	wantErrs(t, f, 1, "unequal inputs fail")
}

func TestNotNil(t *testing.T) {
	f := &fakeT{}
	NotNil(f, "x", "non-nil")
	wantErrs(t, f, 0, "non-nil passes")
	NotNil(f, nil, "nil")
	wantErrs(t, f, 1, "nil fails")
}

func TestError(t *testing.T) {
	f := &fakeT{}
	Error(f, errors.New("boom"), "has error")
	wantErrs(t, f, 0, "an error passes")
	Error(f, nil, "no error")
	wantErrs(t, f, 1, "a nil error fails")
}

func TestErrorIs(t *testing.T) {
	target := errors.New("target")
	f := &fakeT{}
	ErrorIs(f, fmtWrap(target), target, "matches")
	wantErrs(t, f, 0, "a wrapped target matches")
	ErrorIs(f, errors.New("other"), target, "no match")
	wantErrs(t, f, 1, "an unrelated error fails")
}

func TestNoError(t *testing.T) {
	f := &fakeT{}
	NoError(f, nil, "no error")
	wantErrs(t, f, 0, "a nil error passes")
	NoError(f, errors.New("boom"), "has error")
	wantErrs(t, f, 1, "a non-nil error fails")
}

func TestTrue(t *testing.T) {
	f := &fakeT{}
	True(f, true, "true")
	wantErrs(t, f, 0, "true passes")
	True(f, false, "false")
	wantErrs(t, f, 1, "false fails")
}

func TestFalse(t *testing.T) {
	f := &fakeT{}
	False(f, false, "false")
	wantErrs(t, f, 0, "false passes")
	False(f, true, "true")
	wantErrs(t, f, 1, "true fails")
}

func fmtWrap(err error) error { return errors.Join(errors.New("wrap"), err) }

func writeGo(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckBannedImports(t *testing.T) {
	t.Run("clean package passes", func(t *testing.T) {
		dir := t.TempDir()
		writeGo(t, dir, "ok.go", "package ok\nimport \"fmt\"\nvar _ = fmt.Sprint")
		f := &fakeT{}
		CheckBannedImports(f, dir, []string{"discordgo"}, "ok")
		wantErrs(t, f, 0, "a clean package raises no failures")
	})

	t.Run("banned import fails", func(t *testing.T) {
		dir := t.TempDir()
		writeGo(t, dir, "bad.go", "package bad\nimport _ \"github.com/bwmarrin/discordgo\"")
		f := &fakeT{}
		CheckBannedImports(f, dir, []string{"discordgo"}, "bad")
		wantErrs(t, f, 1, "a banned import is reported")
	})

	t.Run("test files are ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeGo(t, dir, "ok.go", "package ok\nimport \"fmt\"\nvar _ = fmt.Sprint")
		writeGo(t, dir, "ok_test.go", "package ok\nimport _ \"github.com/bwmarrin/discordgo\"")
		f := &fakeT{}
		CheckBannedImports(f, dir, []string{"discordgo"}, "ok")
		wantErrs(t, f, 0, "_test.go imports are not inspected")
	})

	t.Run("empty package fails the inspected guard", func(t *testing.T) {
		f := &fakeT{}
		CheckBannedImports(f, t.TempDir(), []string{"discordgo"}, "empty")
		wantErrs(t, f, 1, "an empty dir trips the checked>0 guard")
	})

	t.Run("unparseable file fails", func(t *testing.T) {
		dir := t.TempDir()
		writeGo(t, dir, "broken.go", "package broken\nimport (")
		f := &fakeT{}
		CheckBannedImports(f, dir, []string{"discordgo"}, "broken")
		wantErrs(t, f, 1, "a parse error is reported and aborts the scan")
	})
}
