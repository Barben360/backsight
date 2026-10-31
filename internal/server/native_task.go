package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/supervise"
)

// The native server on Windows (decision 0080): a per-user Task Scheduler
// entry, \<project>\server, runs conhost.exe --headless <shim> server
// supervise at logon and every 5 minutes (IgnoreNew: nothing while one
// runs). The supervisor (internal/supervise) runs postgres then the API
// from supervise.json, Windows' unit file, and answers the user's named
// pipe. Whether it runs is what the pipe answers, never schtasks' output
// (localized). Every schtasks call goes through Native.Sys.

// taskHost is the Windows host: the supervisor under its scheduled task.
type taskHost struct{ n Native }

var (
	_ nativeHost     = taskHost{}
	_ dbAloneStarter = taskHost{}
)

// The task host's files under Root(), and restart-pending's markers: the
// supervisor (it reads supervise.json when it starts), the API (it reads
// server.env at each start), the task (task.xml to register again).
const (
	superviseFile  = "supervise.json"
	taskFile       = "task.xml"
	markSupervisor = "supervisor"
	markAPI        = "api"
	markTask       = "task"
)

// The task's triggers: at logon, and every taskEvery from taskEpoch on (a
// fixed start keeps task.xml the same across renders).
const (
	taskEpoch = "2026-01-01T00:00:00"
	taskEvery = 5 * time.Minute
)

// NoLogonNote is what a start without the task says: with no console
// logon (SSH, CI) the task cannot run, and the supervisor was started
// detached instead.
const NoLogonNote = "the server runs until you log off; it starts by itself at your next logon"

// What the task host waits for and asks with; tests replace them.
var (
	// superviseAsk is the pipe's client when Native.Ask is nil.
	superviseAsk = supervise.Ask
	// startSupervisor starts conhost.exe (its real path) with args,
	// detached from the caller and out of its job (native_task_*.go).
	startSupervisor = startDetachedSupervisor
	// userSID is the current user's SID, the task's principal.
	userSID           = currentUserSID
	taskRunWait       = 10 * time.Second  // the pipe answering after /Run or a detached start
	superviseStopWait = 100 * time.Second // the pipe gone after stop: the API's 30 s, then pg_ctl's 60 s
	supervisePoll     = 200 * time.Millisecond
	askStatusWait     = 10 * time.Second // a status answer
	askOpWait         = 2 * time.Minute  // api-stop waits for the API to stop
)

// SuperviseFile is the supervisor's configuration: Render writes it on
// Windows, server supervise reads it.
func (n Native) SuperviseFile() string { return n.join(n.Root(), superviseFile) }

// TaskName is the server's scheduled task, \<project>\server.
func (n Native) TaskName() string { return `\` + n.project() + `\server` }

// LogDir holds the supervisor's logs: <project>-{supervisor,db,api}.log.
func (n Native) LogDir() string { return n.join(n.Dirs.State, "logs") }

func (h taskHost) Dir() string { return h.n.Root() }

func (h taskHost) fileNames() []string { return []string{superviseFile, taskFile} }

func (h taskHost) markers() []string { return []string{markSupervisor, markAPI, markTask} }

// restartsFor: supervise.json restarts the supervisor, server.env the API
// (the supervisor reads it at each API start), task.xml registers the task
// again (a running instance keeps running, E34 W1b).
func (h taskHost) restartsFor(name string) []string {
	switch name {
	case superviseFile:
		return []string{markSupervisor}
	case taskFile:
		return []string{markTask}
	}
	return []string{markAPI}
}

func (h taskHost) StopCmd() string { return "backsight server stop" }

// Files are supervise.json, for the database on port pg and the API of
// venv, and task.xml. Neither holds a secret: the API's come from
// server.env, which the supervisor reads.
func (h taskHost) Files(pg int, venv string) (map[string][]byte, error) {
	n := h.n
	port := fmt.Sprint(pg)
	postgres := []string{n.pgBin("postgres"), "-D", n.DataDir(), "-p", port, "-c", "fsync=on", "-c", "listen_addresses=127.0.0.1"}
	for _, s := range pgSettings {
		postgres = append(postgres, "-c", s)
	}
	cfg := supervise.Config{
		Project:   n.project(),
		DataDir:   n.DataDir(),
		Postgres:  postgres,
		PGIsReady: []string{n.pgBin("pg_isready"), "-q", "-h", "127.0.0.1", "-p", port},
		PGCtlStop: []string{n.pgBin("pg_ctl"), "stop", "-m", "fast", "-D", n.DataDir()},
		API:       []string{n.venvBin(venv, "hindsight-api")},
		Env:       n.windowsEnv(n.join(n.installation(), "bin")),
		EnvFile:   n.EnvFile(),
		LogDir:    n.LogDir(),
		Venv:      venv,
	}
	var sup bytes.Buffer
	enc := json.NewEncoder(&sup)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	sid, err := userSID()
	if err != nil {
		return nil, fmt.Errorf("the task's principal: %w", err)
	}
	args := []string{"server", "supervise"}
	if home := n.Env["BACKSIGHT_HOME"]; home != "" {
		args = append(args, "--backsight-home", home)
	}
	task, err := scheduler.TaskXML(scheduler.TaskSpec{
		Description: "backsight memory server (" + n.project() + ")",
		UserSID:     sid,
		Triggers:    []scheduler.Trigger{{Kind: scheduler.TriggerLogon}, {Kind: scheduler.TriggerTime, Start: taskEpoch, Every: taskEvery}},
		Program:     n.shim(),
		Args:        args,
		Priority:    5, // normal: Task Scheduler's default 7 is below normal, for the CPU and the disk
	})
	if err != nil {
		return nil, err
	}
	return map[string][]byte{superviseFile: sup.Bytes(), taskFile: task}, nil
}

// superviseArgs are conhost's arguments for the supervisor: the task's
// action, with --no-api for the database alone.
func (h taskHost) superviseArgs(noAPI bool) []string {
	args := []string{"server", "supervise"}
	if noAPI {
		args = append(args, "--no-api")
	}
	if home := h.n.Env["BACKSIGHT_HOME"]; home != "" {
		args = append(args, "--backsight-home", home)
	}
	return scheduler.HeadlessArgs(h.n.shim(), args...)
}

func (h taskHost) schtasks(ctx context.Context, args ...string) ([]byte, error) {
	return h.n.sys(ctx, "schtasks", args...)
}

// registered: Task Scheduler knows the task (its exit status only).
func (h taskHost) registered(ctx context.Context) bool {
	_, err := h.n.Sys.Run(ctx, "schtasks", "/Query", "/TN", h.n.TaskName())
	return err == nil
}

// ask sends op to the supervisor, within a bound: supervise.ErrNotRunning
// when none answers.
func (h taskHost) ask(ctx context.Context, op string) (supervise.Status, error) {
	wait := askOpWait
	if op == "status" {
		wait = askStatusWait
	}
	actx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ask := h.n.Ask
	if ask == nil {
		ask = superviseAsk
	}
	return ask(actx, h.n.project(), op)
}

// running: the supervisor answers.
func (h taskHost) running(ctx context.Context) (bool, error) {
	_, err := h.ask(ctx, "status")
	if errors.Is(err, supervise.ErrNotRunning) {
		return false, nil
	}
	return err == nil, err
}

// waitAnswer waits up to within for the supervisor to answer.
func (h taskHost) waitAnswer(ctx context.Context, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if up, _ := h.running(ctx); up {
			return true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
		case <-time.After(supervisePoll):
		}
	}
}

// stopSupervisor asks the supervisor to stop (it answers first, then
// stops the API and the database, then exits) and waits until its pipe
// is gone, superviseStopWait at most.
func (h taskHost) stopSupervisor(ctx context.Context) error {
	st, err := h.ask(ctx, "stop")
	if errors.Is(err, supervise.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	deadline := time.Now().Add(superviseStopWait)
	for {
		if _, err := h.ask(ctx, "status"); errors.Is(err, supervise.ErrNotRunning) {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return fmt.Errorf("the native server's supervisor (pid %d) still runs %s after its stop: see %s",
				st.PID, superviseStopWait, h.logFile("supervisor"))
		}
		select {
		case <-ctx.Done():
		case <-time.After(supervisePoll):
		}
	}
}

// register (re)creates the task from task.xml when Task Scheduler does
// not know it or task.xml changed under it (restart-pending's task
// marker).
func (h taskHost) register(ctx context.Context) error {
	if !h.n.pending()[markTask] && h.registered(ctx) {
		return nil
	}
	_, err := h.schtasks(ctx, "/Create", "/F", "/XML", filepath.Join(h.Dir(), taskFile), "/TN", h.n.TaskName())
	return err
}

// StartDB registers and enables the task, restarts a running supervisor
// when restart, and starts one when none runs: the task's /Run, else
// (no console logon: the task cannot run) the same command detached,
// with NoLogonNote.
func (h taskHost) StartDB(ctx context.Context, restart bool) error {
	if err := h.register(ctx); err != nil {
		return err
	}
	if _, err := h.schtasks(ctx, "/Change", "/TN", h.n.TaskName(), "/ENABLE"); err != nil {
		return err
	}
	if restart {
		if err := h.stopSupervisor(ctx); err != nil {
			return err
		}
	}
	up, err := h.running(ctx)
	if err != nil || up {
		return err
	}
	_, runErr := h.schtasks(ctx, "/Run", "/TN", h.n.TaskName())
	if runErr == nil && h.waitAnswer(ctx, taskRunWait) {
		return nil
	}
	if err := h.startDetached(ctx, false); err != nil {
		if runErr != nil {
			return errors.Join(fmt.Errorf("the task did not run: %w", runErr), err)
		}
		return err
	}
	h.n.note(NoLogonNote)
	return nil
}

// startDBAlone starts the database without the API and without arming
// anything: a running supervisor stops its API; else one is started
// detached with --no-api, the task left as it is.
func (h taskHost) startDBAlone(ctx context.Context) error {
	up, err := h.running(ctx)
	if err != nil {
		return err
	}
	if up {
		_, err := h.ask(ctx, "api-stop")
		return err
	}
	return h.startDetached(ctx, true)
}

// startDetached starts the supervisor under conhost --headless, detached
// and out of the caller's job, and waits for it to answer.
func (h taskHost) startDetached(ctx context.Context, noAPI bool) error {
	if err := startSupervisor(h.superviseArgs(noAPI)...); err != nil {
		return fmt.Errorf("starting the native server's supervisor: %w", err)
	}
	if !h.waitAnswer(ctx, taskRunWait) {
		return fmt.Errorf("the native server's supervisor did not answer within %s of its start: see %s", taskRunWait, h.logFile("supervisor"))
	}
	return nil
}

// StartAPI has the running supervisor start the API, after stopping it
// when restart (server.env changed: the supervisor reads it at each API
// start). An API not started yet, or started by the supervisor StartDB
// just restarted (restart-pending's supervisor marker), already reads
// the new server.env.
func (h taskHost) StartAPI(ctx context.Context, restart bool) error {
	st, err := h.ask(ctx, "status")
	if errors.Is(err, supervise.ErrNotRunning) {
		return fmt.Errorf("the native server's supervisor is not running: see %s", h.logFile("supervisor"))
	}
	if err != nil {
		return err
	}
	if restart && st.API != "stopped" && st.API != "starting" && !h.n.pending()[markSupervisor] {
		if _, err := h.ask(ctx, "api-stop"); err != nil {
			return err
		}
	}
	_, err = h.ask(ctx, "api-start")
	return err
}

// StopAPI has the supervisor stop the API; the database keeps running.
func (h taskHost) StopAPI(ctx context.Context) error {
	_, err := h.ask(ctx, "api-stop")
	if errors.Is(err, supervise.ErrNotRunning) {
		return nil
	}
	return err
}

// Stop disables the task first (its 5-minute trigger would start the
// supervisor again), then stops the supervisor: nothing runs, nothing
// starts at logon.
func (h taskHost) Stop(ctx context.Context) error {
	return errors.Join(h.onTask(ctx, "/Change", "/TN", h.n.TaskName(), "/DISABLE"), h.stopSupervisor(ctx))
}

// Remove stops, then unregisters the task.
func (h taskHost) Remove(ctx context.Context) error {
	if err := h.Stop(ctx); err != nil {
		return err
	}
	return h.onTask(ctx, "/Delete", "/F", "/TN", h.n.TaskName())
}

// onTask runs schtasks on the task, always: a failure counts only while
// the task is registered (schtasks' text, localized, is never read). A
// /Query that failed for another reason would otherwise skip a /DISABLE,
// and the 5-minute trigger would start the server again.
func (h taskHost) onTask(ctx context.Context, args ...string) error {
	_, err := h.schtasks(ctx, args...)
	if err != nil && !h.registered(ctx) {
		return nil
	}
	return err
}

// stopsWithoutFiles: Delete stops and unregisters even when supervise.json
// and task.xml are gone (a retried delete, files removed by hand): the
// task and the supervisor do not need them.
func (h taskHost) stopsWithoutFiles() bool { return true }

// taskStates maps the supervisor's states to systemd's ActiveState
// (Status and doctor read those).
var taskStates = map[string]string{"running": "active", "starting": "activating", "restarting": "activating/auto-restart",
	"refused": "failed", "stopped": "inactive"}

// States are the supervisor's, as it answers; both inactive when none
// runs; none when task.xml was never written.
func (h taskHost) States(ctx context.Context) (map[string]string, error) {
	if !pathExists(filepath.Join(h.Dir(), taskFile)) {
		return map[string]string{}, nil
	}
	st, err := h.ask(ctx, "status")
	if errors.Is(err, supervise.ErrNotRunning) {
		return map[string]string{"db": "inactive", "hindsight": "inactive"}, nil
	}
	if err != nil {
		return nil, err
	}
	state := func(s string) string {
		if v, ok := taskStates[s]; ok {
			return v
		}
		return s
	}
	return map[string]string{"db": state(st.DB), "hindsight": state(st.API)}, nil
}

func (h taskHost) logFile(what string) string {
	return h.n.join(h.n.LogDir(), h.n.project()+"-"+what+".log")
}

// Logs are the last lines of service's log (db or hindsight), its
// rotated .1 first.
func (h taskHost) Logs(_ context.Context, service string, lines int) (string, error) {
	what := map[string]string{"db": "db", "hindsight": "api"}[service]
	if what == "" {
		return "", fmt.Errorf("no service %q on the native server: db or hindsight", service)
	}
	var all []byte
	for _, p := range []string{h.logFile(what) + ".1", h.logFile(what)} {
		raw, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		all = append(all, raw...)
	}
	return tailLines(string(all), lines), nil
}

// tailLines is text's last n lines.
func tailLines(text string, n int) string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" || n <= 0 {
		return ""
	}
	ls := strings.Split(text, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n") + "\n"
}

// DeployedVenv is supervise.json's venv.
func (h taskHost) DeployedVenv(files map[string][]byte) (string, error) {
	var cfg supervise.Config
	if err := json.Unmarshal(files[superviseFile], &cfg); err != nil {
		return "", err
	}
	if cfg.Venv == "" {
		return "", errors.New("it names no venv")
	}
	return cfg.Venv, nil
}

// Owns: pid is in the supervisor's Job Object.
func (h taskHost) Owns(pid int) bool {
	in, err := supervise.InJob(h.n.project(), pid)
	return err == nil && in
}

// Engine is Task Scheduler: it must know the server's task.
func (h taskHost) Engine(ctx context.Context) (Engine, error) {
	if !h.registered(ctx) {
		return Engine{}, fmt.Errorf("the scheduled task %s is not registered: run backsight server start", h.n.TaskName())
	}
	return Engine{Version: "Task Scheduler"}, nil
}

// note tells the user something a command's result does not say.
func (n Native) note(s string) {
	if n.Note != nil {
		n.Note(s)
	}
}
