//go:build linux || darwin

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Never unlink the lock: replacing its inode would let a second process obtain
// a different lock while the first still owns the original inode.
func lockDataDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".biralobase.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("data directory is already owned by another Biralobase process")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
