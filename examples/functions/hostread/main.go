// hostread is an attack function (milestone 9): it looks for the host's files from inside the
// VM. The test writes a secret "canary" string into files on the host; this function searches
// every file it can read for it, and tries a few classic tricks:
//
//	{"canary":"..."}
//
// Prints {"found": [paths containing the canary], "scanned": files read, "uid": ..., "checks": {...}}.
// Inside a MicroVM the guest has its own kernel and its own disk: the host's files are not
// there to be found, whatever the function does.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var p struct {
		Canary string `json:"canary"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Canary == "" {
		fmt.Fprintln(os.Stderr, "canary is required")
		os.Exit(2)
	}
	needle := []byte(p.Canary)
	var found []string
	scanned := 0
	_ = filepath.WalkDir("/", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (path == "/proc" || path == "/sys" || path == "/dev") {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := readSome(path, 8<<20)
		if err != nil {
			return nil
		}
		scanned++
		if bytes.Contains(b, needle) {
			found = append(found, path)
		}
		return nil
	})

	checks := map[string]string{}
	check := func(name string, err error) {
		if err == nil {
			checks[name] = "allowed"
		} else {
			checks[name] = "denied: " + err.Error()
		}
	}
	// The host's usual secrets (here: only the guest's own copies, if any).
	_, err := os.ReadFile("/etc/shadow")
	check("read /etc/shadow", err)
	// The disk itself: write access would let it change the shared image.
	f, err := os.OpenFile("/dev/vda", os.O_RDWR, 0)
	if err == nil {
		f.Close()
	}
	check("open /dev/vda for writing", err)
	_, err = os.ReadFile("/proc/1/environ")
	check("read init's environment", err)
	err = os.WriteFile("/run/beevm/function", []byte("x"), 0o755)
	check("replace the agent's function file", err)
	mounts, _ := os.ReadFile("/proc/mounts")

	out, _ := json.Marshal(map[string]any{
		"found": found, "scanned": scanned, "uid": os.Getuid(), "checks": checks,
		"mounts": strings.Split(strings.TrimSpace(string(mounts)), "\n"),
	})
	fmt.Println(string(out))
}

func readSome(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}
