// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"github.com/sagernet/sing/service/pause"

	"golang.org/x/sys/unix"
)

type boxEngine struct {
	box       *box.Box
	ctx       context.Context
	pause     pause.Manager
	log       *os.File
	logPath   string
	closeOnce sync.Once
	closeErr  error
}

func newBoxEngine(ctx context.Context, platform PlatformInterface, options option.Options) (mobileEngine, error) {
	ctx = pause.WithDefaultManager(ctx)
	// Standalone macOS keeps native utun networking. Its optional monitor-only
	// adapter is already in ctx; the host applies the effective Go DNS plan.
	// A full mobile wrapper with a nil host would override native networking.
	if platform != nil {
		ctx = service.ContextWith[adapter.PlatformInterface](ctx, &platformInterfaceWrapper{iif: platform})
	}
	path, err := privateLogPath(ctx, options.Log)
	if err != nil {
		return nil, err
	}
	engine := &boxEngine{ctx: ctx, pause: service.FromContext[pause.Manager](ctx), logPath: path}
	// Preparing validates constructors without runtime storage side effects.
	// Activation reports log access failures as startup failures, not config errors.
	if prepared, _ := ctx.Value(standalonePreparationKey{}).(bool); !prepared {
		if err = engine.openLog(); err != nil {
			_ = engine.Close()
			return nil, err
		}
	}
	// Upstream protocol logs may contain credential-bearing URLs. An explicit
	// log target receives only fixed lifecycle events, never those diagnostics.
	options.Log = &option.LogOptions{Disabled: true}
	// The pinned mobile constructor always enabled cache via PlatformLogWriter.
	// Preserve that default without restoring raw logs or the RPC layer.
	var experimental option.ExperimentalOptions
	if options.Experimental != nil {
		experimental = *options.Experimental
	}
	var cache option.CacheFileOptions
	if experimental.CacheFile != nil {
		cache = *experimental.CacheFile
	}
	cache.Enabled = true
	experimental.CacheFile = &cache
	options.Experimental = &experimental
	engine.box, err = box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		engine.event("create failed")
		_ = engine.Close()
		return nil, errors.New("could not create mobile service")
	}
	return engine, nil
}

func privateLogPath(ctx context.Context, options *option.LogOptions) (string, error) {
	if options == nil || options.Disabled {
		return "", nil
	}
	if options.Level != "" {
		if _, err := log.ParseLevel(options.Level); err != nil {
			return "", errors.New("invalid mobile log level")
		}
	}
	output := options.Output
	if output == "stdout" || output == "stderr" {
		return "", errors.New("mobile logging requires a private file")
	}
	if output == "" {
		output = "mobile.log"
	}
	// Hosts supply their private Working directory through the file manager.
	// Never fall back to the process working directory for privileged logging.
	base := filemanager.BasePath(ctx, ".")
	if !filepath.IsAbs(base) {
		return "", errors.New("mobile logging requires a private file")
	}
	if filepath.IsAbs(output) {
		var err error
		output, err = filepath.Rel(base, output)
		if err != nil {
			return "", errors.New("mobile logging requires a private file")
		}
	}
	if !filepath.IsLocal(output) || filepath.Clean(output) == "." {
		return "", errors.New("mobile logging requires a private file")
	}
	return filepath.Clean(output), nil
}

func (e *boxEngine) event(event string) {
	if e.log != nil {
		_, _ = e.log.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + event + "\n")
	}
}

func (e *boxEngine) openLog() error {
	if e.logPath == "" || e.log != nil {
		return nil
	}
	failure := errors.New("could not open private mobile log")
	// Anchor creation and parent resolution to the existing, host-owned runtime
	// directory. Root prevents symlink/rename races from escaping that tree.
	root, err := os.OpenRoot(filemanager.BasePath(e.ctx, "."))
	if err != nil {
		return failure
	}
	defer root.Close()
	if err = root.MkdirAll(filepath.Dir(e.logPath), 0o700); err != nil {
		return failure
	}
	parent, err := root.OpenFile(filepath.Dir(e.logPath), os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return failure
	}
	defer parent.Close()
	// Root.OpenFile follows in-tree symlinks, so open the leaf relative to the
	// pinned parent descriptor to retain O_NOFOLLOW. Nonblocking avoids hanging
	// on a FIFO before its type can be checked; never acquire a controlling TTY.
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(e.logPath), unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return failure
	}
	var stat unix.Stat_t
	// Check the opened object, not its path, before changing permissions or
	// writing. A hard link could otherwise alias a file outside runtime storage.
	if err = unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		_ = unix.Close(fd)
		return failure
	}
	file := os.NewFile(uintptr(fd), e.logPath)
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		return failure
	}
	e.log = file
	return nil
}

func (e *boxEngine) Start() error {
	if err := e.openLog(); err != nil {
		return err
	}
	err := e.box.Start()
	if err != nil {
		e.event("start failed")
	} else {
		e.event("started")
	}
	return err
}

func (e *boxEngine) Close() error {
	e.closeOnce.Do(func() {
		if e.box != nil {
			e.closeErr = e.box.Close()
		}
		closeHistory(e.ctx)
		e.event("stopped")
		if e.log != nil {
			if err := e.log.Close(); e.closeErr == nil {
				e.closeErr = err
			}
			e.log = nil
		}
	})
	return e.closeErr
}

func closeHistory(ctx context.Context) {
	if history := service.PtrFromContext[urltest.HistoryStorage](ctx); history != nil {
		history.Close()
	}
}

func (e *boxEngine) Pause() { e.box.CloseIdleConnections(); e.pause.DevicePause(); e.event("paused") }

// Upstream ignores Wake on iOS and ends the pause from WakeNow/screen-state
// reports. This binding has neither path: hosts call only Pause and Wake, and
// Pause is unconditional, so Wake must end the device pause on every platform.
// DeviceWake is a no-op when the device is not paused.
func (e *boxEngine) Wake()               { e.pause.DeviceWake(); e.event("awake") }
func (e *boxEngine) NeedWIFIState() bool { return e.box.Network().NeedWIFIState() }
