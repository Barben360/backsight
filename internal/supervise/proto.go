package supervise

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

// Proto is the control channel's protocol version, in every Status:
// update apply restarts a supervisor whose Proto differs.
const Proto = 1

// Request is a client's line on the control channel: one JSON object.
type Request struct {
	Op string `json:"op"` // status | stop | api-stop | api-start
}

// Status is the supervisor's answer to every request, one JSON line.
type Status struct {
	Proto   int    `json:"proto"`
	Version string `json:"version"` // backsight's
	PID     int    `json:"pid"`
	DB      string `json:"db"` // starting | running | restarting | stopped | refused
	API     string `json:"api"`
	Refusal string `json:"refusal,omitempty"`
	Venv    string `json:"venv"`
}

// ErrNotRunning is Ask's error when no supervisor of the project answers.
var ErrNotRunning = errors.New("the supervisor is not running")

const (
	opStatus   = "status"
	opStop     = "stop"
	opAPIStop  = "api-stop"
	opAPIStart = "api-start"
)

var ops = []string{opStatus, opStop, opAPIStop, opAPIStart}

// conn is one client's connection (a pipe instance, a unix socket).
type conn interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
}

// listener hands out the clients' connections until Close.
type listener interface {
	Accept() (conn, error)
	Close() error
}

// errListenerClosed is a listener's Accept after Close: serve's only end.
var errListenerClosed = errors.New("the control channel is closed")

// maxRequest bounds a request line.
const maxRequest = 4096

// Ask sends op to the project's supervisor and returns its answer:
// ErrNotRunning when none answers. stop is answered at once, before the
// stop; api-stop once the API stopped; api-start once it started (or is
// waiting for the database).
func Ask(ctx context.Context, project string, op string) (Status, error) {
	if !slices.Contains(ops, op) {
		return Status{}, fmt.Errorf("unknown supervisor request %q (want one of %v)", op, ops)
	}
	c, err := dial(ctx, project)
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = c.Close() }()
	if d, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	raw, _ := json.Marshal(Request{Op: op})
	if _, err := c.Write(append(raw, '\n')); err != nil {
		return Status{}, askErr(ctx, err)
	}
	line, err := bufio.NewReaderSize(c, maxRequest).ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) == 0 {
			// Closed without an answer: a supervisor exiting.
			return Status{}, ErrNotRunning
		}
		return Status{}, askErr(ctx, err)
	}
	var st Status
	if err := json.Unmarshal(line, &st); err != nil {
		return Status{}, fmt.Errorf("the supervisor's answer %q: %w", line, err)
	}
	return st, nil
}

func askErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("asking the supervisor: %w", ctx.Err())
	}
	return fmt.Errorf("asking the supervisor: %w", err)
}

// handler answers op; done closes when the channel shuts down, and ok is
// false when it did before an answer.
type handler func(op string, done <-chan struct{}) (st Status, ok bool)

// serve answers l's clients, each in its own goroutine, until l closes;
// the returned channel closes once every client's goroutine ended.
func serve(l listener, h handler) <-chan struct{} {
	finished := make(chan struct{})
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		defer func() {
			close(done)
			wg.Wait()
			close(finished)
		}()
		for {
			c, err := l.Accept()
			if errors.Is(err, errListenerClosed) {
				return
			}
			if err != nil {
				// One client's trouble: the channel keeps answering.
				time.Sleep(10 * time.Millisecond)
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				answerOne(c, h, done)
			}()
		}
	}()
	return finished
}

func answerOne(c conn, h handler, done <-chan struct{}) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(tm.requestWait))
	line, err := bufio.NewReaderSize(io.LimitReader(c, maxRequest), maxRequest).ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	if json.Unmarshal(line, &req) != nil || !slices.Contains(ops, req.Op) {
		// Malformed or unknown (a newer client): the status answers,
		// with Proto telling the client what this supervisor speaks.
		req.Op = opStatus
	}
	// api-stop waits for the API to stop: no deadline while the loop acts.
	_ = c.SetDeadline(time.Time{})
	st, ok := h(req.Op, done)
	if !ok {
		return
	}
	_ = c.SetDeadline(time.Now().Add(tm.requestWait))
	raw, _ := json.Marshal(st)
	_, _ = c.Write(append(raw, '\n'))
}
