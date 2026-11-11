// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
)

func TestOwnedStoragePreservesDefaultRelativeExternalCacheAndTempIO(t *testing.T) {
	for _, kind := range []string{"default", "relative", "external"} {
		t.Run(kind, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			working := filepath.Join(base, "Host App", "sing_box_flutter")
			lease, err := initializeStorage(working)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			ctx := withStorageContext(context.Background(), lease)
			path, want := "", filepath.Join(working, "cache.db")
			if kind == "relative" {
				path, want = "profile.db", filepath.Join(working, "profile.db")
			} else if kind == "external" {
				external := filepath.Join(base, "external-cache")
				if err := privateDirectory(external); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(external, "explicit.db")
				want = path
			}
			encoded, _ := json.Marshal(path)
			content := `{"experimental":{"cache_file":{"enabled":true,"path":` + string(encoded) + `}}}`
			if runtime.GOOS == "windows" && kind == "external" {
				// Current labels cannot prove when external handles were granted.
				// Reject the explicit path rather than rewriting it to managed state.
				if instance, err := createBox(ctx, content, working, false); err != errConfig || instance != nil {
					if instance != nil {
						instance.Close()
					}
					t.Fatalf("external cache must fail with a sanitized config error: %v", err)
				}
				for _, unexpected := range []string{want, filepath.Join(working, "explicit.db"), filepath.Join(working, "cache.db")} {
					if _, err := os.Lstat(unexpected); !os.IsNotExist(err) {
						t.Fatalf("external cache rejection created or rewrote a path: %s: %v", unexpected, err)
					}
				}
				return
			}
			for attempt := range 2 {
				instance, err := createBox(ctx, content, working, false)
				if err != nil {
					t.Fatal(err)
				}
				defer instance.Close()
				cache := service.FromContext[adapter.CacheFile](instance.ctx)
				if cache == nil {
					t.Fatal("native cache was not registered")
				}
				scope := adapter.NewScope(instance.ctx, log.NewNOPFactory().Logger())
				defer scope.Close()
				if err := scope.Start("cache", cache, adapter.StartStateInitialize); err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					if err := cache.StoreSelected("persisted", "selection"); err != nil {
						t.Fatal(err)
					}
				} else if got := cache.LoadSelected("persisted"); got != "selection" {
					t.Fatalf("native persistence changed: %q", got)
				}
				if _, err := os.Stat(want); err != nil {
					t.Fatalf("cache path changed: %v", err)
				}
				temp, err := filemanager.CreateTemp(instance.ctx, "native-temp-*")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := temp.WriteString("native temp data"); err != nil {
					t.Fatal(err)
				}
				temp.Close()
				if filepath.Dir(temp.Name()) != filepath.Join(working, "tmp") {
					t.Fatal("native temp path changed")
				}
				// Exercise native path creation/read/write under a newly created
				// state directory, as used by temp/Tailscale-backed services.
				nativeState := filemanager.BasePath(instance.ctx, filepath.Join("tailscale-state", "state.bin"))
				if nativeState != filepath.Join(working, "tailscale-state", "state.bin") {
					t.Fatal("external cache changed native persistent storage root")
				}
				if err := os.MkdirAll(filepath.Dir(nativeState), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(nativeState, []byte("state"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := os.ReadDir(filepath.Dir(nativeState)); err != nil {
					t.Fatal(err)
				}
				if err := scope.Close(); err != nil {
					t.Fatal(err)
				}
				if err := instance.Close(); err != nil {
					t.Fatal(err)
				}
			}
			lease.Close()
			// Reopening must preserve the database and seal all native-created
			// descendants rather than rejecting ordinary safe persisted files.
			reopened, err := initializeStorage(working)
			if err != nil {
				t.Fatalf("could not reacquire persisted state on %s: %v", runtime.GOOS, err)
			}
			reopened.Close()
		})
	}
}
