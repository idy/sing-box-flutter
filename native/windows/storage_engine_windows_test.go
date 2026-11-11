// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"golang.org/x/sys/windows"
)

func managedCacheFixture(t *testing.T) (context.Context, string, string) {
	t.Helper()
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "Host App", "sing_box_flutter")
	lease, err := initializeStorage(working)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	return withStorageContext(context.Background(), lease), working, filepath.Join(working, "cache")
}

func externalCacheFixture(t *testing.T) (context.Context, string, string) {
	t.Helper()
	ctx, working, _ := managedCacheFixture(t)
	external := filepath.Join(filepath.Dir(filepath.Dir(working)), "external-cache")
	if err := privateDirectory(external); err != nil {
		t.Fatal(err)
	}
	return ctx, working, external
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

func TestManagedCachePinsSurviveChecksAndNativeCleanup(t *testing.T) {
	ctx, working, managed := managedCacheFixture(t)
	path := filepath.Join(managed, "profile.db")
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
	assertCachePinned(t, managed, path)
	assertStorageIntegrity(t, path, false)
	withMediumIntegrity(t, func() {
		if _, err := os.ReadFile(path); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("Medium cache read must fail with access denied: %v", err)
		}
		if err := os.WriteFile(path, []byte("attacker"), 0o600); err == nil {
			t.Fatal("lower integrity process wrote managed cache")
		}
		if handle, err := directoryMutationHandle(managed); err == nil {
			windows.CloseHandle(handle)
			t.Fatal("lower integrity process acquired managed directory mutation rights")
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
	assertCachePinned(t, managed, path)
	engine.constructors.Add(func() error {
		assertCachePinned(t, managed, path)
		return nil
	})
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertCacheReleased(t, path)
	assertCachePinned(t, managed) // The controller still owns its storage lease.
}

func TestManagedCacheLeaseReleasedOnConstructionAndStartFailure(t *testing.T) {
	ctx, working, managed := managedCacheFixture(t)
	path := filepath.Join(managed, "profile.db")
	duplicate := externalCacheConfig(path, `,"outbounds":[{"type":"direct","tag":"dup"},{"type":"direct","tag":"dup"}]`)
	for range 2 {
		if engine, err := createBox(ctx, duplicate, working, false); err != errConfig || engine != nil {
			if engine != nil {
				engine.Close()
			}
			t.Fatalf("expected sanitized constructor failure, got %v", err)
		}
		assertCacheReleased(t, path)
		assertCachePinned(t, managed)
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
		assertCachePinned(t, managed, path)
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		assertCacheReleased(t, path)
		assertCachePinned(t, managed)
	}
}

func TestManagedCachePersistsAcrossStorageOwnerReopen(t *testing.T) {
	for _, kind := range []string{"default", "relative", "nested", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			working := filepath.Join(base, "Host App", "sing_box_flutter")
			path, want := "", filepath.Join(working, "cache.db")
			switch kind {
			case "relative":
				path, want = "profile.db", filepath.Join(working, "profile.db")
			case "nested":
				path = filepath.Join("cache", "nested", "profile.db")
				want = filepath.Join(working, path)
			case "absolute":
				path = filepath.Join(working, "cache", "profile.db")
				want = path
			}
			for attempt := range 2 {
				lease, err := initializeStorage(working)
				if err != nil {
					t.Fatalf("owner initialization %d: %v", attempt, err)
				}
				defer lease.Close()
				if kind == "nested" {
					if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				ctx := withStorageContext(context.Background(), lease)
				engine, err := createBox(ctx, externalCacheConfig(path, ""), working, false)
				if err != nil {
					t.Fatal(err)
				}
				defer engine.Close()
				cache := service.FromContext[adapter.CacheFile](engine.ctx)
				if err := engine.constructors.Start("cache", cache, adapter.StartStateInitialize); err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					if err := cache.StoreSelected("selection", "persisted"); err != nil {
						t.Fatal(err)
					}
				} else if got := cache.LoadSelected("selection"); got != "persisted" {
					t.Fatalf("managed cache persistence changed after owner reopen: %q", got)
				}
				assertStorageIntegrity(t, want, false)
				temp, err := filemanager.CreateTemp(engine.ctx, "native-temp-*")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := temp.WriteString("native temp state"); err != nil {
					temp.Close()
					t.Fatal(err)
				}
				if err := temp.Close(); err != nil {
					t.Fatal(err)
				}
				if filepath.Dir(temp.Name()) != filepath.Join(working, "tmp") {
					t.Fatal("cache admission changed native temp storage")
				}
				assertStorageIntegrity(t, temp.Name(), true)
				if err := engine.Close(); err != nil {
					t.Fatal(err)
				}
				if err := lease.Close(); err != nil {
					t.Fatal(err)
				}
				assertCacheReleased(t, working)
			}
		})
	}
}

func TestManagedCacheRequiresLiveOwnerAndLiteralHierarchy(t *testing.T) {
	for _, kind := range []string{"missing-owner", "closed-owner", "sibling-prefix", "case-alias"} {
		t.Run(kind, func(t *testing.T) {
			ctx, working, managed := managedCacheFixture(t)
			path := filepath.Join(managed, "profile.db")
			switch kind {
			case "missing-owner":
				ctx = context.Background()
			case "closed-owner":
				if err := ctx.Value(storageContextKey{}).(*windowsStorage).Close(); err != nil {
					t.Fatal(err)
				}
			case "sibling-prefix":
				if err := privateDirectory(working + "-sibling"); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(working+"-sibling", "profile.db")
			case "case-alias":
				path = filepath.Join(filepath.Dir(filepath.Dir(working)), "host app", "sing_box_flutter", "cache", "profile.db")
			}
			if engine, err := createBox(ctx, externalCacheConfig(path, ""), working, false); err != errConfig || engine != nil {
				if engine != nil {
					engine.Close()
				}
				t.Fatalf("cache admitted without its live literal owner: %v", err)
			}
			for _, unexpected := range []string{path, filepath.Join(managed, "profile.db"), filepath.Join(working, "cache.db")} {
				if _, err := os.Lstat(unexpected); !os.IsNotExist(err) {
					t.Fatalf("rejected cache path was created or rewritten: %s: %v", unexpected, err)
				}
			}
		})
	}
}

func TestManagedCacheRejectsUnsafePathsWithoutMutation(t *testing.T) {
	for _, kind := range []string{"directory-policy", "file-policy", "junction", "symlink", "hardlink", "alias"} {
		t.Run(kind, func(t *testing.T) {
			ctx, working, managed := managedCacheFixture(t)
			outside := t.TempDir()
			target := filepath.Join(outside, "secret-target.db")
			if err := os.WriteFile(target, []byte("outside state"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(managed, "profile.db")
			switch kind {
			case "directory-policy", "file-policy":
				directories, err := openDirectoryHierarchy(managed)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "directory-policy" {
					_, err = directories.openChild(directories.last(), "medium", true, unsafeStorageDescriptor(t, "medium"))
					path = filepath.Join(managed, "medium", "profile.db")
				} else {
					_, err = directories.openFile(directories.last(), "profile.db", true, false, unsafeStorageDescriptor(t, "medium"))
				}
				directories.close()
				if err != nil {
					t.Fatal(err)
				}
			case "junction":
				link := filepath.Join(managed, "link")
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
				t.Fatalf("unsafe managed cache accepted or leaked details: %v", err)
			}
			if storagePolicySnapshot(t, target) != beforeFile || storagePolicySnapshot(t, outside) != beforeDirectory {
				t.Fatal("rejected managed cache changed outside security policy")
			}
			if contents, err := os.ReadFile(target); err != nil || string(contents) != "outside state" {
				t.Fatalf("rejected cache modified outside state: %q, %v", contents, err)
			}
			if _, err := os.Lstat(filepath.Join(outside, "profile.db")); !os.IsNotExist(err) {
				t.Fatalf("rejected cache created outside state before construction failed: %v", err)
			}
			if kind == "directory-policy" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("rejected unsafe parent created cache data: %v", err)
				}
			}
			assertCachePinned(t, managed)
		})
	}
}

// Only owned fixtures are deliberately hardened in place to model an external
// administrator; production admission must not perform this transition.
func setOwnedCachePolicy(t *testing.T, path string, descriptor *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.WRITE_DAC|windows.WRITE_OWNER|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	label, _, err := descriptor.SACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION,
		nil, nil, dacl, label); err != nil {
		t.Fatal(err)
	}
}

func TestExternalCacheRejectsRetainedMediumDataHandlesAfterHardening(t *testing.T) {
	for _, tc := range []struct {
		name   string
		access uint32
	}{
		{"read", windows.FILE_READ_DATA},
		{"write", windows.FILE_WRITE_DATA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, working, external := externalCacheFixture(t)
			path := filepath.Join(external, "profile.db")
			content := externalCacheConfig(path, "")
			before := []byte("external cache persisted bytes")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "untouched.bin")
			if err := os.WriteFile(outside, []byte("outside state"), 0o600); err != nil {
				t.Fatal(err)
			}
			outsidePolicy := storagePolicySnapshot(t, outside)
			setOwnedCachePolicy(t, path, unsafeStorageDescriptor(t, "medium"))
			var hostile windows.Handle
			withMediumIntegrity(t, func() {
				name, err := windows.UTF16PtrFromString(path)
				if err != nil {
					t.Fatal(err)
				}
				hostile, err = windows.CreateFile(name, tc.access,
					windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
					windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				if err != nil {
					t.Fatal(err)
				}
			})
			defer func() {
				if hostile != 0 {
					windows.CloseHandle(hostile)
				}
			}()
			private, err := privateDirectorySecurity()
			if err != nil {
				t.Fatal(err)
			}
			setOwnedCachePolicy(t, path, private)
			policyBefore := storagePolicySnapshot(t, path)
			// Prove these granted data rights survive the administrator's change.
			// Writing identical bytes keeps the persisted fixture intact.
			withMediumIntegrity(t, func() {
				var count uint32
				if tc.access == windows.FILE_READ_DATA {
					var header [16]byte
					if err := windows.ReadFile(hostile, header[:], &count, nil); err != nil || count != 16 || !bytes.Equal(header[:], before[:16]) {
						t.Fatalf("retained read rights did not survive hardening: %v", err)
					}
				} else if err := windows.WriteFile(hostile, before[:16], &count, nil); err != nil || count != 16 {
					t.Fatalf("retained write rights did not survive hardening: %v", err)
				}
			})
			if rejected, err := createBox(ctx, content, working, false); err != errConfig || rejected != nil {
				if rejected != nil {
					rejected.Close()
				}
				t.Fatalf("accepted retained Medium data access: %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) || storagePolicySnapshot(t, path) != policyBefore {
				t.Fatalf("rejected cache admission changed existing state or policy: %v", err)
			}
			if after, err := os.ReadFile(outside); err != nil || string(after) != "outside state" || storagePolicySnapshot(t, outside) != outsidePolicy {
				t.Fatalf("rejected cache admission touched outside state: %v", err)
			}
			assertCacheReleased(t, external)
			if err := windows.CloseHandle(hostile); err != nil {
				t.Fatal(err)
			}
			hostile = 0
			if retry, err := createBox(ctx, content, working, false); err != errConfig || retry != nil {
				if retry != nil {
					retry.Close()
				}
				t.Fatalf("external cache gained provenance when hostile handle closed: %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("external persisted bytes changed: %v", err)
			}
			assertCacheReleased(t, path)
			assertCacheReleased(t, external)
		})
	}
}

func TestExternalCacheRejectsRetainedMediumMetadataRightsAfterHardening(t *testing.T) {
	for _, kind := range []string{"directory", "file"} {
		t.Run(kind, func(t *testing.T) {
			ctx, working, external := externalCacheFixture(t)
			directories, err := openDirectoryHierarchy(external)
			if err != nil {
				t.Fatal(err)
			}
			unsafe := unsafeStorageDescriptor(t, "medium")
			path := filepath.Join(external, "hardened")
			cachePath := filepath.Join(path, "profile.db")
			if kind == "directory" {
				_, err = directories.openChild(directories.last(), "hardened", true, unsafe)
			} else {
				_, err = directories.openFile(directories.last(), "hardened", true, false, unsafe)
				cachePath = path
			}
			directories.close()
			if err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				if err := os.WriteFile(path, []byte("external persisted state"), 0o600); err != nil {
					t.Fatal(err)
				}
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
			private, err := privateDirectorySecurity()
			if err != nil {
				t.Fatal(err)
			}
			setOwnedCachePolicy(t, path, private)
			assertStorageIntegrity(t, path, false)
			// Establish the loophole: all current policy checks and an exclusive
			// probe pass while Medium retains WRITE_DAC and WRITE_OWNER.
			directories, err = openDirectoryHierarchy(external)
			if err != nil {
				t.Fatal(err)
			}
			_, err = directories.openObject(directories.last(), "hardened", false, private,
				storageObjectOptions{directory: kind == "directory", nativeIO: kind == "file", exclusive: true})
			directories.close()
			if err != nil {
				t.Fatalf("metadata fixture must evade policy/share probes: %v", err)
			}
			before := storagePolicySnapshot(t, path)
			if engine, err := createBox(ctx, externalCacheConfig(cachePath, ""), working, false); err != errConfig || engine != nil {
				if engine != nil {
					engine.Close()
				}
				t.Fatalf("admitted hardened external object with retained metadata rights: %v", err)
			}
			if storagePolicySnapshot(t, path) != before {
				t.Fatal("mutated rejected external security policy")
			}
			if kind == "directory" {
				if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
					t.Fatalf("rejected external cache created a database: %v", err)
				}
			} else if data, err := os.ReadFile(path); err != nil || string(data) != "external persisted state" {
				t.Fatalf("rejected external cache changed data: %q, %v", data, err)
			}
			// Prove both metadata capabilities still work after hardening. These
			// are owned fixtures; production never performs an in-place seal.
			dacl, _, err := unsafe.DACL()
			if err != nil {
				t.Fatal(err)
			}
			label, _, err := unsafe.SACL()
			if err != nil {
				t.Fatal(err)
			}
			withMediumIntegrity(t, func() {
				if err := windows.SetSecurityInfo(metadata, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION,
					nil, nil, dacl, nil); err != nil {
					t.Fatalf("retained WRITE_DAC did not survive hardening: %v", err)
				}
				if err := windows.SetSecurityInfo(metadata, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION,
					nil, nil, nil, label); err != nil {
					t.Fatalf("retained WRITE_OWNER did not survive hardening: %v", err)
				}
			})
			assertCacheReleased(t, external)
		})
	}
}
