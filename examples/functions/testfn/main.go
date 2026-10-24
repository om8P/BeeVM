// testfn is a test function for the guest agent. The payload picks its behaviour:
//
//	{"mode":"echo", ...}                  prints {"echo": <payload>}
//	{"mode":"fail"}                       writes to stderr and exits with code 3
//	{"mode":"sleep","ms":N,"pidfile":P}   starts a background child (its pid goes to P), then sleeps N ms
//	{"mode":"garbage"}                    prints text that is not JSON
//	{"mode":"big","bytes":N}              prints a JSON string of about N bytes
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var p struct {
		Mode    string `json:"mode"`
		MS      int    `json:"ms"`
		Bytes   int    `json:"bytes"`
		PidFile string `json:"pidfile"`
	}
	_ = json.Unmarshal(raw, &p)

	switch p.Mode {
	case "echo", "":
		out, _ := json.Marshal(map[string]json.RawMessage{"echo": json.RawMessage(raw)})
		fmt.Println(string(out))
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: this function always fails")
		os.Exit(3)
	case "sleep":
		if p.PidFile != "" {
			// The child shares our output pipes and our process group. It must die with us.
			child := exec.Command("sleep", "60")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err == nil {
				_ = os.WriteFile(p.PidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
			}
		}
		time.Sleep(time.Duration(p.MS) * time.Millisecond)
		fmt.Println(`{"slept":true}`)
	case "garbage":
		fmt.Println("this is not json")
	case "big":
		fmt.Printf("%q\n", strings.Repeat("x", p.Bytes))
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", p.Mode)
		os.Exit(2)
	}
}
