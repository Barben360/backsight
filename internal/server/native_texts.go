package server

// How commands name the native server to the user: its two systemd user
// units on Linux, its scheduled task and the supervisor's logs on Windows
// (decision 0080). Linux's words are the ones printed since 0078.

// ServerName names what runs the API, in a sentence.
func (n Native) ServerName() string {
	if n.goos() == "windows" {
		return "the native server's API (scheduled task " + n.TaskName() + ")"
	}
	return "the server unit " + n.ServerUnit()
}

// LogsHint says where the server's logs are.
func (n Native) LogsHint() string {
	if n.goos() == "windows" {
		return "the logs " + n.logFile("supervisor") + ", " + n.logFile("db") + " and " + n.logFile("api")
	}
	return "journalctl --user -u " + n.ServerUnit()
}

// Services names what a start starts: the units, or the task.
func (n Native) Services() string {
	if n.goos() == "windows" {
		return "the scheduled task " + n.TaskName()
	}
	return n.DBUnit() + " and " + n.ServerUnit()
}

// Started is server install's start step: what it started.
func (n Native) Started() string {
	if n.goos() == "windows" {
		return n.Services()
	}
	return "units " + n.Services()
}

// ServiceFiles are the files Render writes beside server.env: the units,
// or supervise.json and task.xml.
func (n Native) ServiceFiles() []string {
	if n.goos() == "windows" {
		return []string{n.SuperviseFile(), n.join(n.Root(), taskFile)}
	}
	return []string{n.join(n.UnitDir, n.DBUnit()), n.join(n.UnitDir, n.ServerUnit())}
}

func (n Native) logFile(what string) string { return n.join(n.LogDir(), n.project()+"-"+what+".log") }

// StopText is server stop's line on Windows, planned or done; "" on
// Linux, whose line is the one every runtime prints.
func (n Native) StopText(plan bool) string {
	if n.goos() != "windows" {
		return ""
	}
	if plan {
		return "would disable the scheduled task " + n.TaskName() + " and stop the native server (data kept)\n"
	}
	return "stopped; the scheduled task " + n.TaskName() + " stays disabled (the server does not start at logon) until backsight server start\n"
}
