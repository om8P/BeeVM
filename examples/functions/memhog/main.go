// memhog is an attack function (milestone 9): it allocates memory and writes to every page
// (so the memory is really used, not just promised) until it has {"mib": N} (default 1024).
// Progress goes to stderr every 16 MiB. If it survives, it prints {"allocated_mib": N}.
//
// In a 128 MiB VM it cannot get 1 GiB: the guest kernel's OOM killer ends it. The host is not
// affected: the guest can never use more than its own memory (plus the cgroup's memory.max
// for Firecracker itself).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var p struct {
		MiB int `json:"mib"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.MiB <= 0 {
		p.MiB = 1024
	}
	const chunk = 16 << 20
	var keep [][]byte
	for got := 0; got < p.MiB; got += 16 {
		b := make([]byte, chunk)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1 // touch every page
		}
		keep = append(keep, b)
		fmt.Fprintf(os.Stderr, "allocated %d MiB\n", got+16)
	}
	out, _ := json.Marshal(map[string]int{"allocated_mib": len(keep) * 16})
	fmt.Println(string(out))
}
