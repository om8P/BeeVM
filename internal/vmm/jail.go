package vmm

// Running Firecracker through its jailer (milestone 9).
//
// The jailer is a small helper that ships with Firecracker. Started as root, it
//   - creates a folder <ChrootBase>/firecracker/<ID>/root and makes it the process's "/"
//     (chroot, really pivot_root in its own mount namespace), with only /dev/kvm and
//     /dev/net/tun inside,
//   - puts the process into its own cgroup (<ParentCgroup>/<ID>) with the given limits,
//   - joins the given network namespace,
//   - switches to an unprivileged UID/GID, and then
//   - execs firecracker in place (same PID), which installs its seccomp filter.
//
// So a guest that escapes into the Firecracker process finds itself as an unknown user, in an
// almost empty folder, unable to make most system calls, inside resource limits.
//
// Everything Firecracker needs must be inside the jail. We hard-link the kernel, the root
// filesystem and snapshot files in (a hard link is a second name for the same file: no copy,
// and the page cache is shared). That needs the jail on the same filesystem as the images.
// Inside the jail the files always have the same names (/vmlinux, /rootfs.ext4), so a
// snapshot taken in one jail can be restored in any other.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Jail says how to run firecracker through the jailer. The isolation package fills it in.
type Jail struct {
	JailerBin    string   // path to the jailer binary (same version as firecracker)
	ChrootBase   string   // jails are created below it; same filesystem as images and snapshots
	UID, GID     int      // firecracker runs as this user; unique per VM
	NetNS        string   // network namespace to join (e.g. /run/netns/beevm-3); "" = the host's
	ParentCgroup string   // cgroup v2 parent (relative to /sys/fs/cgroup); "" = no cgroup
	Cgroups      []string // "file=value" limits for the VM's own cgroup, e.g. "pids.max=32"
	FileOwner    *Owner   // gets the snapshot files the VM writes; nil = they stay root's
}

// Owner is a file owner.
type Owner struct{ UID, GID int }

// NetIface is the guest's network card: a TAP device on the host side (inside the VM's
// network namespace) and a fixed MAC address. Every VM uses the same names and addresses, so
// a snapshot's network settings fit every restored copy.
type NetIface struct {
	HostDev  string // TAP device name, e.g. "tap0"
	GuestMAC string // e.g. "06:00:AC:10:00:02"
	Addr     string // where other VMs (of the same tenant) reach this VM; informational
}

// Names inside the jail.
const (
	jailKernel    = "vmlinux"
	jailRootfs    = "rootfs.ext4"
	jailSnapState = "snap.vmstate"
	jailSnapMem   = "snap.mem"
	jailNewState  = "snapshot.vmstate" // written by CreateSnapshot
	jailNewMem    = "snapshot.mem"
	jailAPISock   = "firecracker.sock"
)

func (j *Jail) validate(c Config) error {
	var errs []error
	if j.JailerBin == "" {
		errs = append(errs, errors.New("Jail.JailerBin is required"))
	} else if _, err := os.Stat(j.JailerBin); err != nil {
		errs = append(errs, fmt.Errorf("Jail.JailerBin: %w", err))
	}
	if j.ChrootBase == "" {
		errs = append(errs, errors.New("Jail.ChrootBase is required"))
	}
	if j.UID <= 0 || j.GID <= 0 {
		errs = append(errs, fmt.Errorf("Jail.UID/GID must be > 0 (not root), got %d/%d", j.UID, j.GID))
	}
	if !c.SharedRootfs {
		errs = append(errs, errors.New("the jailer needs SharedRootfs (the image is linked into the jail)"))
	}
	return errors.Join(errs...)
}

// JailRoot is the jail's root folder as the host sees it (Firecracker's "/"); "" without jail.
func (c Config) JailRoot() string {
	if c.Jail == nil {
		return ""
	}
	return filepath.Join(c.jailDir(), "root")
}

// jailDir is the folder the jailer creates for this VM: <ChrootBase>/<exec name>/<ID>.
func (c Config) jailDir() string {
	return filepath.Join(c.Jail.ChrootBase, filepath.Base(c.FirecrackerBin), c.ID)
}

// jailerArgs is the jailer's command line. Arguments after "--" go to firecracker.
// We never pass --daemonize or --new-pid-ns: then the jailer execs firecracker in place, and
// the process we started is firecracker itself (Kill and Wait keep working). We never pass
// --no-seccomp either: Firecracker's own seccomp filter stays on.
func jailerArgs(c Config, fcArgs ...string) []string {
	j := c.Jail
	args := []string{
		"--id", c.ID,
		"--exec-file", c.FirecrackerBin,
		"--uid", fmt.Sprint(j.UID),
		"--gid", fmt.Sprint(j.GID),
		"--chroot-base-dir", j.ChrootBase,
	}
	if j.NetNS != "" {
		args = append(args, "--netns", j.NetNS)
	}
	if j.ParentCgroup != "" {
		args = append(args, "--cgroup-version", "2", "--parent-cgroup", j.ParentCgroup)
		for _, cg := range j.Cgroups {
			args = append(args, "--cgroup", cg)
		}
	}
	args = append(args, "--")
	return append(args, fcArgs...)
}

// jailerCommand builds the command that starts firecracker in its jail. Firecracker's API
// socket is /firecracker.sock inside the jail (SocketPath on the host).
func jailerCommand(ctx context.Context, c Config) *exec.Cmd {
	return exec.CommandContext(ctx, c.Jail.JailerBin, jailerArgs(c, "--api-sock", "/"+jailAPISock)...)
}

// prepareJail creates the jail's root folder, owned by the VM's user (Firecracker creates its
// sockets there), and hard-links files into it. links maps a name inside the jail to a host
// file. The files keep their owner and stay read-only for the VM's user.
func prepareJail(c Config, links map[string]string) error {
	root := c.JailRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create jail: %w", err)
	}
	if err := os.Chown(root, c.Jail.UID, c.Jail.GID); err != nil {
		return fmt.Errorf("create jail: %w", err)
	}
	for name, host := range links {
		if err := worldReadable(host); err != nil {
			return err
		}
		if err := os.Link(host, filepath.Join(root, name)); err != nil {
			return fmt.Errorf("link %s into the jail (the jail must be on the same filesystem): %w", filepath.Base(host), err)
		}
	}
	return nil
}

// worldReadable makes sure the VM's user (a stranger to the file) can read it. Images and
// snapshots are not secret; the user's home folder still keeps other people out (a hard link
// skips folder permissions, but only root can make one into the jail).
func worldReadable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o004 != 0 {
		return nil
	}
	return os.Chmod(path, st.Mode().Perm()|0o044)
}

// removeJail deletes the VM's jail folder (only links and sockets are in it).
func removeJail(c Config) error {
	if c.Jail == nil {
		return nil
	}
	return os.RemoveAll(c.jailDir())
}

// fcPath turns a host path inside Firecracker's folder into the path Firecracker sees.
func (c Config) fcPath(host string) string {
	if c.Jail == nil {
		return host
	}
	rel, err := filepath.Rel(c.JailRoot(), host)
	if err != nil || strings.HasPrefix(rel, "..") {
		return host
	}
	return "/" + rel
}

// moveOut moves a file Firecracker wrote inside the jail to dst, gives it to owner and makes
// it readable for everyone (other VMs' users must read snapshot files through hard links).
func moveOut(src, dst string, owner *Owner) error {
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	if owner != nil {
		if err := os.Chown(dst, owner.UID, owner.GID); err != nil {
			return err
		}
	}
	return os.Chmod(dst, 0o644)
}
