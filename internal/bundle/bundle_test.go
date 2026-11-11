package bundle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/biralo-studio/biralobase/internal/schema"
)

func TestReleaseVerification(t *testing.T) {
	source := t.TempDir()
	s, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err = schema.Write(filepath.Join(source, schema.FileName), s); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, ".env"), []byte("MUST_NOT_BE_BUNDLED"), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "release")
	if _, err = Build(source, out, "abc123", "test-runtime", Platform()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(out, ".env")); !os.IsNotExist(err) {
		t.Fatal("secret file copied")
	}
	if err = Verify(out, "test-runtime", Platform()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm()&0055 != 0055 {
		t.Fatal("release is inaccessible to non-root runtime", err)
	}
	if err = Verify(out, "other-runtime", Platform()); err == nil {
		t.Fatal("wrong runtime accepted")
	}
	if _, err = Build(source, out, "abc123", "test-runtime", Platform()); err == nil {
		t.Fatal("existing immutable release overwritten")
	}
	if err = os.WriteFile(filepath.Join(out, "injected.js"), []byte("injected"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = Verify(out, "test-runtime", Platform()); err == nil {
		t.Fatal("untracked release file accepted")
	}
	if err = os.Remove(filepath.Join(out, "injected.js")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(out, schema.FileName), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = Verify(out, "test-runtime", Platform()); err == nil {
		t.Fatal("tampered schema accepted")
	}
}

func TestRejectSymlinks(t *testing.T) {
	source := t.TempDir()
	s, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	schema.Write(filepath.Join(source, schema.FileName), s)
	os.MkdirAll(filepath.Join(source, "node_modules"), 0755)
	if err = os.Symlink("/etc/passwd", filepath.Join(source, "node_modules", "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = Build(source, filepath.Join(t.TempDir(), "release"), "abc", "runtime", Platform()); err == nil {
		t.Fatal("symlink escape accepted")
	}
}
