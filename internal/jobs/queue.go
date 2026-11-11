// Package jobs implements a transactional outbox and leased, at-least-once jobs.
package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/functions"
	"github.com/biralo-studio/biralobase/tools/cron"
	"github.com/biralo-studio/biralobase/tools/hook"
	"github.com/biralo-studio/biralobase/tools/security"
	"github.com/pocketbase/dbx"
)

const table = "_bb_jobs"

// maxAttempts is the number of deliveries before a job is dead-lettered.
const maxAttempts = 10

type Job struct {
	ID       string `db:"id" json:"id"`
	Kind     string `db:"kind" json:"kind"`
	Target   string `db:"target" json:"target"`
	Payload  string `db:"payload" json:"-"`
	Attempts int    `db:"attempts" json:"attempt"`
	Lease    string `db:"lease" json:"-"`
}
type Handler func(context.Context, Job, any) error
type Queue struct {
	app        core.App
	definition functions.Definition
	schedules  map[string]*cron.Schedule
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
}

func New(app core.App, definition functions.Definition) (*Queue, error) {
	q := &Queue{app: app, definition: definition, schedules: map[string]*cron.Schedule{}, stop: make(chan struct{}), done: make(chan struct{})}
	for name, schedule := range definition.Schedules {
		parsed, err := cron.NewSchedule(schedule.Cron)
		if err != nil {
			return nil, fmt.Errorf("schedule %s: %w", name, err)
		}
		q.schedules[name] = parsed
	}
	err := app.RunInTransaction(func(tx core.App) error {
		for _, sql := range []string{
			`CREATE TABLE IF NOT EXISTS _bb_jobs (id TEXT PRIMARY KEY, kind TEXT NOT NULL, target TEXT NOT NULL, payload TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, available_at INTEGER NOT NULL, lease_until INTEGER NOT NULL DEFAULT 0, lease TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS bb_jobs_pending ON _bb_jobs(status,available_at)`,
			`CREATE TABLE IF NOT EXISTS _bb_schedule (name TEXT PRIMARY KEY, last_slot INTEGER NOT NULL)`,
		} {
			if _, err := tx.DB().NewQuery(sql).Execute(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return q, nil
}

func enqueue(app core.App, id, kind, target string, payload any, now time.Time) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("event payload exceeds 4 MiB")
	}
	_, err = app.DB().NewQuery(`INSERT INTO _bb_jobs (id,kind,target,payload,available_at,created) VALUES ({:id},{:kind},{:target},{:payload},{:now},{:now}) ON CONFLICT(id) DO NOTHING`).Bind(dbx.Params{"id": id, "kind": kind, "target": target, "payload": string(b), "now": now.UnixMilli()}).Execute()
	return err
}

// Bind wraps mutations of subscribed collections in a transaction before adding
// their outbox entries. A failed validation, hook, or commit leaves neither a
// record change nor an event. Delivery starts only after the transaction commits.
func (q *Queue) Bind() error {
	byCollection := map[string]map[string][]string{}
	for name, event := range q.definition.Events {
		c, err := q.app.FindCachedCollectionByNameOrId(event.Collection)
		if err != nil || c.System || c.IsView() {
			return fmt.Errorf("event %q must reference an application base/auth collection", name)
		}
		if byCollection[c.Id] == nil {
			byCollection[c.Id] = map[string][]string{}
		}
		for _, action := range event.Actions {
			byCollection[c.Id][action] = append(byCollection[c.Id][action], name)
		}
	}
	for id, actions := range byCollection {
		for action, targets := range actions {
			slices.Sort(targets)
			fn := func(e *core.RecordEvent) error {
				original := e.App
				defer func() { e.App = original }()
				return original.RunInTransaction(func(tx core.App) error {
					e.App = tx
					if err := e.Next(); err != nil {
						return err
					}
					for _, target := range targets {
						payload := map[string]any{"action": action, "collection": id, "record": e.Record.PublicExport()}
						if err := enqueue(tx, security.RandomString(32), "event", target, payload, time.Now()); err != nil {
							return err
						}
					}
					return nil
				})
			}
			h := &hook.Handler[*core.RecordEvent]{Id: "biralobase:outbox:" + id + ":" + action, Priority: -100000, Func: fn}
			switch action {
			case "create":
				q.app.OnRecordCreate(id).Bind(h)
			case "update":
				q.app.OnRecordUpdate(id).Bind(h)
			case "delete":
				q.app.OnRecordDelete(id).Bind(h)
			}
		}
	}
	return nil
}

// Tick persists all due UTC minute slots, including bounded catch-up after a
// restart. Unique IDs prevent duplicate enqueueing. Larger gaps catch up over
// subsequent ticks, rather than monopolizing a single startup transaction.
func (q *Queue) Tick(now time.Time) error {
	current := now.UTC().Truncate(time.Minute).Unix()
	for name, schedule := range q.schedules {
		err := q.app.RunInTransaction(func(tx core.App) error {
			var last int64
			readErr := tx.DB().NewQuery("SELECT last_slot FROM _bb_schedule WHERE name={:name}").Bind(dbx.Params{"name": name}).Row(&last)
			if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
				return readErr
			}
			if last == 0 {
				last = current - 60
			}
			if last >= current {
				return nil
			}
			limit := min(current, last+1440*60)
			for slot := last + 60; slot <= limit; slot += 60 {
				date := time.Unix(slot, 0).UTC()
				if schedule.IsDue(cron.NewMoment(date)) {
					hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", name, slot)))
					if err := enqueue(tx, hex.EncodeToString(hash[:]), "schedule", name, map[string]any{"scheduledAt": date.Format(time.RFC3339)}, now); err != nil {
						return err
					}
				}
			}
			_, err := tx.DB().NewQuery(`INSERT INTO _bb_schedule (name,last_slot) VALUES ({:name},{:slot}) ON CONFLICT(name) DO UPDATE SET last_slot=excluded.last_slot`).Bind(dbx.Params{"name": name, "slot": limit}).Execute()
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (q *Queue) Claim(now time.Time) (*Job, error) {
	for {
		job, retired, err := q.claim(now)
		if err != nil || !retired {
			return job, err
		}
	}
}

func (q *Queue) claim(now time.Time) (job *Job, retired bool, err error) {
	err = q.app.RunInTransaction(func(tx core.App) error {
		job, retired = nil, false // the transaction function may run again
		var matches []Job
		err := tx.DB().NewQuery(`SELECT id,kind,target,payload,attempts FROM _bb_jobs WHERE (status='pending' AND available_at <= {:now}) OR (status='running' AND lease_until < {:now}) ORDER BY created,id LIMIT 1`).Bind(dbx.Params{"now": now.UnixMilli()}).All(&matches)
		if err != nil || len(matches) == 0 {
			return err
		}
		// A job that has already used every attempt can only be here because
		// its last delivery was never acknowledged: the process died while
		// handling it. Retiring it stops it from being redelivered after every
		// restart; the caller then claims the job behind it.
		if matches[0].Attempts >= maxAttempts {
			_, err = tx.DB().NewQuery(`UPDATE _bb_jobs SET status='dead',last_error='delivery was never acknowledged',lease_until=0,lease='' WHERE id={:id}`).Bind(dbx.Params{"id": matches[0].ID}).Execute()
			retired = err == nil
			return err
		}
		job = &matches[0]
		job.Attempts++
		job.Lease = security.RandomString(32)
		_, err = tx.DB().NewQuery(`UPDATE _bb_jobs SET status='running',attempts={:attempts},lease={:lease},lease_until={:until} WHERE id={:id}`).Bind(dbx.Params{"id": job.ID, "attempts": job.Attempts, "lease": job.Lease, "until": now.Add(5 * time.Minute).UnixMilli()}).Execute()
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return job, retired, nil
}

func (q *Queue) Complete(job Job, handlerErr error, now time.Time) error {
	status, reason := "done", ""
	if handlerErr != nil {
		status = "pending"
		// Error messages may contain application secrets. Store a generic reason;
		// operators get the job ID/attempt count and can use their own safe logs.
		reason = "handler failed"
		if job.Attempts >= maxAttempts {
			status = "dead"
		}
	}
	delay := time.Duration(1<<min(job.Attempts, 10)) * time.Second
	_, err := q.app.DB().NewQuery(`UPDATE _bb_jobs SET status={:status},last_error={:error},available_at={:next},lease_until=0,lease='' WHERE id={:id} AND lease={:lease} AND status='running'`).Bind(dbx.Params{"status": status, "error": reason, "next": now.Add(delay).UnixMilli(), "id": job.ID, "lease": job.Lease}).Execute()
	return err
}

func (q *Queue) Run(handler Handler) {
	defer close(q.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		if err := q.Tick(time.Now()); err != nil {
			q.app.Logger().Error("schedule enqueue failed")
		}
		job, err := q.Claim(time.Now())
		if err == nil && job != nil {
			var payload any
			err = json.Unmarshal([]byte(job.Payload), &payload)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				err = handler(ctx, *job, payload)
				cancel()
			}
			if err != nil {
				q.app.Logger().Warn("job handler failed", "job", job.ID, "target", job.Target, "attempt", job.Attempts)
			}
			if err = q.Complete(*job, err, time.Now()); err != nil {
				q.app.Logger().Error("job acknowledgement failed", "job", job.ID)
			}
			continue
		}
		select {
		case <-q.stop:
			return
		case <-ticker.C:
		}
	}
}
func (q *Queue) Stop() { q.once.Do(func() { close(q.stop) }); <-q.done }
