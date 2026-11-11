// Package functions hosts trusted application code in a separate Node process.
// It is a deployment/runtime boundary, not a sandbox for untrusted tenants.
package functions

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed worker.mjs
var source string

type Hook struct {
	Event    string   `json:"event"`
	Tags     []string `json:"tags"`
	Priority int      `json:"priority"`
}
type Event struct {
	Collection string   `json:"collection"`
	Actions    []string `json:"actions"`
}
type Schedule struct {
	Cron string `json:"cron"`
}
type Definition struct {
	Hooks     []Hook              `json:"hooks"`
	Functions []string            `json:"functions"`
	Events    map[string]Event    `json:"events"`
	Schedules map[string]Schedule `json:"schedules"`
}
type remoteError struct {
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}
type message struct {
	Kind     string            `json:"kind"`
	Scope    string            `json:"scope,omitempty"`
	ID       string            `json:"id,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Op       string            `json:"op,omitempty"`
	Name     string            `json:"name,omitempty"`
	Category string            `json:"category,omitempty"`
	Args     []json.RawMessage `json:"args,omitempty"`
	Value    json.RawMessage   `json:"value,omitempty"`
	Error    *remoteError      `json:"error,omitempty"`
}

type Worker struct {
	Definition Definition
	cmd        *exec.Cmd
	input      io.WriteCloser
	output     io.ReadCloser
	writes     sync.Mutex
	mu         sync.Mutex
	scopes     map[string]chan message
	sequence   atomic.Uint64
	done       chan struct{}
	kill       sync.Once
	close      sync.Once
	Timeout    time.Duration
}

var validName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

// Start loads a precompiled ESM entry point. Dependencies are shipped in the
// immutable bundle; neither npm installs nor network fetches occur at startup.
func Start(ctx context.Context, root string) (*Worker, error) {
	entry, err := filepath.Abs(filepath.Join(root, "functions", "dist", "index.js"))
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(entry); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("node", "--disable-proto=throw", "--input-type=module", "-e", source, entry)
	cmd.Dir = filepath.Dir(filepath.Dir(filepath.Dir(entry)))
	cmd.ExtraFiles = []*os.File{write}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// Provisioning and encryption credentials are not inherited by functions.
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "PATH" || key == "HOME" || key == "TZ" || key == "LANG" || strings.HasPrefix(key, "BIRALOBASE_FUNCTION_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		read.Close()
		write.Close()
		return nil, err
	}
	w := &Worker{cmd: cmd, input: input, output: read, scopes: map[string]chan message{}, done: make(chan struct{}), Timeout: 30 * time.Second}
	if err = cmd.Start(); err != nil {
		read.Close()
		write.Close()
		input.Close()
		return nil, err
	}
	write.Close()
	ready := make(chan message, 1)
	go w.read(ready)
	go func() { _ = cmd.Wait(); close(w.done) }()
	timer := time.NewTimer(w.Timeout)
	defer timer.Stop()
	select {
	case m := <-ready:
		if err = json.Unmarshal(m.Value, &w.Definition); err == nil {
			err = w.Definition.validate()
		}
		if err != nil {
			w.Close()
			return nil, err
		}
		return w, nil
	case <-w.done:
		w.Close()
		return nil, errors.New("functions process exited during startup")
	case <-ctx.Done():
		w.Close()
		return nil, ctx.Err()
	case <-timer.C:
		w.Close()
		return nil, errors.New("functions startup timed out")
	}
}

func (d Definition) validate() error {
	if len(d.Hooks)+len(d.Functions)+len(d.Events)+len(d.Schedules) > 1024 {
		return errors.New("too many function registrations")
	}
	for _, name := range d.Functions {
		if !validName.MatchString(name) {
			return fmt.Errorf("invalid function name %q", name)
		}
	}
	for name, e := range d.Events {
		if !validName.MatchString(name) || e.Collection == "" || len(e.Actions) == 0 {
			return errors.New("events require name, collection, and actions")
		}
		for _, action := range e.Actions {
			if action != "create" && action != "update" && action != "delete" {
				return errors.New("invalid event action")
			}
		}
	}
	for name := range d.Schedules {
		if !validName.MatchString(name) {
			return errors.New("invalid schedule name")
		}
	}
	return nil
}

func (w *Worker) read(ready chan<- message) {
	scanner := bufio.NewScanner(w.output)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var m message
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			break
		}
		if m.Kind == "ready" {
			select {
			case ready <- m:
			default:
			}
			continue
		}
		w.mu.Lock()
		ch := w.scopes[m.Scope]
		w.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			case <-w.done:
				return
			}
		} else if m.Kind == "call" {
			_ = w.send(message{Kind: "reply", ID: m.ID, Error: &remoteError{Message: "invocation has ended"}})
		}
	}
	w.abort()
}

func (w *Worker) send(v any) error {
	w.writes.Lock()
	defer w.writes.Unlock()
	return json.NewEncoder(w.input).Encode(v)
}
func (w *Worker) Healthy() bool {
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}
func (w *Worker) Done() <-chan struct{} { return w.done }
func (w *Worker) abort()                { w.kill.Do(func() { _ = w.cmd.Process.Kill() }) }
func (w *Worker) Close() {
	w.close.Do(func() {
		_ = w.input.Close()
		select {
		case <-w.done:
		case <-time.After(time.Second):
			w.abort()
			<-w.done
		}
		_ = w.output.Close()
	})
}

// Invoke processes bridge calls on the invoking goroutine. Consequently e.Next,
// mutations, nested hooks, and transaction-scoped App pointers preserve their
// original Go call stack and cannot race the final hook response.
func (w *Worker) Invoke(ctx context.Context, category, name string, root any) (json.RawMessage, error) {
	id := strconv.FormatUint(w.sequence.Add(1), 10)
	s := newScope()
	value, err := json.Marshal(s.export(root))
	if err != nil {
		return nil, err
	}
	ch := make(chan message, 128)
	w.mu.Lock()
	w.scopes[id] = ch
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.scopes, id); w.mu.Unlock() }()
	if err = w.send(message{Kind: "invoke", Scope: id, Category: category, Name: name, Value: value}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(w.Timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			w.abort()
			return nil, ctx.Err()
		case <-w.done:
			return nil, errors.New("functions process exited")
		case <-timer.C:
			w.abort()
			return nil, errors.New("function exceeded bridge timeout")
		case m := <-ch:
			if m.Kind == "result" {
				if m.Error != nil {
					return nil, functionError(m.Error)
				}
				decoded, err := s.decodeWire(m.Value)
				if err != nil {
					return nil, err
				}
				return json.Marshal(decoded)
			}
			if m.Kind != "call" {
				w.abort()
				return nil, errors.New("invalid function protocol")
			}
			timer.Stop()
			result, callErr := s.call(m)
			response := message{Kind: "reply", ID: m.ID}
			if callErr != nil {
				response.Error = &remoteError{Message: callErr.Error()}
			} else {
				response.Value, callErr = json.Marshal(s.export(result))
				if callErr != nil {
					response.Error = &remoteError{Message: "unsupported bridge return value"}
				}
			}
			if err = w.send(response); err != nil {
				return nil, err
			}
			timer.Reset(w.Timeout)
		}
	}
}
