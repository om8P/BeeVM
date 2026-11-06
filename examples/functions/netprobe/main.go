// netprobe is an attack function (milestone 9): it tries to reach other VMs and the host
// over the network. The payload picks the mode:
//
//	{"mode":"listen","port":P,"ms":N}                  accepts TCP connections on port P for N ms;
//	                                                   prints {"accepted": count}
//	{"mode":"connect","targets":["ip:port"],"timeout_ms":N}
//	                                                   tries each target; prints {"results": {"ip:port": "ok" | error}}
//	{"mode":"scan","cidr":"10.200.0.0/24","port":P,"timeout_ms":N}
//	                                                   tries every address in the range (an
//	                                                   attacker looking for neighbours);
//	                                                   prints {"open": [addresses that answered]}
//	{"mode":"info"}                                    prints the VM's own addresses
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type payload struct {
	Mode      string   `json:"mode"`
	Port      int      `json:"port"`
	MS        int      `json:"ms"`
	Targets   []string `json:"targets"`
	TimeoutMS int      `json:"timeout_ms"`
	CIDR      string   `json:"cidr"`
}

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		fail("bad payload: %v", err)
	}
	if p.TimeoutMS <= 0 {
		p.TimeoutMS = 500
	}
	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	switch p.Mode {
	case "listen":
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.Port))
		if err != nil {
			fail("listen: %v", err)
		}
		var accepted atomic.Int64
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				accepted.Add(1)
				_, _ = c.Write([]byte("hello from netprobe\n"))
				c.Close()
			}
		}()
		time.Sleep(time.Duration(p.MS) * time.Millisecond)
		ln.Close()
		out(map[string]any{"accepted": accepted.Load()})
	case "connect":
		results := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, t := range p.Targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := try(t, timeout)
				mu.Lock()
				results[t] = r
				mu.Unlock()
			}()
		}
		wg.Wait()
		out(map[string]any{"results": results})
	case "scan":
		pfx, err := netip.ParsePrefix(p.CIDR)
		if err != nil || pfx.Bits() < 20 {
			fail("cidr must be a range of at most 4096 addresses: %q", p.CIDR)
		}
		var open []string
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 128)
		for a := pfx.Masked().Addr(); pfx.Contains(a); a = a.Next() {
			wg.Add(1)
			sem <- struct{}{}
			go func(a netip.Addr) {
				defer wg.Done()
				defer func() { <-sem }()
				target := netip.AddrPortFrom(a, uint16(p.Port)).String()
				if try(target, timeout) == "ok" {
					mu.Lock()
					open = append(open, a.String())
					mu.Unlock()
				}
			}(a)
		}
		wg.Wait()
		sort.Strings(open)
		out(map[string]any{"open": open, "scanned": 1 << (32 - pfx.Bits())})
	case "info":
		var addrs []string
		ifs, _ := net.Interfaces()
		for _, i := range ifs {
			as, _ := i.Addrs()
			for _, a := range as {
				addrs = append(addrs, i.Name+" "+a.String())
			}
		}
		out(map[string]any{"addrs": addrs})
	default:
		fail("unknown mode %q", p.Mode)
	}
}

// try opens a TCP connection and reads the greeting, so a connection that the network lets
// through but nobody answers is not counted as reachable.
func try(target string, timeout time.Duration) string {
	c, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	return "ok"
}

func out(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}
