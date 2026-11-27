// Package control lets runr commands in other terminals talk to a running
// session: ask for its processes and their logs, start, stop and restart
// projects, and stop the session. Each session listens on a
// Unix domain socket (Windows has them since Windows 10 1803) named after
// runr's pid in runr's state directory, which only the user can open.
//
// A client sends one JSON request per connection; the server answers with
// one JSON reply per line and closes the connection, except while following
// logs or stopping the session.
package control

import (
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Skaanning/runr/internal/state"
)

// Version is the protocol version. A session answers requests of another
// version with an error.
const Version = 1

// Requests.
const (
	OpStatus  = "status"  // the session and its processes
	OpLogs    = "logs"    // buffered log lines, then new ones if Follow is set
	OpStart   = "start"   // start projects and groups, with their dependencies
	OpStop    = "stop"    // stop projects
	OpRestart = "restart" // restart projects
	OpDown    = "down"    // stop the session, as q does; the reply ends when it has
)

// maxPath is the longest socket path that works everywhere: sun_path holds
// 104 bytes on macOS (108 on Linux), including the final NUL.
const maxPath = 103

// ErrPathTooLong is returned by Listen when the socket's path doesn't fit
// in a socket address.
var ErrPathTooLong = errors.New("control socket path is too long")

// SocketPath is where the session of runr's pid listens in dir, or
// ErrPathTooLong.
func SocketPath(dir string, pid int) (string, error) {
	path := filepath.Join(dir, strconv.Itoa(pid)+".sock")
	if len(path) > maxPath {
		return "", ErrPathTooLong
	}
	return path, nil
}

// Dir is where sessions put their sockets: control in runr's state
// directory.
func Dir() (string, error) {
	d, err := state.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "control"), nil
}

// Request is what a client asks a session.
type Request struct {
	V      int      `json:"v"`
	Op     string   `json:"op"`
	Names  []string `json:"names,omitempty"`  // projects; none: all
	Tail   int      `json:"tail,omitempty"`   // logs: the last this many lines; 0: all buffered
	Errors bool     `json:"errors,omitempty"` // logs: only error lines
	Follow bool     `json:"follow,omitempty"` // logs: keep sending new lines
	Force  bool     `json:"force,omitempty"`  // down: kill instead of stopping gracefully

	// Dirs are directories to run projects in instead of their configured
	// ones, by project (start, restart): set before they start, and kept.
	Dirs map[string]string `json:"dirs,omitempty"`
}

// Reply is one line of a session's answer: an error, or one of the others.
type Reply struct {
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
	Line   *Line   `json:"line,omitempty"`
}

// Status describes a session.
type Status struct {
	Pid       int       `json:"pid"`            // of runr
	Started   time.Time `json:"started"`        // when the session began
	Titles    []string  `json:"titles"`         // the groups and projects it was started with
	Host      bool      `json:"host,omitempty"` // runs in the background, shown by attached views
	Attached  int       `json:"attached"`       // views attached now (runr attach)
	Processes []Process `json:"processes"`
}

// Has reports whether the session has a project of that name.
func (s Status) Has(name string) bool {
	for _, p := range s.Processes {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Process describes a project of a session (see supervisor.ProcessInfo).
type Process struct {
	Name      string    `json:"name"`
	Label     string    `json:"label"` // the display name, or the name
	State     string    `json:"state"`
	Pid       int       `json:"pid,omitempty"`       // while alive
	StartedAt time.Time `json:"started_at,omitzero"` // of the most recent start
	Exit      string    `json:"exit,omitempty"`      // how it ended, e.g. "exit code 1"
	Restarts  int       `json:"restarts"`            // starts after the first
	Errors    int       `json:"errors"`              // error lines of the most recent run
	Args      string    `json:"args,omitempty"`      // extra arguments
	Dir       string    `json:"dir,omitempty"`       // where it runs, when not its configured dir
	Ports     []int     `json:"ports,omitempty"`     // listening TCP ports, if they can be read
}

// Line is a log line (see supervisor.LogLine). Text is as the program
// printed it, colors included.
type Line struct {
	Seq     uint64    `json:"seq"`
	Project string    `json:"project"`
	Time    time.Time `json:"time"`
	Runr    bool      `json:"runr,omitempty"` // written by runr itself
	Error   bool      `json:"error,omitempty"`
	Text    string    `json:"text"`
}
