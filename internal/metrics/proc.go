package metrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Memory of one process, from /proc/<pid>/smaps_rollup, in bytes.
//
// RSS counts every page the process has in RAM, including pages it shares with others. PSS
// (proportional set size) divides each shared page by the number of processes sharing it.
// Restored VMs map the same snapshot memory file, so pages they only read are shared: PSS is
// the honest "memory per VM" and adds up to the real total, RSS would count them N times.
type procMemory struct {
	RSS uint64
	PSS uint64
}

// readSmapsRollup reads the Rss and Pss lines of an smaps_rollup file.
func readSmapsRollup(path string) (procMemory, error) {
	f, err := os.Open(path)
	if err != nil {
		return procMemory{}, err
	}
	defer f.Close()
	var m procMemory
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, kb, ok := kbLine(sc.Text())
		if !ok {
			continue
		}
		switch name {
		case "Rss":
			m.RSS = kb * 1024
		case "Pss":
			m.PSS = kb * 1024
		}
	}
	return m, sc.Err()
}

// readMeminfo reads MemTotal and MemAvailable from /proc/meminfo, in bytes.
func readMeminfo(path string) (total, available uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, kb, ok := kbLine(sc.Text())
		if !ok {
			continue
		}
		switch name {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			available = kb * 1024
		}
	}
	return total, available, sc.Err()
}

// kbLine parses "Name:   1234 kB".
func kbLine(line string) (string, uint64, bool) {
	name, rest, ok := strings.Cut(line, ":")
	if !ok {
		return "", 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) != 2 || fields[1] != "kB" {
		return "", 0, false
	}
	v, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name, v, true
}
