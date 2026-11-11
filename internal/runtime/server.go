// Package runtime assembles the immutable application release and server.
package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/bundle"
	"github.com/biralo-studio/biralobase/internal/functions"
	"github.com/biralo-studio/biralobase/internal/jobs"
	"github.com/biralo-studio/biralobase/internal/mcpserver"
	"github.com/biralo-studio/biralobase/internal/oauth"
	"github.com/biralo-studio/biralobase/internal/provision"
	"github.com/biralo-studio/biralobase/internal/schema"
	"github.com/biralo-studio/biralobase/storage/turso"
	"github.com/biralo-studio/biralobase/tools/router"
	"github.com/biralo-studio/biralobase/tools/security"
)

// Version is replaced by the runtime image build, and must match the builder.
var Version = "0.1.0-dev"

type Server struct {
	App         core.App
	Schema      schema.Schema
	Config      provision.Config
	Engine      *turso.Engine
	Functions   *functions.Worker
	Queue       *jobs.Queue
	OAuth       *oauth.Server
	API         http.Handler
	MCP         http.Handler
	keyEnv      string
	runMu       sync.Mutex
	closing     bool
	jobsStarted bool
	unlock      func()
	once        sync.Once
	closeErr    error
}

func Open(ctx context.Context, config provision.Config) (_ *Server, err error) {
	if err = config.ValidateServer(); err != nil {
		return nil, err
	}
	if !config.Dev {
		if err = bundle.Verify(config.AppDir, Version, bundle.Platform()); err != nil {
			return nil, err
		}
	}
	s, err := schema.Read(filepath.Join(config.AppDir, schema.FileName))
	if err != nil {
		return nil, err
	}
	ms, err := schema.ReadMigrations(filepath.Join(config.AppDir, "migrations"))
	if err != nil {
		return nil, err
	}
	unlock, err := lockDataDir(config.DataDir)
	if err != nil {
		return nil, err
	}
	engine := new(turso.Engine)
	// The core settings implementation accepts an environment key. Resolve the
	// _FILE convention before bootstrap, so existing encrypted settings can load.
	encryptionEnv := "BIRALOBASE_RESOLVED_KEY_" + security.RandomString(20)
	if err = os.Setenv(encryptionEnv, config.EncryptionKey); err != nil {
		unlock()
		return nil, err
	}
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir: config.DataDir, EncryptionEnv: encryptionEnv,
		DBConnect: engine.Connect, ConcurrentWrites: true,
		DataMaxOpenConns: 16, DataMaxIdleConns: 4, AuxMaxOpenConns: 4, AuxMaxIdleConns: 2,
	})
	server := &Server{App: app, Schema: s, Config: config, Engine: engine, unlock: unlock, keyEnv: encryptionEnv}
	defer func() {
		if err != nil {
			_ = server.Close()
		}
	}()
	server.Functions, err = functions.Start(ctx, config.AppDir)
	if err != nil {
		return nil, err
	}
	if server.Functions != nil {
		if err = server.Functions.BindHooks(app); err != nil {
			return nil, err
		}
	}
	if err = app.Bootstrap(); err != nil {
		return nil, fmt.Errorf("bootstrap Turso: %w", err)
	}
	if err = schema.ConfigureSecrets(app, config.OAuthSecretsJSON); err != nil {
		return nil, err
	}
	if config.Dev {
		if err = schema.ReconcileDevelopment(app, s, ms); err != nil {
			return nil, err
		}
	}
	if pending, checkErr := schema.Pending(app, s, ms); checkErr != nil {
		return nil, checkErr
	} else if pending {
		name := "biralobase_before_" + time.Now().UTC().Format("20060102T150405.000000000") + ".zip"
		if err = app.CreateBackup(ctx, name); err != nil {
			return nil, fmt.Errorf("pre-migration backup: %w", err)
		}
	}
	if err = schema.Apply(app, s, ms); err != nil {
		return nil, err
	}
	if err = provision.ApplySettings(app, s.Settings, config.SettingsJSON, config.PublicURL); err != nil {
		return nil, err
	}
	if err = provision.ApplyOAuthSecrets(app, config.OAuthSecretsJSON); err != nil {
		return nil, err
	}
	if err = provision.ReconcileAdmin(app, config.AdminEmail, config.AdminPassword); err != nil {
		return nil, err
	}
	api, err := apis.NewRouter(app)
	if err != nil {
		return nil, err
	}
	server.API, err = api.BuildMux()
	if err != nil {
		return nil, err
	}
	if server.Functions != nil {
		server.Queue, err = jobs.New(app, server.Functions.Definition)
		if err != nil {
			return nil, err
		}
		if err = server.Queue.Bind(); err != nil {
			return nil, err
		}
	}
	if len(s.MCP) > 0 {
		origin := config.PublicURL
		if origin == "" && config.Dev {
			origin = "http://" + config.HTTP
		}
		secret := config.EncryptionKey
		if secret == "" {
			keyPath := filepath.Join(config.DataDir, ".oauth-key")
			key, readErr := os.ReadFile(keyPath)
			if errors.Is(readErr, os.ErrNotExist) {
				key = []byte(security.RandomString(64))
				readErr = os.WriteFile(keyPath, key, 0600)
			}
			if readErr != nil {
				return nil, readErr
			}
			secret = string(key)
		}
		hash := sha256.Sum256([]byte("biralobase:oauth:v1\x00" + secret))
		server.OAuth, err = oauth.New(app, origin, s.AuthCollection, mcpserver.Scopes(s), hash[:])
		if err != nil {
			return nil, err
		}
		server.MCP, err = mcpserver.New(server.OAuth, s, server.API)
		if err != nil {
			return nil, err
		}
	}
	if !config.Dev {
		schema.LockProductionSchema(app)
		app.OnSettingsUpdateRequest().BindFunc(func(e *core.SettingsUpdateRequestEvent) error {
			return e.ForbiddenError("Production settings are managed by configuration", nil)
		})
	}
	app.OnBackupCreate().BindFunc(func(e *core.BackupEvent) error {
		e.Exclude = append(e.Exclude, ".biralobase.lock")
		return e.Next()
	})
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.InstallerFunc = nil
		server.Bind(e.Router)
		server.runMu.Lock()
		if server.closing {
			server.runMu.Unlock()
			return errors.New("server is shutting down")
		}
		if server.Queue != nil && !server.jobsStarted {
			server.jobsStarted = true
			go server.Queue.Run(func(ctx context.Context, job jobs.Job, payload any) error {
				_, err := server.Functions.Invoke(ctx, job.Kind, job.Target, map[string]any{"job": job, "event": payload, "admin": app})
				return err
			})
		}
		server.runMu.Unlock()
		return e.Next()
	})
	// Dashboard DDL needs exclusive transactions. Production applies DDL before
	// serving, then enables concurrent writes; local dashboard editing retains
	// exclusive transaction semantics.
	engine.SetConcurrent(!config.Dev)
	return server, nil
}

func (s *Server) Bind(routes *router.Router[*core.RequestEvent]) {
	if !s.Config.Dev {
		routes.BindFunc(func(e *core.RequestEvent) error {
			if e.Request.URL.Path == "/api/sql" && e.Request.Method == http.MethodPost {
				return e.ForbiddenError("SQL console is disabled in Git-managed production", nil)
			}
			return e.Next()
		})
	}
	if s.Functions != nil {
		s.Functions.BindHTTP(routes, s.API)
	}
	if s.Queue != nil {
		s.Queue.BindHTTP(routes)
	}
	if s.OAuth != nil {
		s.OAuth.Bind(routes)
		routes.Any("/mcp", apis.WrapStdHandler(s.MCP))
	}
	routes.GET("/api/biralobase/ready", func(e *core.RequestEvent) error {
		if s.Functions != nil && !s.Functions.Healthy() {
			return e.JSON(503, map[string]bool{"ready": false})
		}
		return e.JSON(200, map[string]any{"ready": true, "runtime": Version})
	})
}

func (s *Server) Serve(ctx context.Context) error {
	stopped := make(chan struct{})
	var workerDone <-chan struct{}
	if s.Functions != nil {
		workerDone = s.Functions.Done()
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-workerDone:
			_ = s.Close()
		case <-stopped:
		}
	}()
	defer close(stopped)
	defer s.Close()
	err := apis.Serve(s.App, apis.ServeConfig{HttpAddr: s.Config.HTTP})
	if ctx.Err() == nil && s.Functions != nil && !s.Functions.Healthy() {
		return errors.New("functions worker exited; server stopped to preserve hook semantics")
	}
	return err
}

func (s *Server) Close() error {
	s.once.Do(func() {
		s.runMu.Lock()
		s.closing = true
		started := s.jobsStarted
		s.runMu.Unlock()
		if started {
			s.Queue.Stop()
		}
		s.closeErr = s.App.OnTerminate().Trigger(&core.TerminateEvent{App: s.App})
		if s.Functions != nil {
			s.Functions.Close()
		}
		s.closeErr = errors.Join(s.closeErr, s.App.ClearBootstrap())
		_ = os.Unsetenv(s.keyEnv)
		s.unlock()
	})
	if errors.Is(s.closeErr, http.ErrServerClosed) {
		return nil
	}
	return s.closeErr
}
