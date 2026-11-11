// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux && !android

package libbox

import (
	"context"
	"errors"
	"sync"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service/pause"
)

// DesktopService uses sing-box's native Linux TUN, routing and network monitor.
// In particular, it must not install the Android/iOS PlatformInterface wrapper.
type DesktopService struct {
	box       *box.Box
	ctx       context.Context
	cancel    context.CancelFunc
	cleanup   *adapter.Scope
	closeOnce sync.Once
	closeErr  error
}

// CheckDesktopConfig validates the effective Linux options without creating a
// TUN. Desktop logs are disabled, so mobile private-file policy does not apply.
func CheckDesktopConfig(content string) error {
	ctx, cancel := context.WithCancel(mobileBaseContext(nil))
	defer cancel()
	ctx = pause.WithDefaultManager(ctx)
	options, err := parseConfig(ctx, content)
	if err != nil {
		return err
	}
	return checkConfig(ctx, options)
}

// DesktopConfig retains decoded options between the TUN permission check and
// engine construction, so a start request only decodes and gates its JSON once.
type DesktopConfig struct {
	options option.Options
}

func ParseDesktopConfig(content string) (*DesktopConfig, error) {
	options, err := parseConfig(mobileBaseContext(nil), content)
	if err != nil {
		return nil, errors.New("invalid Linux configuration")
	}
	return &DesktopConfig{options: options}, nil
}

func (c *DesktopConfig) HasTunInbound() bool {
	return hasTunInbound(c.options)
}

func NewDesktopService(parent context.Context, content string) (*DesktopService, error) {
	config, err := ParseDesktopConfig(content)
	if err != nil {
		return nil, err
	}
	return NewDesktopServiceWithConfig(parent, config)
}

func NewDesktopServiceWithConfig(parent context.Context, config *DesktopConfig) (*DesktopService, error) {
	if config == nil {
		return nil, errors.New("invalid Linux configuration")
	}
	ctx, cancel := context.WithCancel(mobileBaseContext(nil))
	stopParent := context.AfterFunc(parent, cancel)
	cancelAll := func() { stopParent(); cancel() }
	ctx = pause.WithDefaultManager(ctx)
	options := config.options
	ctx, cleanup := withConstructorCleanup(ctx)
	// Upstream logs can expose authentication URLs and credentials. Keep the
	// control protocol and stderr free of raw engine diagnostics.
	options.Log = &option.LogOptions{Disabled: true}
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil || ctx.Err() != nil {
		cancelAll()
		if instance != nil {
			_ = instance.Close()
		}
		_ = cleanup.Close()
		closeHistory(ctx)
		return nil, errors.New("could not create Linux service")
	}
	return &DesktopService{box: instance, ctx: ctx, cancel: cancelAll, cleanup: cleanup}, nil
}

func (s *DesktopService) Start() error { return s.box.Start() }

// Close may interrupt Start: the box scope synchronizes cleanup registration,
// and cancellation prevents later startup stages. A parent exit must not wait
// for an upstream HTTP download that ignores its request context.
func (s *DesktopService) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.closeErr = s.box.Close()
		if err := s.cleanup.Close(); s.closeErr == nil {
			s.closeErr = err
		}
		closeHistory(s.ctx)
	})
	return s.closeErr
}
