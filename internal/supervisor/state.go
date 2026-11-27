package supervisor

import (
	"time"

	"github.com/Skaanning/runr/internal/proc"
)

// State is the lifecycle state of a managed project.
type State int

const (
	Pending  State = iota // known to the session but not started yet
	Waiting               // waiting for dependencies to become ready
	Starting              // spawned, waiting for its own readiness check
	Running               // spawned and ready
	Stopping              // asked to stop, waiting for it to exit
	Stopped               // stopped by the user
	Exited                // exited on its own with code 0
	Crashed               // exited on its own with a failure, or failed to start
	Blocked               // a dependency failed before becoming ready
)

var stateNames = [...]string{
	Pending:  "pending",
	Waiting:  "waiting",
	Starting: "starting",
	Running:  "running",
	Stopping: "stopping",
	Stopped:  "stopped",
	Exited:   "exited",
	Crashed:  "crashed",
	Blocked:  "blocked",
}

func (s State) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "unknown"
}

// Alive reports whether a process exists in this state.
func (s State) Alive() bool {
	return s == Starting || s == Running || s == Stopping
}

// ProcessInfo is a snapshot of a managed project.
type ProcessInfo struct {
	Name      string
	State     State
	Pid       int             // valid while Alive
	StartedAt time.Time       // time of the most recent start
	Exit      proc.ExitStatus // how the most recent run ended; valid in Stopped, Exited, Crashed
	Restarts  int             // number of times the project was started again in this session
	Args      string          // extra arguments of the most recent start (see Supervisor.SetArgs)
	Dir       string          // the directory of the most recent start if not the configured one (see Supervisor.SetDir)
	Errors    int             // error lines of the most recent run (see LogLine.Error)
}
