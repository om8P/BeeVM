//go:build linux

package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// handleResume is called by the host right after a VM was restored from a snapshot. Every VM
// restored from the same snapshot starts as an exact copy of one moment in the past, so:
//   - its clock is behind (it stood still while the snapshot sat on disk), and
//   - its random-number generator is in the same state as in every other copy.
//
// The host sends the current time and fresh random bytes; we set the clock, mix the bytes
// into the kernel's entropy pool and force the kernel to reseed its generator.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	var req agentproto.ResumeRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad resume request: %v", err)
		return
	}
	if req.UnixNano <= 0 || len(req.Entropy) < 16 {
		writeError(w, http.StatusBadRequest, "resume needs unix_nano and at least 16 bytes of entropy")
		return
	}
	before := time.Now()
	host := time.Unix(0, req.UnixNano)
	if err := s.setClock(host); err != nil {
		writeError(w, http.StatusInternalServerError, "set clock: %v", err)
		return
	}
	if err := s.addEntropy(req.Entropy); err != nil {
		writeError(w, http.StatusInternalServerError, "add entropy: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, agentproto.ResumeResponse{ClockAdjustedMS: host.Sub(before).Milliseconds()})
}

// setSystemClock sets the wall clock (needs root; the agent runs as root in the guest).
func setSystemClock(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}

// ioctl numbers from <linux/random.h>.
const (
	rndAddEntropy  = 0x40085203 // RNDADDENTROPY: mix bytes in and credit them as entropy
	rndReseedCRNG  = 0x5207     // RNDRESEEDCRNG: rebuild the generator's key from the pool now
	bitsPerByte    = 8
	maxEntropyRead = 512
)

// addKernelEntropy mixes b into the kernel's entropy pool and forces a reseed, so the next
// random numbers differ from those of every other copy of this snapshot.
func addKernelEntropy(b []byte) error {
	if len(b) > maxEntropyRead {
		b = b[:maxEntropyRead]
	}
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	info := make([]byte, 8+len(b))
	binary.NativeEndian.PutUint32(info[0:], uint32(len(b)*bitsPerByte))
	binary.NativeEndian.PutUint32(info[4:], uint32(len(b)))
	copy(info[8:], b)
	if err := ioctl(f, rndAddEntropy, uintptr(unsafe.Pointer(&info[0]))); err != nil {
		return fmt.Errorf("RNDADDENTROPY: %w", err)
	}
	if err := ioctl(f, rndReseedCRNG, 0); err != nil {
		return fmt.Errorf("RNDRESEEDCRNG: %w", err)
	}
	return nil
}

func ioctl(f *os.File, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, arg); errno != 0 {
		return errno
	}
	return nil
}
