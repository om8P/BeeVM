package isolation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/om8P/BeeVM/internal/vmm"
)

// ownerOf returns who owns path (BeeVM's folder belongs to the user who ran make setup).
func ownerOf(path string) (vmm.Owner, error) {
	st, err := os.Stat(path)
	if err != nil {
		return vmm.Owner{}, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return vmm.Owner{}, errors.New("cannot read file owner")
	}
	return vmm.Owner{UID: int(sys.Uid), GID: int(sys.Gid)}, nil
}

// ChownTree gives every file and folder below root that is not owner's to owner, skipping the
// folders in skip. The jailed server runs as root, so the files it creates in BeeVM's folder
// (database, function code, snapshots) would otherwise belong to root, and the next run
// without sudo could not change or delete them. It returns how many it changed.
func ChownTree(root string, owner vmm.Owner, skip ...string) (int, error) {
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while we walked
			}
			return err
		}
		for _, s := range skip {
			if path == s {
				return filepath.SkipDir
			}
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		sys, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (int(sys.Uid) == owner.UID && int(sys.Gid) == owner.GID) {
			return nil
		}
		if err := os.Lchown(path, owner.UID, owner.GID); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		n++
		return nil
	})
	return n, err
}
