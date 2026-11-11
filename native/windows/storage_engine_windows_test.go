// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
	"golang.org/x/sys/windows"
)

func externalCacheFixture(t *testing.T) (context.Context, string, string) {
	t.Helper()
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "Host App", "sing_box_flutter")
	lease, err := initializeStorage(working)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	external := filepath.Join(base, "external-cache")
	if err := privateDirectory(external); err != nil {
		t.Fatal(err)
	}
	return withStorageContext(context.Background(), lease), working, external
}

func externalCacheConfig(path, extra string) string {
	encoded, _ := json.Marshal(path)
	return `{"experimental":{"cache_file":{"enabled":true,"path":` + string(encoded) + `}}` + extra + `}`
}

func assertCachePinned(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.Rename(path, path+"-moved"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("cache path was not pinned: %s: %v", path, err)
		}
	}
}

func assertCacheReleased(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatalf("cache lease survived cleanup: %v", err)
	}
	if err := os.Rename(path+"-moved", path); err != nil {
		t.Fatal(err)
	}
}

func TestExternalCachePinsSurviveChecksAndNativeCleanup(t *testing.T) {
	ctx, working, external := externalCacheFixture(t)
	path := filepath.Join(external, "profile.db")
	content := externalCacheConfig(path, "")
	engine, err := createBox(ctx, content, working, false)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	cache := service.FromContext[adapter.CacheFile](engine.ctx)
	// Put native cache cleanup in the engine-owned scope, so Close must close
	// the database before releasing the extra path lease.
	if err := engine.constructors.Start("cache", cache, adapter.StartStateInitialize); err != nil {
		t.Fatal(err)
	}
	if err := cache.StoreSelected("test", "selected"); err != nil {
		t.Fatal(err)
	}
	assertCachePinned(t, external, path)
	assertStorageIntegrity(t, path, false)
	withMediumIntegrity(t, func() {
		if err := os.WriteFile(path, []byte("attacker"), 0o600); err == nil {
			t.Fatal("lower integrity process wrote external cache")
		}
		if handle, err := directoryMutationHandle(external); err == nil {
			windows.CloseHandle(handle)
			t.Fatal("lower integrity process acquired external directory mutation rights")
		}
	})
	var checks sync.WaitGroup
	for range 4 {
		checks.Add(1)
		go func() {
			defer checks.Done()
			for range 2 {
				if err := (boxBackend{}).Check(ctx, content, working); err != nil {
					t.Errorf("repeated check failed with live cache: %v", err)
				}
			}
		}()
	}
	checks.Wait()
	assertCachePinned(t, external, path)
	engine.constructors.Add(func() error {
		assertCachePinned(t, external, path)
		return nil
	})
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertCacheReleased(t, path)
	assertCacheReleased(t, external)
}

func TestExternalCacheLeaseReleasedOnConstructionAndStartFailure(t *testing.T) {
	ctx, working, external := externalCacheFixture(t)
	path := filepath.Join(external, "profile.db")
	duplicate := externalCacheConfig(path, `,"outbounds":[{"type":"direct","tag":"dup"},{"type":"direct","tag":"dup"}]`)
	for range 2 {
		if engine, err := createBox(ctx, duplicate, working, false); err != errConfig || engine != nil {
			if engine != nil {
				engine.Close()
			}
			t.Fatalf("expected sanitized constructor failure, got %v", err)
		}
		assertCacheReleased(t, path)
		assertCacheReleased(t, external)
	}
	content := externalCacheConfig(path, `,"inbounds":[{"type":"tun","address":["172.19.0.1/30"],"auto_route":false}]`)
	for range 2 {
		parent, cancel := context.WithCancel(ctx)
		engine, err := (boxBackend{}).New(parent, content, working)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if err := engine.Start(); err == nil || err.Error() != "could not start Windows tunnel" {
			engine.Close()
			t.Fatalf("expected sanitized cancelled start, got %v", err)
		}
		assertCachePinned(t, external, path)
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		assertCacheReleased(t, path)
		assertCacheReleased(t, external)
	}
}

func TestExternalCacheRejectsUnsafePathsWithoutMutation(t *testing.T) {
	for _, kind := range []string{"directory-policy", "file-policy", "junction", "symlink", "hardlink", "alias"} {
		t.Run(kind, func(t *testing.T) {
			ctx, working, external := externalCacheFixture(t)
			outside := t.TempDir()
			target := filepath.Join(outside, "secret-target.db")
			if err := os.WriteFile(target, []byte("outside state"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(external, "profile.db")
			switch kind {
			case "directory-policy":
				path = filepath.Join(outside, "profile.db")
			case "file-policy":
				directories, err := openDirectoryHierarchy(external)
				if err != nil {
					t.Fatal(err)
				}
				_, err = directories.openFile(directories.last(), "profile.db", true, false, unsafeStorageDescriptor(t, "medium"))
				directories.close()
				if err != nil {
					t.Fatal(err)
				}
			case "junction":
				link := filepath.Join(external, "link")
				createDirectoryJunction(t, link, outside)
				path = filepath.Join(link, "profile.db")
			case "symlink":
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("file symlinks unavailable: %v", err)
				}
			case "hardlink":
				if err := os.Link(target, path); err != nil {
					t.Fatal(err)
				}
			case "alias":
				path += ":secret-stream"
			}
			beforeFile, beforeDirectory := storagePolicySnapshot(t, target), storagePolicySnapshot(t, outside)
			if engine, err := createBox(ctx, externalCacheConfig(path, ""), working, false); err != errConfig || engine != nil {
				if engine != nil {
					engine.Close()
				}
				t.Fatalf("unsafe cache accepted or leaked details: %v", err)
			}
			if storagePolicySnapshot(t, target) != beforeFile || storagePolicySnapshot(t, outside) != beforeDirectory {
				t.Fatal("rejected external cache changed outside security policy")
			}
			if contents, err := os.ReadFile(target); err != nil || string(contents) != "outside state" {
				t.Fatalf("rejected cache modified outside state: %q, %v", contents, err)
			}
			if _, err := os.Lstat(filepath.Join(outside, "profile.db")); !os.IsNotExist(err) {
				t.Fatalf("rejected cache created outside state before construction failed: %v", err)
			}
			assertCacheReleased(t, external)
		})
	}
}

func TestExternalCacheRejectsRetainedMediumMetadataRights(t *testing.T) {
	for _, kind := range []string{"directory", "file"} {
		t.Run(kind, func(t *testing.T) {
			ctx, working, external := externalCacheFixture(t)
			directories, err := openDirectoryHierarchy(external)
			if err != nil {
				t.Fatal(err)
			}
			unsafe := unsafeStorageDescriptor(t, "medium")
			path := filepath.Join(external, "medium")
			cachePath := filepath.Join(path, "profile.db")
			if kind == "directory" {
				_, err = directories.openChild(directories.last(), "medium", true, unsafe)
			} else {
				_, err = directories.openFile(directories.last(), "medium", true, false, unsafe)
				cachePath = path
			}
			directories.close()
			if err != nil {
				t.Fatal(err)
			}
			var metadata windows.Handle
			withMediumIntegrity(t, func() {
				name, err := windows.UTF16PtrFromString(path)
				if err != nil {
					t.Fatal(err)
				}
				metadata, err = windows.CreateFile(name, windows.WRITE_DAC|windows.WRITE_OWNER|windows.READ_CONTROL,
					windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
					windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				if err != nil {
					t.Fatal(err)
				}
			})
			defer windows.CloseHandle(metadata)
			before := storagePolicySnapshot(t, path)
			if engine, err := createBox(ctx, externalCacheConfig(cachePath, ""), working, false); err != errConfig || engine != nil {
				if engine != nil {
					engine.Close()
				}
				t.Fatalf("adopted preowned metadata rights: %v", err)
			}
			if storagePolicySnapshot(t, path) != before {
				t.Fatal("upgraded an object whose old metadata rights survive")
			}
			assertCacheReleased(t, external)
		})
	}
}
