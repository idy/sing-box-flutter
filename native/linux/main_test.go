// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	libbox "github.com/sagernet/sing-box/experimental/libbox"
)

type fakeService struct {
	startErr   error
	closeErr   error
	start      func() error
	onClose    func()
	closed     atomic.Bool
	closeCalls atomic.Int32
	once       sync.Once
}

func (s *fakeService) Start() error {
	if s.start != nil {
		return s.start()
	}
	return s.startErr
}
func (s *fakeService) Close() error {
	s.closeCalls.Add(1)
	s.once.Do(func() {
		s.closed.Store(true)
		if s.onClose != nil {
			s.onClose()
		}
	})
	return s.closeErr
}

func testServer(output io.Writer, service *fakeService) *server {
	return &server{
		output: json.NewEncoder(output), state: "disconnected", initialized: true,
		create:     func(context.Context, *libbox.DesktopConfig) (coreService, error) { return service, nil },
		prepare:    func(string) (startConfig, error) { return startConfig{}, nil },
		permission: func() bool { return true },
	}
}

func TestStartFailureCleansUpAndRedactsDiagnostics(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{startErr: errors.New("secret-auth-key")}
	s := testServer(&output, service)
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if !service.closed.Load() || s.service != nil || s.state != "error" {
		t.Fatal("failed startup must close resources without reporting connected")
	}
	if strings.Contains(output.String(), "secret-auth-key") || strings.Contains(output.String(), `"state":"connected"`) {
		t.Fatal("raw errors or false success escaped onto the protocol")
	}
}

func TestLifecycleAndDuplicateStart(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if s.state != "connected" || s.service == nil {
		t.Fatal("successful core start must report connected")
	}
	s.handle(context.Background(), request{ID: 2, Method: "start", Config: `{}`})
	if !strings.Contains(output.String(), "already_started") || service.closed.Load() {
		t.Fatal("duplicate start must preserve the active service")
	}
	s.handle(context.Background(), request{ID: 3, Method: "stop"})
	if !service.closed.Load() || s.service != nil || s.state != "disconnected" {
		t.Fatal("stop must release the active service")
	}
}

func TestStopCleanupFailureRequiresHelperReplacement(t *testing.T) {
	var output lockedOutput
	service := &fakeService{closeErr: errors.New("secret-close-error")}
	s := testServer(&output, service)
	s.handle(context.Background(), request{ID: 1, Method: "start"})
	s.access.Lock()
	owner := s.service
	s.access.Unlock()
	s.handle(context.Background(), request{ID: 2, Method: "stop"})
	afterStop := len(output.text())
	s.handle(context.Background(), request{ID: 3, Method: "stop"})
	s.handle(context.Background(), request{ID: 4, Method: "initialize"})
	s.prepare = func(string) (startConfig, error) {
		t.Fatal("terminal helper attempted to prepare another connection")
		return startConfig{}, nil
	}
	s.handle(context.Background(), request{ID: 5, Method: "start"})
	s.handle(context.Background(), request{ID: 6, Method: "getStatus"})
	if err := s.close(); !errors.Is(err, errCleanupFailed) {
		t.Fatalf("repeated shutdown forgot cleanup failure: %v", err)
	}
	if err := s.waitForWorkers(); !errors.Is(err, errCleanupFailed) {
		t.Fatalf("worker drain forgot cleanup failure: %v", err)
	}
	s.access.Lock()
	retained := s.cleanupFailed && s.state == "error" && s.service == owner && owner != nil
	s.access.Unlock()
	if !retained || service.closeCalls.Load() != 1 {
		t.Fatal("failed owner must remain terminal without retrying its cached Close")
	}
	text := output.text()
	requireCleanupFailure(t, text, 2, 3, 4, 5)
	if strings.Contains(text[afterStop:], `"state":"disconnected"`) || strings.Contains(text[afterStop:], `"state":"disconnecting"`) ||
		!strings.Contains(text, `"id":6,"result":{"code":"cleanup_failed"`) {
		t.Fatalf("terminal helper reverted state or hid its failure from status:\n%s", text)
	}
}

func TestFailedStartupCleanupRequiresHelperReplacement(t *testing.T) {
	for _, construction := range []bool{false, true} {
		t.Run(fmt.Sprintf("construction=%v", construction), func(t *testing.T) {
			var output lockedOutput
			service := &fakeService{
				startErr: errors.New("secret-start-error"),
				closeErr: errors.New("secret-close-error"),
			}
			s := testServer(&output, service)
			var creates atomic.Int32
			s.create = func(context.Context, *libbox.DesktopConfig) (coreService, error) {
				creates.Add(1)
				if construction {
					return service, errors.New("secret-construction-error")
				}
				return service, nil
			}
			s.handle(context.Background(), request{ID: 1, Method: "start"})
			s.handle(context.Background(), request{ID: 2, Method: "stop"})
			s.handle(context.Background(), request{ID: 3, Method: "start"})
			if err := s.waitForWorkers(); !errors.Is(err, errCleanupFailed) {
				t.Fatalf("failed startup cleanup was not terminal: %v", err)
			}
			requireCleanupFailure(t, output.text(), 1, 2, 3)
			s.access.Lock()
			failed := s.cleanupFailed && s.state == "error"
			s.access.Unlock()
			if !failed || creates.Load() != 1 || service.closeCalls.Load() != 1 || strings.Contains(output.text(), `"state":"connected"`) {
				t.Fatal("failed startup cleanup allowed false success, recreation or duplicate Close")
			}
		})
	}
}

func TestPermissionDenialDoesNotCreateService(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	// Protocol lifecycle tests must not depend on the host's optional registry.
	s.prepare = func(string) (startConfig, error) { return startConfig{hasTun: true}, nil }
	s.permission = func() bool { return false }
	var created atomic.Bool
	s.create = func(context.Context, *libbox.DesktopConfig) (coreService, error) {
		created.Store(true)
		return service, nil
	}
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if created.Load() || s.service != nil || s.state != "disconnected" || !strings.Contains(output.String(), "permission_denied") {
		t.Fatal("unprivileged TUN start must fail before creating an engine")
	}
}

func TestStartReusesPreparedConfigAfterPermission(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	prepared := &libbox.DesktopConfig{}
	var preparations int
	var permitted bool
	var reused atomic.Bool
	s.prepare = func(string) (startConfig, error) {
		preparations++
		return startConfig{native: prepared, hasTun: true}, nil
	}
	s.permission = func() bool {
		permitted = preparations == 1
		return permitted
	}
	s.create = func(_ context.Context, config *libbox.DesktopConfig) (coreService, error) {
		reused.Store(permitted && config == prepared)
		return service, nil
	}
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if preparations != 1 || !reused.Load() || s.state != "connected" {
		t.Fatal("startup must prepare once, check TUN permission, and construct from those same options")
	}
	s.handle(context.Background(), request{ID: 2, Method: "stop"})
}

func TestFailedFactoryReturnsNilInterfaceAndNormalError(t *testing.T) {
	// An empty inline rule set parses for every registry, then fails construction.
	const content = `{"route":{"final":"missing","rule_set":[{"type":"inline","tag":"empty","rules":[]}]}}`
	config, err := libbox.ParseDesktopConfig(content)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := newCoreService(context.Background(), config)
	if err == nil || instance != nil {
		t.Fatal("failed concrete construction must preserve a nil service interface")
	}
	var output bytes.Buffer
	s := testServer(&output, &fakeService{})
	s.prepare, s.create = prepareStartConfig, newCoreService
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: content})
	if !strings.Contains(output.String(), "start_failed") || s.service != nil {
		t.Fatal("construction failure must be a recoverable response")
	}
}

func TestParentEOFClosesService(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	s.service = &ownedService{coreService: service, cancel: func() {}}
	s.serve(strings.NewReader(""), make(chan os.Signal))
	if !service.closed.Load() || s.service != nil {
		t.Fatal("parent pipe closure must clean up the TUN and routes")
	}
}

type lockedOutput struct {
	sync.Mutex
	bytes.Buffer
	changed chan struct{}
}

func (b *lockedOutput) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	n, err := b.Buffer.Write(p)
	if b.changed != nil {
		close(b.changed)
	}
	b.changed = make(chan struct{})
	return n, err
}
func (b *lockedOutput) snapshot() (string, <-chan struct{}) {
	b.Lock()
	defer b.Unlock()
	if b.changed == nil {
		b.changed = make(chan struct{})
	}
	return b.Buffer.String(), b.changed
}
func (b *lockedOutput) text() string {
	text, _ := b.snapshot()
	return text
}

func awaitOutput(t *testing.T, output *lockedOutput, contains string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		text, changed := output.snapshot()
		if strings.Contains(text, contains) {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("missing protocol output %q:\n%s", contains, text)
		}
	}
}

func awaitCleanupReply(t *testing.T, output *lockedOutput, id int) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		text, changed := output.snapshot()
		for _, line := range strings.Split(text, "\n") {
			var message struct {
				ID    int `json:"id"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(line), &message) == nil && message.ID == id {
				if message.Error == nil || message.Error.Code != "cleanup_failed" {
					t.Fatalf("request %d must fail cleanup: %s", id, line)
				}
				return
			}
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("missing cleanup failure reply %d:\n%s", id, text)
		}
	}
}

func replyIDs(t *testing.T, text string) []int {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	var ids []int
	for {
		var message struct {
			ID int `json:"id"`
		}
		if err := decoder.Decode(&message); err == io.EOF {
			return ids
		} else if err != nil {
			t.Fatalf("invalid protocol output: %v", err)
		}
		if message.ID != 0 {
			ids = append(ids, message.ID)
		}
	}
}

func requireCleanupFailure(t *testing.T, text string, ids ...int) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	seen := make(map[int]int)
	for {
		var message struct {
			ID    int `json:"id"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := decoder.Decode(&message); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("invalid protocol output: %v", err)
		}
		if !slices.Contains(ids, message.ID) {
			continue
		}
		seen[message.ID]++
		if message.Error == nil || message.Error.Code != "cleanup_failed" || message.Error.Message != cleanupFailureMessage {
			t.Fatalf("request %d must require helper replacement:\n%s", message.ID, text)
		}
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("request %d has %d replies:\n%s", id, seen[id], text)
		}
	}
	if strings.Contains(text, "secret-") {
		t.Fatalf("raw cleanup/start diagnostics escaped onto the protocol:\n%s", text)
	}
}

func await(t *testing.T, ready <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func TestShutdownInterruptsInFlightStartup(t *testing.T) {
	for _, signal := range []bool{false, true} {
		t.Run(fmt.Sprintf("signal=%v", signal), func(t *testing.T) {
			var output lockedOutput
			started, release := make(chan struct{}), make(chan struct{})
			// Model an upstream download that ignores context cancellation. Close
			// must still run and serve must return without waiting for Start.
			service := &fakeService{start: func() error { close(started); <-release; return nil }}
			s := testServer(&output, service)
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			defer close(release)
			signals := make(chan os.Signal, 1)
			stopped := make(chan struct{})
			go func() { s.serve(input, signals); close(stopped) }()
			_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start","config":"{}"}`)
			await(t, started, "startup did not begin")
			// A queued request must not prevent the reader from detecting EOF.
			_, _ = fmt.Fprintln(writer, `{"id":2,"method":"getStatus"}`)
			if signal {
				signals <- syscall.SIGTERM
			} else {
				_ = writer.Close()
			}
			await(t, stopped, "parent shutdown blocked behind engine startup")
			if !service.closed.Load() {
				t.Fatal("shutdown did not clean the in-flight engine")
			}
		})
	}
}

type blockedOutput struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockedOutput) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return len(p), nil
}

func TestShutdownClosesServiceWhileReplyPipeBlocked(t *testing.T) {
	for _, signal := range []bool{false, true} {
		t.Run(fmt.Sprintf("signal=%v", signal), func(t *testing.T) {
			output := &blockedOutput{entered: make(chan struct{}), release: make(chan struct{})}
			service := &fakeService{}
			s := testServer(output, service)
			s.service = &ownedService{coreService: service, cancel: func() {}}
			s.state = "connected"
			input, writer := io.Pipe()
			signals := make(chan os.Signal, 1)
			stopped := make(chan struct{})
			t.Cleanup(func() {
				close(output.release)
				_ = writer.Close()
				_ = input.Close()
				await(t, stopped, "helper did not exit after releasing blocked output")
			})
			go func() { s.serve(input, signals); close(stopped) }()
			_, _ = fmt.Fprintln(writer, `{"id":1,"method":"getStatus"}`)
			await(t, output.entered, "status reply did not reach the blocked pipe")
			if signal {
				signals <- syscall.SIGTERM
			} else {
				_ = writer.Close()
			}
			await(t, stopped, "shutdown cleanup blocked behind a protocol write")
			if !service.closed.Load() || service.closeCalls.Load() != 1 {
				t.Fatal("blocked output prevented service cleanup")
			}
		})
	}
}

func TestStopCancelsStartupAndReturnsSerializedReply(t *testing.T) {
	var output lockedOutput
	started := make(chan struct{})
	service := &fakeService{}
	s := testServer(&output, service)
	s.create = func(ctx context.Context, _ *libbox.DesktopConfig) (coreService, error) {
		service.start = func() error { close(started); <-ctx.Done(); return ctx.Err() }
		return service, nil
	}
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	stopped := make(chan struct{})
	go func() { s.serve(input, make(chan os.Signal)); close(stopped) }()
	_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start","config":"{}"}`)
	await(t, started, "startup did not begin")
	_, _ = fmt.Fprintln(writer, `{"id":2,"method":"stop"}`)
	awaitOutput(t, &output, `"id":2,"result":null`)
	if !service.closed.Load() || !strings.Contains(output.text(), `"state":"disconnected"`) {
		t.Fatal("stop did not release the interrupted engine")
	}
	_ = writer.Close()
	await(t, stopped, "helper did not exit after EOF")
}

func TestCancelledStartupCleanupFailureRequiresHelperReplacement(t *testing.T) {
	for _, construction := range []bool{false, true} {
		t.Run(fmt.Sprintf("construction=%v", construction), func(t *testing.T) {
			var output lockedOutput
			entered := make(chan struct{})
			service := &fakeService{closeErr: errors.New("secret-cancel-close-error")}
			s := testServer(&output, service)
			var creates atomic.Int32
			s.create = func(ctx context.Context, _ *libbox.DesktopConfig) (coreService, error) {
				creates.Add(1)
				if construction {
					close(entered)
					<-ctx.Done()
					return service, ctx.Err()
				}
				service.start = func() error { close(entered); <-ctx.Done(); return ctx.Err() }
				return service, nil
			}
			input, writer := io.Pipe()
			stopped := make(chan struct{})
			t.Cleanup(func() {
				_ = writer.Close()
				_ = input.Close()
				await(t, stopped, "helper did not exit after EOF")
			})
			go func() { s.serve(input, make(chan os.Signal)); close(stopped) }()
			_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start"}`)
			await(t, entered, "startup did not reach the cancellable operation")
			_, _ = fmt.Fprintln(writer, `{"id":2,"method":"stop"}`)
			awaitCleanupReply(t, &output, 2)
			_, _ = fmt.Fprintln(writer, `{"id":3,"method":"start"}`)
			awaitCleanupReply(t, &output, 3)
			_ = writer.Close()
			await(t, stopped, "helper did not exit after EOF")
			if err := s.waitForWorkers(); !errors.Is(err, errCleanupFailed) {
				t.Fatalf("cancelled startup lost cleanup failure: %v", err)
			}
			text := output.text()
			requireCleanupFailure(t, text, 2, 3)
			if creates.Load() != 1 || service.closeCalls.Load() != 1 || strings.Contains(text, `"state":"connected"`) ||
				!slices.Equal(replyIDs(t, text), []int{1, 2, 3}) {
				t.Fatalf("cancelled startup broke ordered terminal cleanup:\n%s", text)
			}
		})
	}
}

func TestStopBeforeStartSuccessCommit(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	op := s.beginStart(context.Background(), request{ID: 1, Method: "start"})
	if op == nil {
		t.Fatal("startup was not accepted")
	}
	owned := &ownedService{coreService: service, cancel: op.cancel}
	if !s.publish(op, owned) || owned.Start() != nil {
		t.Fatal("fixture did not reach the start-success commit boundary")
	}
	// Stop is observed after Start returned nil but before connected/the RPC
	// success are committed. That final commit must validate under the stop lock.
	s.interruptStartup()
	if s.finishStart(op, nil) {
		t.Fatal("an interrupted startup committed success")
	}
	await(t, op.done, "cancelled startup RPC was not released")
	s.replyStart(op)
	s.finishWorker(op)
	s.handle(context.Background(), request{ID: 2, Method: "stop", stopRecorded: true})
	text := output.String()
	if strings.Contains(text, `"state":"connected"`) || strings.Contains(text, `"id":1,"result"`) {
		t.Fatalf("cancelled startup published success:\n%s", text)
	}
	if !slices.Equal(replyIDs(t, text), []int{1, 2}) || strings.Count(text, `"code":"start_failed"`) != 1 {
		t.Fatalf("startup cancellation was not acknowledged exactly once:\n%s", text)
	}
	if s.state != "disconnected" || s.service != nil || service.closeCalls.Load() != 1 {
		t.Fatal("stop did not clean up the cancelled startup exactly once")
	}
}

func TestStopDuringBlockedStartupAllowsControlAndReconnect(t *testing.T) {
	for _, construction := range []bool{false, true} {
		for _, lateError := range []bool{false, true} {
			t.Run(fmt.Sprintf("construction=%v/lateError=%v", construction, lateError), func(t *testing.T) {
				var output lockedOutput
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				var firstStarted atomic.Bool
				var lateErr error
				if lateError {
					lateErr = errors.New("secret-obsolete-start-error")
				}
				first := &fakeService{start: func() error {
					firstStarted.Store(true)
					if !construction {
						close(entered)
						<-release // Deliberately ignores cancellation and Close.
					}
					return lateErr
				}}
				second := &fakeService{}
				s := testServer(&output, first)
				var creates atomic.Int32
				s.create = func(context.Context, *libbox.DesktopConfig) (coreService, error) {
					if creates.Add(1) == 1 {
						if construction {
							close(entered)
							<-release
							return first, lateErr
						}
						return first, nil
					}
					return second, nil
				}
				input, writer := io.Pipe()
				stopped := make(chan struct{})
				var oldOp *startOperation
				t.Cleanup(func() {
					releaseOnce.Do(func() { close(release) })
					_ = writer.Close()
					_ = input.Close()
					await(t, stopped, "helper did not exit after EOF")
					if oldOp != nil {
						await(t, oldOp.finished, "released startup worker did not exit")
					}
				})
				go func() { s.serve(input, make(chan os.Signal)); close(stopped) }()
				_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start","config":"{}"}`)
				await(t, entered, "startup did not reach the blocking operation")
				s.access.Lock()
				oldOp = s.startup
				s.access.Unlock()
				if oldOp == nil {
					t.Fatal("blocked startup lost its operation identity")
				}
				// Queue status before stop, then request status and a new start
				// immediately afterward. All replies must retain request order.
				_, _ = fmt.Fprintln(writer, `{"id":2,"method":"getStatus"}`)
				_, _ = fmt.Fprintln(writer, `{"id":3,"method":"stop"}`)
				_, _ = fmt.Fprintln(writer, `{"id":4,"method":"getStatus"}`)
				_, _ = fmt.Fprintln(writer, `{"id":5,"method":"start","config":"{}"}`)
				_, _ = fmt.Fprintln(writer, `{"id":6,"method":"getStatus"}`)
				awaitOutput(t, &output, `"id":6,"result":{"state":"connected","type":"status"}`)
				beforeRelease := output.text()
				if !slices.Equal(replyIDs(t, beforeRelease), []int{1, 2, 3, 4, 5, 6}) ||
					!strings.Contains(beforeRelease, `"id":3,"result":null`) ||
					!strings.Contains(beforeRelease, `"id":4,"result":{"state":"disconnected","type":"status"}`) ||
					strings.Contains(beforeRelease, `"id":1,"result"`) ||
					strings.Count(beforeRelease, `{"state":"connected","type":"status"}`+"\n") != 1 {
					t.Fatalf("blocked startup broke ordered cancellation/control:\n%s", beforeRelease)
				}
				if oldOp.ctx.Err() == nil || (!construction && !first.closed.Load()) || second.closed.Load() {
					t.Fatal("stop did not cancel/close its own instance while preserving reconnect")
				}
				select {
				case <-oldOp.finished:
					t.Fatal("fixture unexpectedly released the blocked engine operation")
				default:
				}
				// Old success, error and constructor cleanup all happen after the
				// replacement has connected. They must be completely silent.
				releaseOnce.Do(func() { close(release) })
				await(t, oldOp.finished, "released startup worker did not exit")
				if output.text() != beforeRelease || first.closeCalls.Load() != 1 || second.closed.Load() {
					t.Fatalf("obsolete startup changed the replacement or protocol:\n%s", output.text())
				}
				if construction && firstStarted.Load() {
					t.Fatal("cancelled constructor result was started after stop")
				}
				s.access.Lock()
				active := s.state == "connected" && s.service != nil && s.service.coreService == second
				s.access.Unlock()
				if !active {
					t.Fatal("obsolete worker removed the current service")
				}
				_, _ = fmt.Fprintln(writer, `{"id":7,"method":"stop"}`)
				awaitOutput(t, &output, `"id":7,"result":null`)
				if second.closeCalls.Load() != 1 {
					t.Fatal("replacement connection was not independently stoppable")
				}
			})
		}
	}
}

func TestRetiredCleanupFailurePoisonsReplacement(t *testing.T) {
	for _, replacementStarting := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacementStarting=%v", replacementStarting), func(t *testing.T) {
			var output lockedOutput
			entered, release := make(chan struct{}), make(chan struct{})
			replacementEntered := make(chan struct{})
			var releaseOnce sync.Once
			first := &fakeService{closeErr: errors.New("secret-retired-close-error")}
			second := &fakeService{}
			s := testServer(&output, first)
			var creates atomic.Int32
			s.create = func(ctx context.Context, _ *libbox.DesktopConfig) (coreService, error) {
				if creates.Add(1) == 1 {
					close(entered)
					<-release // A constructor that ignores cancellation and returns resources late.
					return first, nil
				}
				if replacementStarting {
					second.start = func() error { close(replacementEntered); <-ctx.Done(); return ctx.Err() }
				}
				return second, nil
			}
			input, writer := io.Pipe()
			stopped := make(chan struct{})
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				_ = writer.Close()
				_ = input.Close()
				await(t, stopped, "helper did not exit after EOF")
				_ = s.waitForWorkers()
			})
			go func() { s.serve(input, make(chan os.Signal)); close(stopped) }()
			_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start"}`)
			await(t, entered, "old constructor did not block")
			s.access.Lock()
			oldOp := s.startup
			s.access.Unlock()
			_, _ = fmt.Fprintln(writer, `{"id":2,"method":"stop"}`)
			awaitOutput(t, &output, `"id":2,"result":null`)
			_, _ = fmt.Fprintln(writer, `{"id":3,"method":"start"}`)
			if replacementStarting {
				await(t, replacementEntered, "replacement startup did not block")
			} else {
				awaitOutput(t, &output, `"id":3,"result":null`)
			}
			s.access.Lock()
			replacement, replacementOp := s.service, s.startup
			s.access.Unlock()
			if replacement == nil || replacement.coreService != second {
				t.Fatal("replacement service was not published")
			}
			releaseOnce.Do(func() { close(release) })
			await(t, oldOp.finished, "retired constructor did not finish cleanup")
			// With an already connected replacement there is no pending RPC: the
			// asynchronous status alone must tell Dart to retire this helper.
			awaitOutput(t, &output, `{"code":"cleanup_failed","message":"`+cleanupFailureMessage+`","state":"error","type":"status"}`)
			if replacementStarting {
				await(t, replacementOp.finished, "terminal failure did not cancel replacement startup")
				awaitCleanupReply(t, &output, 3)
				requireCleanupFailure(t, output.text(), 3)
			} else {
				s.access.Lock()
				preserved := s.service == replacement && s.cleanupFailed && s.state == "error"
				s.access.Unlock()
				if !preserved || second.closed.Load() {
					t.Fatal("stale cleanup silently discarded or closed the active replacement")
				}
			}
			afterFailure := len(output.text())
			_, _ = fmt.Fprintln(writer, `{"id":4,"method":"stop"}`)
			_, _ = fmt.Fprintln(writer, `{"id":5,"method":"start"}`)
			_, _ = fmt.Fprintln(writer, `{"id":6,"method":"initialize"}`)
			_, _ = fmt.Fprintln(writer, `{"id":7,"method":"getStatus"}`)
			awaitOutput(t, &output, `"id":7,"result":{"code":"cleanup_failed"`)
			text := output.text()
			requireCleanupFailure(t, text, 4, 5, 6)
			if strings.Contains(text[afterFailure:], `"state":"connected"`) || strings.Contains(text[afterFailure:], `"state":"disconnected"`) ||
				creates.Load() != 2 || first.closeCalls.Load() != 1 || second.closeCalls.Load() != 1 {
				t.Fatalf("retired cleanup allowed another live connection or false recovery:\n%s", text)
			}
		})
	}
}

func TestShutdownWaitsForRetiredWorkers(t *testing.T) {
	for _, construction := range []bool{false, true} {
		t.Run(fmt.Sprintf("construction=%v", construction), func(t *testing.T) {
			var output lockedOutput
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			first := &fakeService{start: func() error { close(entered); <-release; return nil }}
			second := &fakeService{}
			s := testServer(&output, first)
			var creates atomic.Int32
			s.create = func(context.Context, *libbox.DesktopConfig) (coreService, error) {
				if creates.Add(1) == 1 {
					if construction {
						close(entered)
						<-release
					}
					return first, nil
				}
				return second, nil
			}
			input, writer := io.Pipe()
			stopped := make(chan struct{})
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				_ = writer.Close()
				_ = input.Close()
				await(t, stopped, "helper did not exit after EOF")
				_ = s.waitForWorkers()
			})
			go func() { s.serve(input, make(chan os.Signal)); close(stopped) }()
			_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start"}`)
			await(t, entered, "old startup did not block")
			_, _ = fmt.Fprintln(writer, `{"id":2,"method":"stop"}`)
			awaitOutput(t, &output, `"id":2,"result":null`)
			_, _ = fmt.Fprintln(writer, `{"id":3,"method":"start"}`)
			awaitOutput(t, &output, `"id":3,"result":null`)
			_ = writer.Close()
			await(t, stopped, "serve did not return before the retired worker")
			if !second.closed.Load() {
				t.Fatal("serve did not close the active replacement before final worker drain")
			}
			drained := make(chan error, 1)
			go func() { drained <- s.waitForWorkers() }()
			select {
			case err := <-drained:
				t.Fatalf("shutdown forgot the retired worker: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-drained:
				if err != nil {
					t.Fatalf("successful worker cleanup failed: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("final shutdown did not finish after retired worker cleanup")
			}
			if first.closeCalls.Load() != 1 || second.closeCalls.Load() != 1 {
				t.Fatal("worker drain did not retain independent cleanup ownership")
			}
			s.access.Lock()
			generation := s.stopGeneration
			s.access.Unlock()
			if op := s.beginStart(context.Background(), request{ID: 4, Method: "start", generation: generation}); op != nil {
				op.cancel()
				s.finishWorker(op)
				t.Fatal("final worker drain admitted a new startup")
			}
		})
	}
}

func TestCleanupFailureWorkerDrainDoesNotDependOnOutput(t *testing.T) {
	output := &blockedOutput{entered: make(chan struct{}), release: make(chan struct{})}
	closed := make(chan struct{})
	service := &fakeService{
		startErr: errors.New("secret-start-error"), closeErr: errors.New("secret-close-error"),
		onClose: func() { close(closed) },
	}
	s := testServer(output, service)
	input, writer := io.Pipe()
	stopped := make(chan struct{})
	t.Cleanup(func() {
		close(output.release)
		_ = writer.Close()
		_ = input.Close()
		await(t, stopped, "helper did not exit after releasing output")
	})
	go func() { s.serve(input, make(chan os.Signal)); close(stopped) }()
	_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start"}`)
	await(t, output.entered, "startup did not reach blocked output")
	await(t, closed, "blocked output prevented startup cleanup")
	_ = writer.Close()
	await(t, stopped, "terminal cleanup blocked on its notification output")
	drained := make(chan error, 1)
	go func() { drained <- s.waitForWorkers() }()
	select {
	case err := <-drained:
		if !errors.Is(err, errCleanupFailed) {
			t.Fatalf("blocked output hid cleanup failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failure notification blocked the worker drain")
	}
}

// This controlled, credential-free fixture runs only in a disposable Linux
// network namespace. It keeps detailed engine errors out of the production IPC.
func TestRealLinuxTun(t *testing.T) {
	if os.Getenv("SINGBOX_TEST_TUN") != "1" {
		t.Skip("requires an isolated network namespace with TUN permissions")
	}
	data := t.TempDir()
	if err := libbox.Setup(&libbox.SetupOptions{BasePath: data, WorkingPath: data, TempPath: data}); err != nil {
		t.Fatal(err)
	}
	instance, err := libbox.NewDesktopService(context.Background(), `{"dns":{"servers":[{"type":"udp","tag":"dns-direct","server":"1.1.1.1"}],"final":"dns-direct"},"inbounds":[{"type":"tun","interface_name":"sbf-gotest","address":["172.29.199.1/30"],"auto_route":true,"stack":"system"}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"auto_detect_interface":true,"final":"direct"}}`)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err = instance.Start(); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeRequiredAndConfigRedaction(t *testing.T) {
	var output bytes.Buffer
	s := testServer(&output, &fakeService{})
	s.initialized = false
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if !strings.Contains(output.String(), "not_initialized") {
		t.Fatal("start must require initialization")
	}
	s.handle(context.Background(), request{ID: 2, Method: "initialize"})
	s.handle(context.Background(), request{ID: 3, Method: "checkConfig", Config: `{"outbounds":[{"type":"secret-auth-key"}]}`})
	if !strings.Contains(output.String(), "invalid_config") || strings.Contains(output.String(), "secret-auth-key") {
		t.Fatal("config errors must be sanitized")
	}
}
