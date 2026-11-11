//go:build !no_default_driver && turso

package core

import (
	"github.com/biralo-studio/biralobase/storage/turso"
	"github.com/pocketbase/dbx"
)

// The build tag runs the unmodified upstream assertions against Turso. It is
// separate from the production launcher, which always chooses Turso explicitly.
func DefaultDBConnect(path string) (*dbx.DB, error) {
	return new(turso.Engine).Connect(path)
}
