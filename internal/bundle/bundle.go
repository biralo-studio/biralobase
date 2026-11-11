// Package bundle builds and verifies immutable, read-only application releases.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/biralo-studio/biralobase/internal/schema"
)

const ManifestName = "biralobase.release.json"

type Manifest struct {
	Version    int               `json:"version"`
	Revision   string            `json:"revision"`
	Runtime    string            `json:"runtime"`
	Platform   string            `json:"platform"`
	SchemaHash string            `json:"schemaHash"`
	Files      map[string]string `json:"files"`
}

func Build(source, destination, revision, runtimeID, platform string) (Manifest, error) {
	var m Manifest
	if revision == "" || runtimeID == "" || platform == "" {
		return m, errors.New("revision, runtime identity, and platform are required")
	}
	if err := schema.Check(source); err != nil {
		return m, err
	}
	if _, err := os.Stat(filepath.Join(source, "functions", "index.ts")); err == nil {
		if _, err := os.Stat(filepath.Join(source, "functions", "dist", "index.js")); err != nil {
			return m, errors.New("compile application functions with npm run build before bundling")
		}
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return m, errors.New("release destination must not already exist")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return m, err
	}
	temp, err := os.MkdirTemp(filepath.Dir(destination), ".biralobase-release-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(temp)
	if err = os.Chmod(temp, 0755); err != nil {
		return m, err
	}
	for _, name := range []string{schema.FileName, "migrations", "functions/dist", "node_modules", "package.json", "package-lock.json"} {
		src := filepath.Join(source, name)
		if _, err = os.Lstat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return m, err
		}
		if err = copyTree(src, filepath.Join(temp, name)); err != nil {
			return m, err
		}
	}
	s, err := schema.Read(filepath.Join(temp, schema.FileName))
	if err != nil {
		return m, err
	}
	files, err := checksums(temp)
	if err != nil {
		return m, err
	}
	m = Manifest{Version: 1, Revision: revision, Runtime: runtimeID, Platform: platform, SchemaHash: schema.Hash(s), Files: files}
	if err = schema.Write(filepath.Join(temp, ManifestName), m); err != nil {
		return m, err
	}
	if err = os.Chmod(filepath.Join(temp, ManifestName), 0644); err != nil {
		return m, err
	}
	if err = Verify(temp, runtimeID, platform); err != nil {
		return m, err
	}
	return m, os.Rename(temp, destination)
}

func Verify(root, runtimeID, platform string) error {
	b, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return err
	}
	if len(b) > 16<<20 {
		return errors.New("release manifest exceeds limit")
	}
	var m Manifest
	if err = schema.Decode(b, &m); err != nil {
		return err
	}
	if m.Version != 1 || m.Revision == "" || m.Runtime != runtimeID || m.Platform != platform {
		return errors.New("release runtime, platform, or format mismatch")
	}
	actual, err := checksums(root)
	if err != nil {
		return err
	}
	if len(actual) != len(m.Files) {
		return errors.New("release file inventory differs from manifest")
	}
	for path, sum := range m.Files {
		if actual[path] != sum {
			return fmt.Errorf("release checksum mismatch: %s", path)
		}
	}
	s, err := schema.Read(filepath.Join(root, schema.FileName))
	if err != nil {
		return err
	}
	if schema.Hash(s) != m.SchemaHash {
		return errors.New("release schema hash mismatch")
	}
	return nil
}

func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func checksums(root string) (map[string]string, error) {
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release symlinks are not allowed: %s", rel)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("release contains non-regular file: %s", rel)
		}
		if rel == ManifestName {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, readErr := io.Copy(h, f)
		closeErr := f.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return err
		}
		result[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return result, err
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		// npm command symlinks are build-time conveniences, never runtime code.
		if d.IsDir() && d.Name() == ".bin" {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("dependency symlink %q: install with npm --install-links", path)
		}
		target := filepath.Join(destination, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !d.Type().IsRegular() {
			return errors.New("bundle inputs must be regular files")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := fs.FileMode(0644)
		if info.Mode()&0111 != 0 {
			mode = 0755
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errors.Join(err, out.Close())
	})
}

// ReadManifest is useful to deployment tooling without exposing secret config.
func ReadManifest(root string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return m, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return m, errors.New("empty manifest")
	}
	err = json.Unmarshal(b, &m)
	return m, err
}
