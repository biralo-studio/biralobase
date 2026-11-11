package runtime

import (
	"os"
	"path/filepath"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/provision"
	"github.com/biralo-studio/biralobase/internal/schema"
	"github.com/biralo-studio/biralobase/storage/turso"
)

func Export(config provision.Config) error {
	s, err := schema.Read(filepath.Join(config.AppDir, schema.FileName))
	if err != nil {
		return err
	}
	unlock, err := lockDataDir(config.DataDir)
	if err != nil {
		return err
	}
	defer unlock()
	const key = "BIRALOBASE_EXPORT_ENCRYPTION_KEY"
	if err = os.Setenv(key, config.EncryptionKey); err != nil {
		return err
	}
	defer os.Unsetenv(key)
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: config.DataDir, DBConnect: new(turso.Engine).Connect, EncryptionEnv: key})
	defer app.OnTerminate().Trigger(&core.TerminateEvent{App: app}, func(e *core.TerminateEvent) error { return app.ClearBootstrap() })
	if err = app.Bootstrap(); err != nil {
		return err
	}
	s.Collections, err = schema.Snapshot(app)
	if err != nil {
		return err
	}
	return schema.Write(filepath.Join(config.AppDir, schema.FileName), s)
}
