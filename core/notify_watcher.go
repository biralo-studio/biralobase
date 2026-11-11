package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/biralo-studio/biralobase/tools/hook"
	"github.com/biralo-studio/biralobase/tools/routine"
	"github.com/biralo-studio/biralobase/tools/security"
	"github.com/fatih/color"
	"github.com/fsnotify/fsnotify"
)

const systemHookIdNotifyWatcher = "__pbNotifyWatcherSystemHook__"

type notifyDirWatcher struct {
	*fsnotify.Watcher
	done chan struct{}
	// goroutine that delivers notifications and runs their reload hooks
	deliverer atomic.Uint64
}

func (w *notifyDirWatcher) Close() error {
	err := w.Watcher.Close()
	// A reload hook may itself clear or restart the application. That call
	// arrives on the delivering goroutine, which cannot wait for its own exit;
	// it stops as soon as the hook returns.
	if w.deliverer.Load() == goroutineID() {
		return err
	}
	<-w.done // no notification may read app database handles after cleanup
	return err
}

// goroutineID identifies the calling goroutine. The runtime has no accessor
// for it; the "goroutine N [state]:" header of a stack trace is the stable way
// to read it. Zero is never a valid identifier.
func goroutineID() uint64 {
	var header [64]byte
	fields := bytes.Fields(header[:runtime.Stack(header[:], false)])
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(string(fields[1]), 10, 64)
	return id
}

func (app *BaseApp) registerNotifyWatcherHooks() {
	var notifyWatcher *notifyDirWatcher

	instanceId := "@" + security.PseudorandomString(10)

	localNotifyDirPath := filepath.Join(app.DataDir(), LocalNotifyDirName)
	settingsFile := filepath.Join(localNotifyDirPath, "settings"+instanceId)
	collectionsFile := filepath.Join(localNotifyDirPath, "collections"+instanceId)

	// init
	app.OnBootstrap().Bind(&hook.Handler[*BootstrapEvent]{
		Id: systemHookIdNotifyWatcher,
		Func: func(e *BootstrapEvent) error {
			err := e.Next()
			if err != nil {
				return err
			}

			if notifyWatcher != nil {
				_ = notifyWatcher.Close()
			}

			notifyWatcher, err = createNotifyDirWatcher(e.App, instanceId, localNotifyDirPath)
			if err != nil {
				e.App.Logger().Warn("Notify dir watcher failure.", "error", err)
			}

			return nil
		},
		Priority: -998,
	})
	app.OnBootstrapClear().Bind(&hook.Handler[*BootstrapEvent]{
		Id:       systemHookIdNotifyWatcher,
		Priority: -998,
		Func: func(e *BootstrapEvent) error {
			if notifyWatcher != nil {
				_ = notifyWatcher.Close()
			}
			return e.Next()
		},
	})

	// cleanup
	app.OnTerminate().Bind(&hook.Handler[*TerminateEvent]{
		Id: systemHookIdNotifyWatcher,
		Func: func(e *TerminateEvent) error {
			if notifyWatcher != nil {
				_ = notifyWatcher.Close()
			}

			_ = os.Remove(settingsFile)
			_ = os.Remove(collectionsFile)

			return e.Next()
		},
		Priority: -998,
	})

	// ---------------------------------------------------------------

	settingsNotify := func(e *ModelEvent) error {
		err := e.Next()
		if err != nil || e.Model.PK() != paramsKeySettings {
			return err
		}

		if notifyWatcher != nil {
			if err := os.WriteFile(settingsFile, nil, 0644); err != nil {
				e.App.Logger().Warn("Failed to write watcher file", "error", err, "file", settingsFile)
			}
			// Keep the signal until termination. Creating and immediately deleting
			// it can be coalesced into only a Remove event by kqueue, losing the
			// notification. Subsequent writes update the existing signal file.
		}

		return nil
	}
	app.OnModelAfterCreateSuccess(paramsTable).Bind(&hook.Handler[*ModelEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     settingsNotify,
		Priority: 999,
	})
	app.OnModelAfterUpdateSuccess(paramsTable).Bind(&hook.Handler[*ModelEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     settingsNotify,
		Priority: 999,
	})

	// ---------------------------------------------------------------

	collectionsNotify := func(e *CollectionEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		if notifyWatcher != nil {
			if err := os.WriteFile(collectionsFile, nil, 0644); err != nil {
				e.App.Logger().Warn("Failed to write watcher file", "error", err, "file", collectionsFile)
			}
		}

		return nil
	}
	app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     collectionsNotify,
		Priority: 999,
	})
	app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     collectionsNotify,
		Priority: 999,
	})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     collectionsNotify,
		Priority: 999,
	})
}

func createNotifyDirWatcher(app App, instanceId string, localNotifyDirPath string) (*notifyDirWatcher, error) {
	// create the notify dir (if not already)
	err := os.MkdirAll(localNotifyDirPath, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("failed to create a notify dir: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to init notify dir watcher: %w", err)
	}

	err = watcher.Add(localNotifyDirPath)
	if err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("unable to watch notify dir: %w", err)
	}

	var debounceTimer *time.Timer
	var debounce <-chan time.Time
	// Signals of both kinds can arrive within one debounce window; remember
	// each kind rather than only the most recent event.
	var settingsPending, collectionsPending bool
	wrapped := &notifyDirWatcher{Watcher: watcher, done: make(chan struct{})}

	stopDebounceTimer := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
			debounceTimer = nil
		}
		debounce = nil
	}

	// watch
	routine.FireAndForget(func() {
		defer close(wrapped.done)
		defer stopDebounceTimer()
		wrapped.deliverer.Store(goroutineID())

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				// modified from within the current app instance or cleanup event
				if strings.HasSuffix(event.Name, instanceId) || event.Has(fsnotify.Remove) || !app.IsBootstrapped() {
					continue
				}

				filename := filepath.Base(event.Name)
				switch {
				case strings.HasPrefix(filename, "settings@"):
					settingsPending = true
				case strings.HasPrefix(filename, "collections@"):
					collectionsPending = true
				default:
					continue
				}

				stopDebounceTimer()
				debounceTimer = time.NewTimer(50 * time.Millisecond)
				debounce = debounceTimer.C
			case <-debounce:
				stopDebounceTimer()
				reloadSettings, reloadCollections := settingsPending, collectionsPending
				settingsPending, collectionsPending = false, false

				if reloadSettings && app.IsBootstrapped() {
					app.Logger().Debug("Reloading settings after notify event")

					err := app.ReloadSettings()
					if err != nil {
						app.Logger().Warn("Failed to reload app settings after notify", "error", err)
					}
				}

				// a settings reload hook may have cleared the bootstrap
				if reloadCollections && app.IsBootstrapped() {
					app.Logger().Debug("Reloading cached collections after notify event")

					err := app.ReloadCachedCollections()
					if err != nil {
						app.Logger().Warn("Failed to reload cached collections after notify", "error", err)
					}
				}
			case err, ok := <-watcher.Errors:
				if app.IsDev() && err != nil {
					color.Red("Notify dir watch error:", err)
				}

				if !ok {
					return
				}
			}
		}
	})

	return wrapped, err
}
