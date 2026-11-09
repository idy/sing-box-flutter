// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"

	C "github.com/sagernet/sing-box/constant"
)

var (
	setupAccess      sync.RWMutex
	sBasePath        string
	sWorkingPath     string
	sTempPath        string
	sUserID          = os.Getuid()
	sGroupID         = os.Getgid()
	sFixAndroidStack atomic.Bool
	coreFingerprint  string
)

// SetupOptions retains the fields assigned by the Android/iOS hosts. RPC fields
// are binding compatibility only: this core never opens a command listener.
type SetupOptions struct {
	BasePath                string
	WorkingPath             string
	TempPath                string
	FixAndroidStack         bool
	CommandServerListenPort int32
	CommandServerSecret     string
	LogMaxLines             int
	Debug                   bool
	CrashReportSource       string
	OomKillerEnabled        bool
	OomMemoryLimit          int64
}

func Setup(options *SetupOptions) error {
	if options == nil {
		return errors.New("mobile setup options are required")
	}
	if options.CommandServerListenPort != 0 {
		return errors.New("mobile command listeners are unsupported")
	}
	paths := []string{options.BasePath, options.WorkingPath, options.TempPath}
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			return errors.New("mobile directories must use absolute paths")
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return errors.New("could not create private mobile directories")
		}
	}
	setupAccess.Lock()
	sBasePath, sWorkingPath, sTempPath = paths[0], paths[1], paths[2]
	sUserID, sGroupID = os.Getuid(), os.Getgid()
	sFixAndroidStack.Store(options.FixAndroidStack)
	setupAccess.Unlock()
	// A Go heap budget is sufficient here; no daemon OOM recorder/service is
	// installed. The host controls the lifetime of the Network Extension.
	if options.OomKillerEnabled {
		budget := options.OomMemoryLimit
		if budget == 0 && C.IsIos {
			budget = 50 * 1024 * 1024
		}
		if budget > 0 {
			debug.SetMemoryLimit(budget / 5 * 4)
		}
	}
	return nil
}

func Version() string { return C.Version }

// CoreFingerprint is set by the build from the selected capabilities receipt.
func CoreFingerprint() string { return coreFingerprint }

// All callers lock around generated baseContext's setup globals. Each service
// receives its own registry/context; a failed reload cannot poison the next one.
func mobileBaseContext(platform PlatformInterface) context.Context {
	setupAccess.RLock()
	defer setupAccess.RUnlock()
	return baseContext(platform)
}
