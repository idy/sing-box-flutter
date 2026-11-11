// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	boxservice "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"github.com/sagernet/sing/service/pause"

	"golang.org/x/sys/unix"
)

func cacheTestContext(dir string) context.Context {
	outbounds := outbound.NewRegistry()
	direct.RegisterOutbound(outbounds)
	group.RegisterSelector(outbounds)
	dnsRegistry := dns.NewTransportRegistry()
	transport.RegisterUDP(dnsRegistry)
	ctx := service.ContextWithDefaultRegistry(context.Background())
	ctx = filemanager.WithDefault(ctx, dir, dir, os.Getuid(), os.Getgid())
	return box.Context(ctx, inbound.NewRegistry(), outbounds, endpoint.NewRegistry(), dnsRegistry, boxservice.NewRegistry(), certificate.NewRegistry())
}

func startCachedSelector(t *testing.T, dir string, cacheOptions *option.CacheFileOptions) (*group.Selector, adapter.CacheFile, func()) {
	t.Helper()
	ctx := cacheTestContext(dir)
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(`{"outbounds":[{"type":"selector","tag":"choose","outbounds":["defaultA","proxyB"],"default":"defaultA"},{"type":"direct","tag":"defaultA"},{"type":"direct","tag":"proxyB"}],"route":{"final":"choose"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cacheOptions != nil {
		options.Experimental = &option.ExperimentalOptions{CacheFile: cacheOptions}
	}
	var original option.CacheFileOptions
	if cacheOptions != nil {
		original = *cacheOptions
	}
	mobile, err := newBoxEngine(ctx, &monitorTestPlatform{}, options)
	if err != nil {
		t.Fatal(err)
	}
	engine := mobile.(*boxEngine)
	t.Cleanup(func() { _ = engine.Close() })
	if cacheOptions != nil && !reflect.DeepEqual(*cacheOptions, original) {
		t.Fatal("mobile defaults mutated the caller's cache options")
	}
	cache := service.FromContext[adapter.CacheFile](engine.ctx)
	if cache == nil {
		t.Fatal("mobile engine did not register its default cache")
	}
	// Start only the real cache and selector stages needed for persistence. No
	// TUN, monitor, API listener or network endpoint is started by this test.
	scope := adapter.NewScope(engine.ctx, log.NewNOPFactory().Logger())
	t.Cleanup(func() { _ = scope.Close() })
	if err := scope.Start("cache", cache, adapter.StartStateInitialize); err != nil {
		t.Fatal(err)
	}
	selected, loaded := engine.box.Outbound().Outbound("choose")
	if !loaded {
		t.Fatal("selector missing")
	}
	selector := selected.(*group.Selector)
	if err := scope.Start("selector", selector, adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	var closeOnce sync.Once
	close := func() {
		closeOnce.Do(func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
			if err := engine.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	return selector, cache, close
}

func TestMobileCachePersistsSelectorForAllEnableSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options *option.CacheFileOptions
	}{
		{"absent", nil},
		{"explicit-false", &option.CacheFileOptions{Enabled: false}},
		{"explicit-true", &option.CacheFileOptions{Enabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			first, _, closeFirst := startCachedSelector(t, dir, tc.options)
			defer closeFirst()
			if first.Selected(N.NetworkTCP).Tag() != "defaultA" || !first.SelectOutbound("proxyB") {
				t.Fatal("could not change the selector")
			}
			closeFirst()
			second, _, closeSecond := startCachedSelector(t, dir, tc.options)
			defer closeSecond()
			if second.Selected(N.NetworkTCP).Tag() != "proxyB" {
				t.Fatal("mobile selector choice was not restored")
			}
		})
	}
}

func TestMobileCachePreservesPathIDAndFlags(t *testing.T) {
	dir := t.TempDir()
	options := &option.CacheFileOptions{
		Enabled: false, Path: "custom.db", CacheID: "profile-A",
		StoreFakeIP: true, StoreRDRC: true, StoreDNS: true,
		RDRCTimeout: badoption.Duration(time.Hour),
	}
	first, cache, closeFirst := startCachedSelector(t, dir, options)
	defer closeFirst()
	if cache.CacheID() != options.CacheID || !cache.StoreFakeIP() || !cache.StoreRDRC() || !cache.StoreDNS() {
		t.Fatal("custom cache options were lost")
	}
	if _, err := os.Stat(filepath.Join(dir, options.Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); !os.IsNotExist(err) {
		t.Fatal("custom cache path was replaced with the default")
	}
	if !first.SelectOutbound("proxyB") {
		t.Fatal("selector change failed")
	}
	closeFirst()
	otherOptions := *options
	otherOptions.CacheID = "profile-B"
	other, _, closeOther := startCachedSelector(t, dir, &otherOptions)
	if other.Selected(N.NetworkTCP).Tag() != "defaultA" {
		t.Fatal("cache IDs no longer isolate selector state")
	}
	closeOther()
	restored, _, closeRestored := startCachedSelector(t, dir, options)
	defer closeRestored()
	if restored.Selected(N.NetworkTCP).Tag() != "proxyB" {
		t.Fatal("custom cache ID did not restore its state")
	}
}

func TestMobileCacheRestoresExistingPinnedCache(t *testing.T) {
	dir := t.TempDir()
	ctx := cacheTestContext(dir)
	logger := log.NewNOPFactory().Logger()
	scope := adapter.NewScope(ctx, logger)
	defer scope.Close()
	// The pinned constructor enabled this service even with Enabled=false.
	cache := cachefile.New(ctx, logger, option.CacheFileOptions{Enabled: false})
	if err := scope.Start("legacy cache", cache, adapter.StartStateInitialize); err != nil {
		t.Fatal(err)
	}
	if err := cache.StoreSelected("choose", "proxyB"); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	selector, _, close := startCachedSelector(t, dir, nil)
	defer close()
	if selector.Selected(N.NetworkTCP).Tag() != "proxyB" {
		t.Fatal("existing mobile cache was ignored")
	}
}

func TestMobileWakeEndsDevicePauseOnEveryPlatform(t *testing.T) {
	ctx := pause.WithDefaultManager(context.Background())
	manager := service.FromContext[pause.Manager](ctx)
	logFile, err := os.CreateTemp(t.TempDir(), "mobile-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	engine := &boxEngine{ctx: ctx, pause: manager, log: logFile}
	var events []int
	manager.RegisterCallback(func(event int) { events = append(events, event) })
	// Waking an active core must not emit a spurious wake transition.
	engine.Wake()
	// Pause ends with this same device pause; no host reports screen state.
	manager.DevicePause()
	if !manager.IsPaused() {
		t.Fatal("device pause did not pause the core")
	}
	engine.Wake()
	if manager.IsDevicePaused() || manager.IsPaused() {
		t.Fatal("wake left the core paused")
	}
	if !slices.Equal(events, []int{pause.EventDevicePaused, pause.EventDeviceWake}) {
		t.Fatalf("pause events=%v", events)
	}
	content, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(content), " awake\n") != 2 {
		t.Fatal("wake did not record its lifecycle event")
	}
}

func privateLogTestContext(dir string) context.Context {
	return filemanager.WithDefault(context.Background(), dir, dir, os.Getuid(), os.Getgid())
}

func privateLogTestEngine(t *testing.T, dir, output string) *boxEngine {
	t.Helper()
	ctx := privateLogTestContext(dir)
	path, err := privateLogPath(ctx, &option.LogOptions{Output: output})
	if err != nil {
		t.Fatal(err)
	}
	engine := &boxEngine{ctx: ctx, logPath: path}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

func writePrivateLogTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Fixtures live only in t.TempDir. Set a known mode independently of umask
	// so an unintended chmod to 0600 is observable on every test host.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertPrivateLogUnchanged(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("permissions changed: got %o, want %o", info.Mode().Perm(), mode)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("file content changed: %q", got)
	}
}

func assertPrivateLogOpenRejected(t *testing.T, engine *boxEngine) {
	t.Helper()
	if err := engine.openLog(); err == nil || err.Error() != "could not open private mobile log" {
		t.Fatalf("expected sanitized log access error, got %v", err)
	}
	if engine.log != nil {
		t.Fatal("rejected log retained a writable descriptor")
	}
}

func TestPrivateLogPathConfinement(t *testing.T) {
	dir := t.TempDir()
	ctx := privateLogTestContext(dir)
	for _, tc := range []struct {
		name    string
		options *option.LogOptions
		want    string
		wantErr string
	}{
		{name: "absent"},
		{name: "disabled", options: &option.LogOptions{Disabled: true, Output: "../ignored.log"}},
		{name: "default", options: &option.LogOptions{}, want: "mobile.log"},
		{name: "relative", options: &option.LogOptions{Output: "logs/mobile.log"}, want: "logs/mobile.log"},
		{name: "absolute-inside", options: &option.LogOptions{Output: filepath.Join(dir, "logs/mobile.log")}, want: "logs/mobile.log"},
		{name: "parent-escape", options: &option.LogOptions{Output: "../outside.log"}, wantErr: "mobile logging requires a private file"},
		{name: "nested-escape", options: &option.LogOptions{Output: "logs/../../outside.log"}, wantErr: "mobile logging requires a private file"},
		{name: "absolute-outside", options: &option.LogOptions{Output: filepath.Join(dir, "..", "outside.log")}, wantErr: "mobile logging requires a private file"},
		{name: "prefix-sibling", options: &option.LogOptions{Output: dir + "-other/mobile.log"}, wantErr: "mobile logging requires a private file"},
		{name: "directory", options: &option.LogOptions{Output: "."}, wantErr: "mobile logging requires a private file"},
		{name: "stdout", options: &option.LogOptions{Output: "stdout"}, wantErr: "mobile logging requires a private file"},
		{name: "stderr", options: &option.LogOptions{Output: "stderr"}, wantErr: "mobile logging requires a private file"},
		{name: "invalid-level", options: &option.LogOptions{Level: "secret-invalid-level"}, wantErr: "invalid mobile log level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := privateLogPath(ctx, tc.options)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr || got != "" {
					t.Fatalf("path=%q, error=%v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("path=%q, error=%v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := privateLogPath(context.Background(), &option.LogOptions{}); err == nil || err.Error() != "mobile logging requires a private file" {
		t.Fatalf("unconfigured runtime accepted: %v", err)
	}
}

func TestPrivateLogRejectsOutsidePathsAtOpen(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	if err := os.Mkdir(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.log")
	writePrivateLogTestFile(t, outside, "unchanged\n")
	for _, path := range []string{outside, "../outside.log", "../new/mobile.log"} {
		// Exercise the open-time boundary independently of option validation,
		// using only disposable paths even if both protections regress.
		engine := &boxEngine{ctx: privateLogTestContext(runtime), logPath: path}
		t.Cleanup(func() { _ = engine.Close() })
		assertPrivateLogOpenRejected(t, engine)
	}
	assertPrivateLogUnchanged(t, outside, "unchanged\n", 0o644)
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Fatalf("created storage outside runtime: %v", err)
	}
}

func TestPrivateLogPathRejectsDevicePaths(t *testing.T) {
	ctx := privateLogTestContext(t.TempDir())
	for _, output := range []string{"/dev/null", "/dev/zero", "/dev/tty"} {
		t.Run(filepath.Base(output), func(t *testing.T) {
			// Validation only: never pass host devices to openLog, even if a
			// regression lets one of these paths through validation.
			path, err := privateLogPath(ctx, &option.LogOptions{Output: output})
			if err == nil || err.Error() != "mobile logging requires a private file" || path != "" {
				t.Fatalf("device path accepted: path=%q, error=%v", path, err)
			}
		})
	}
}

func TestPrivateLogValidationDoesNotCreateStorage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing-runtime")
	engine := privateLogTestEngine(t, dir, "logs/mobile.log")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("path validation created runtime storage: %v", err)
	}
	assertPrivateLogOpenRejected(t, engine)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("log opening created an unconfigured runtime directory: %v", err)
	}
}

func TestPrivateLogAppendsRegularFiles(t *testing.T) {
	for _, kind := range []string{"relative", "absolute-inside"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mobile.log")
			writePrivateLogTestFile(t, path, "previous\n")
			output := "mobile.log"
			if kind == "absolute-inside" {
				output = path
			}
			engine := privateLogTestEngine(t, dir, output)
			if err := engine.openLog(); err != nil {
				t.Fatal(err)
			}
			opened := engine.log
			if err := engine.openLog(); err != nil || engine.log != opened {
				t.Fatalf("repeated log opening replaced the descriptor: %v", err)
			}
			engine.event("started")
			if err := engine.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
				t.Fatalf("log is not a private regular file: %v, %v", info, err)
			}
			content, err := os.ReadFile(path)
			if err != nil || !strings.HasPrefix(string(content), "previous\n") || strings.Count(string(content), " started\n") != 1 || strings.Count(string(content), " stopped\n") != 1 {
				t.Fatalf("log was not appended: %q, %v", content, err)
			}
		})
	}
}

func TestPrivateLogCreatesNestedFilesThroughTrustedRuntimeAlias(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	if err := os.Mkdir(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	// Apple and Android may provide storage through trusted system aliases.
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(runtime, alias); err != nil {
		t.Fatal(err)
	}
	engine := privateLogTestEngine(t, alias, "logs/mobile.log")
	if err := engine.openLog(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(runtime, "logs"))
	if err != nil || info.Mode().Perm() != 0o700 || !info.IsDir() {
		t.Fatalf("log parent is not private: %v, %v", info, err)
	}
	assertPrivateLogUnchanged(t, filepath.Join(runtime, "logs/mobile.log"), "", 0o600)
}

func TestPrivateLogRejectsSymlinkEscapes(t *testing.T) {
	for _, kind := range []string{"absolute-leaf", "relative-leaf", "inside-leaf", "dangling-leaf", "absolute-parent", "relative-parent"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			runtime, outside := filepath.Join(dir, "runtime"), filepath.Join(dir, "outside")
			for _, path := range []string{runtime, outside} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				writePrivateLogTestFile(t, filepath.Join(path, "victim.log"), "unchanged\n")
			}
			output, target := "link", filepath.Join(outside, "victim.log")
			switch kind {
			case "relative-leaf":
				target = "../outside/victim.log"
			case "inside-leaf":
				target = "victim.log"
			case "dangling-leaf":
				target = "../outside/missing.log"
			case "absolute-parent":
				output, target = "link/new/mobile.log", outside
			case "relative-parent":
				output, target = "link/new/mobile.log", "../outside"
			}
			engine := privateLogTestEngine(t, runtime, output)
			// Install the link after path validation to exercise the open-time
			// boundary rather than relying on a prior path/type inspection.
			if err := os.Symlink(target, filepath.Join(runtime, "link")); err != nil {
				t.Fatal(err)
			}
			assertPrivateLogOpenRejected(t, engine)
			assertPrivateLogUnchanged(t, filepath.Join(outside, "victim.log"), "unchanged\n", 0o644)
			assertPrivateLogUnchanged(t, filepath.Join(runtime, "victim.log"), "unchanged\n", 0o644)
			for _, path := range []string{filepath.Join(outside, "new"), filepath.Join(outside, "missing.log")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("created storage through symlink: %v", err)
				}
			}
		})
	}
}

func TestPrivateLogRejectsNonRegularFiles(t *testing.T) {
	for _, kind := range []string{"directory", "fifo-without-reader", "fifo-with-reader"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mobile.log")
			if kind == "directory" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := unix.Mkfifo(path, 0o640); err != nil {
					t.Fatal(err)
				}
				if kind == "fifo-with-reader" {
					// Keep a reader so the writer open succeeds and the descriptor
					// type check, rather than ENXIO, must reject this temporary FIFO.
					reader, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = reader.Close() })
				}
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			assertPrivateLogOpenRejected(t, privateLogTestEngine(t, dir, "mobile.log"))
			after, err := os.Stat(path)
			if err != nil || after.Mode() != before.Mode() {
				t.Fatalf("nonregular log permissions changed: %v, %v", after, err)
			}
		})
	}
}

func TestPrivateLogRejectsHardLinkAliases(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	if err := os.Mkdir(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.log")
	writePrivateLogTestFile(t, outside, "unchanged\n")
	if err := os.Link(outside, filepath.Join(runtime, "mobile.log")); err != nil {
		t.Fatal(err)
	}
	assertPrivateLogOpenRejected(t, privateLogTestEngine(t, runtime, "mobile.log"))
	assertPrivateLogUnchanged(t, outside, "unchanged\n", 0o644)
}

func TestPrivateLogWritesRetainOpenedFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	writePrivateLogTestFile(t, outside, "unchanged\n")
	engine := privateLogTestEngine(t, dir, "mobile.log")
	if err := engine.openLog(); err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(dir, "retained.log")
	if err := os.Rename(filepath.Join(dir, "mobile.log"), retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "mobile.log")); err != nil {
		t.Fatal(err)
	}
	engine.event("started")
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertPrivateLogUnchanged(t, outside, "unchanged\n", 0o644)
	content, err := os.ReadFile(retained)
	if err != nil || strings.Count(string(content), " started\n") != 1 {
		t.Fatalf("event did not reach the opened file: %q, %v", content, err)
	}
}
