package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/pkg"
)

// localCode is a function given on the command line, ready to upload into a VM.
type localCode struct {
	Path    string // a binary, or a zip package (a temp file when we zipped it)
	Runtime string
	Handler string // python/node: the handler to load
	cleanup func()
}

// Close removes the temporary zip, if one was made.
func (c localCode) Close() {
	if c.cleanup != nil {
		c.cleanup()
	}
}

// openCode prepares path (a binary, a folder, a .py/.js file or a .zip) for the runtime given
// (or detected when empty). Folders and single files are zipped into a temporary package.
func openCode(path, runtime, handler string) (localCode, error) {
	if runtime == "" {
		rt, err := pkg.Detect(path)
		if err != nil {
			return localCode{}, err
		}
		runtime = rt
	}
	if !pkg.ValidRuntime(runtime) {
		return localCode{}, fmt.Errorf("runtime must be binary, python or node")
	}
	if !pkg.IsPackaged(runtime) {
		return localCode{Path: path, Runtime: agentproto.RuntimeBinary}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return localCode{}, err
	}
	if !info.IsDir() && handler == "" && !strings.EqualFold(filepath.Ext(path), ".zip") {
		handler = strings.TrimSuffix(info.Name(), filepath.Ext(info.Name())) + ".handler"
	}
	if handler == "" {
		handler = pkg.DefaultHandler(runtime)
	}
	c := localCode{Path: path, Runtime: runtime, Handler: handler}
	if info.IsDir() || !strings.EqualFold(filepath.Ext(path), ".zip") {
		f, err := os.CreateTemp("", "beevm-package-*.zip")
		if err != nil {
			return localCode{}, err
		}
		if err := pkg.Zip(path, f); err != nil {
			f.Close()
			os.Remove(f.Name())
			return localCode{}, err
		}
		f.Close()
		c.Path = f.Name()
		c.cleanup = func() { os.Remove(f.Name()) }
	}
	if err := pkg.Check(c.Path, runtime, handler); err != nil {
		c.Close()
		return localCode{}, err
	}
	return c, nil
}

// rootfsFor is the root filesystem image of a runtime on this machine.
func rootfsFor(home, runtime string) string {
	if p, ok := runtimeImages(home)[runtime]; ok {
		return p
	}
	return filepath.Join(home, "images", "rootfs.ext4")
}
