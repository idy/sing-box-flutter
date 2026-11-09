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
	closeOnce sync.Once
	closeErr  error
}

func newBoxEngine(ctx context.Context, platform PlatformInterface, options option.Options) (mobileEngine, error) {
	ctx = pause.WithDefaultManager(ctx)
	ctx = service.ContextWith[adapter.PlatformInterface](ctx, &platformInterfaceWrapper{iif: platform})
	path, err := privateLogPath(ctx, options.Log)
	if err != nil {
		return nil, err
	}
	engine := &boxEngine{ctx: ctx, pause: service.FromContext[pause.Manager](ctx)}
	if path != "" {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, errors.New("could not open private mobile log")
		}
		engine.log, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			err = engine.log.Chmod(0o600)
		}
		if err != nil {
			_ = engine.Close()
			return nil, errors.New("could not open private mobile log")
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
	return filemanager.BasePath(ctx, output), nil
}

func (e *boxEngine) event(event string) {
	if e.log != nil {
		_, _ = e.log.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + event + "\n")
	}
}

func (e *boxEngine) Start() error {
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
