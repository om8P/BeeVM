// cpuhog is an attack function (milestone 9): it keeps {"threads": T} CPUs (default 1) busy
// for {"ms": N} (default 2000) and prints {"iterations": ...}.
//
// A VM can never use more host CPU than it has vCPUs; the cgroup's cpu.max can limit it
// further (beevmd serve --cpu-limit), so a busy VM cannot slow its neighbours down.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var p struct {
		Threads int `json:"threads"`
		MS      int `json:"ms"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Threads <= 0 {
		p.Threads = 1
	}
	if p.MS <= 0 {
		p.MS = 2000
	}
	runtime.GOMAXPROCS(p.Threads)
	end := time.Now().Add(time.Duration(p.MS) * time.Millisecond)
	var total atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < p.Threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := int64(0)
			x := uint64(1)
			for time.Now().Before(end) {
				for j := 0; j < 10000; j++ {
					x = x*6364136223846793005 + 1442695040888963407
				}
				n++
			}
			total.Add(n + int64(x&1))
		}()
	}
	wg.Wait()
	out, _ := json.Marshal(map[string]any{"iterations": total.Load(), "threads": p.Threads, "ms": p.MS})
	fmt.Fprintln(os.Stdout, string(out))
}
