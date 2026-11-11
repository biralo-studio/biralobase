package jobs

import (
	"errors"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/functions"
	_ "github.com/biralo-studio/biralobase/migrations"
	"github.com/biralo-studio/biralobase/storage/turso"
)

func TestOutboxAtomicityLeasesAndScheduleRecovery(t *testing.T) {
	for _, engine := range []string{"sqlite", "turso"} {
		t.Run(engine, func(t *testing.T) {
			config := core.BaseAppConfig{DataDir: t.TempDir()}
			db := new(turso.Engine)
			if engine == "turso" {
				config.DBConnect = db.Connect
				config.ConcurrentWrites = true
			}
			app := core.NewBaseApp(config)
			if err := app.Bootstrap(); err != nil {
				t.Fatal(err)
			}
			defer app.ClearBootstrap()
			c := core.NewBaseCollection("notes")
			if err := app.Save(c); err != nil {
				t.Fatal(err)
			}
			definition := functions.Definition{Events: map[string]functions.Event{"changed": {Collection: c.Id, Actions: []string{"create", "update", "delete"}}}, Schedules: map[string]functions.Schedule{"everyMinute": {Cron: "* * * * *"}}}
			q, err := New(app, definition)
			if err != nil {
				t.Fatal(err)
			}
			if err = q.Bind(); err != nil {
				t.Fatal(err)
			}
			db.SetConcurrent(true)
			r := core.NewRecord(c)
			rollback := errors.New("abort")
			err = app.RunInTransaction(func(tx core.App) error {
				if err := tx.Save(r); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if job, err := q.Claim(time.Now()); err != nil || job != nil {
				t.Fatal("rolled back event was delivered", err)
			}
			r = core.NewRecord(c)
			if err = app.Save(r); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			job, err := q.Claim(now)
			if err != nil || job == nil {
				t.Fatal("missing committed event", err)
			}
			if other, err := q.Claim(now); err != nil || other != nil {
				t.Fatal("live lease claimed twice", err)
			}
			restarted, err := New(app, definition)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := restarted.Claim(now.Add(6 * time.Minute))
			if err != nil || claimed == nil || claimed.ID != job.ID || claimed.Attempts != 2 {
				t.Fatal("expired lease not recovered", err)
			}
			if err = q.Complete(*job, nil, now); err != nil {
				t.Fatal(err)
			} // stale acknowledgement must not complete the new lease
			if err = q.Complete(*claimed, errors.New("failure"), now); err != nil {
				t.Fatal(err)
			}
			if retry, err := q.Claim(now); err != nil || retry != nil {
				t.Fatal("retry backoff ignored", err)
			}
			retry, err := q.Claim(now.Add(time.Minute))
			if err != nil || retry == nil || retry.ID != job.ID {
				t.Fatal("retry not delivered", err)
			}
			if err = q.Complete(*retry, nil, now); err != nil {
				t.Fatal(err)
			}
			if err = q.Tick(now); err != nil {
				t.Fatal(err)
			}
			if err = q.Tick(now); err != nil {
				t.Fatal(err)
			}
			scheduled, err := q.Claim(now)
			if err != nil || scheduled == nil || scheduled.Kind != "schedule" {
				t.Fatal("schedule missing", err)
			}
			q.Complete(*scheduled, nil, now)
			if job, err := q.Claim(now); err != nil || job != nil {
				t.Fatal("schedule slot duplicated", err)
			}
			if err = restarted.Tick(now.Add(3 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				job, err := q.Claim(now.Add(4 * time.Minute))
				if err != nil || job == nil {
					t.Fatal("missed schedule not recovered", err)
				}
				q.Complete(*job, nil, now)
			}
		})
	}
}

// A job that takes the whole process down while it is being handled is never
// acknowledged, so only its lease expiry is visible. Such a job must reach the
// dead-letter state like one that fails ten times; otherwise it is redelivered
// after every restart for as long as it exists.
func TestJobThatNeverReturnsIsDeadLettered(t *testing.T) {
	db := new(turso.Engine)
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), DBConnect: db.Connect, ConcurrentWrites: true})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app.ClearBootstrap()
	q, err := New(app, functions.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	db.SetConcurrent(true)
	now := time.Now()
	if err = enqueue(app, "poison", "event", "changed", map[string]any{}, now); err != nil {
		t.Fatal(err)
	}
	if err = enqueue(app, "healthy", "event", "changed", map[string]any{}, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	for attempt := 0; attempt < 30; attempt++ {
		now = now.Add(6 * time.Minute) // the previous lease has expired
		job, err := q.Claim(now)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			continue
		}
		if job.ID == "healthy" {
			if err = q.Complete(*job, nil, now); err != nil {
				t.Fatal(err)
			}
			continue
		}
		deliveries++ // the process "crashes" here: no acknowledgement
	}
	if deliveries != maxAttempts {
		t.Fatalf("unacknowledged job was delivered %d times, want %d", deliveries, maxAttempts)
	}
	var status struct {
		Poison  string `db:"poison"`
		Healthy string `db:"healthy"`
	}
	if err = app.DB().NewQuery("SELECT (SELECT status FROM _bb_jobs WHERE id='poison') AS poison, (SELECT status FROM _bb_jobs WHERE id='healthy') AS healthy").One(&status); err != nil {
		t.Fatal(err)
	}
	if status.Poison != "dead" || status.Healthy != "done" {
		t.Fatalf("poison job is %q and the job behind it is %q", status.Poison, status.Healthy)
	}
}
