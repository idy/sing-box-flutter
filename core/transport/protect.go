package transport

import (
	"context"
	"errors"
	"runtime"
	"syscall"
)

// SocketProtector exempts an outer socket from the host VPN before bind/connect.
// The fd is borrowed only for the duration of the call: do not close, retain or
// duplicate it. Return an error if protection fails. Calls may be concurrent.
// On Android this must call VpnService.protect(fd), treating false as failure.
// Protection applies to outer transport sockets, never the supplied TUN fd.
type SocketProtector func(fd int) error

var (
	ErrSocketProtectorRequired = errors.New("Android requires a socket protector")
	ErrSocketProtection        = errors.New("socket protection failed")
)

type socketProtectorKey struct{}

// WithSocketProtector installs protection for this session/engine and for every
// custom factory and wrapper receiving its context. It does not take ownership
// of the callback or its resources. The host must keep them alive until Close.
func WithSocketProtector(ctx context.Context, protector SocketProtector) context.Context {
	return context.WithValue(ctx, socketProtectorKey{}, protector)
}

func socketProtector(ctx context.Context) SocketProtector {
	protector, _ := ctx.Value(socketProtectorKey{}).(SocketProtector)
	return protector
}

func HasSocketProtector(ctx context.Context) bool { return socketProtector(ctx) != nil }

// ProtectSocket protects a borrowed, newly-created outer socket before any bind,
// connect or send. It never closes fd. It is a no-op without a protector on
// non-Android hosts; Android fails closed. Errors from the host are redacted.
// Prefer SocketControl for net.Dialer and net.ListenConfig, which close their
// own socket when control fails. Raw-socket factories must close on failure.
func ProtectSocket(ctx context.Context, fd int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fd < 0 {
		return ErrSocketProtection
	}
	protector := socketProtector(ctx)
	if protector == nil {
		if runtime.GOOS == "android" {
			return ErrSocketProtectorRequired
		}
		return nil
	}
	if err := protector(fd); err != nil {
		return ErrSocketProtection
	}
	return nil
}

// SocketControl returns a pre-bind/pre-connect control function suitable for
// net.Dialer.Control and net.ListenConfig.Control. Factories/wrappers that open
// additional sockets must install it on every dialer/listener (including DNS,
// reconnects and auxiliary sockets), or use ProtectSocket for raw descriptors.
// It returns nil when no protection is needed on a non-Android host. Compose it
// with any existing control function; never overwrite required protection.
func SocketControl(ctx context.Context) func(string, string, syscall.RawConn) error {
	if !HasSocketProtector(ctx) && runtime.GOOS != "android" {
		return nil
	}
	return func(_, _ string, conn syscall.RawConn) error {
		if conn == nil {
			return ErrSocketProtection
		}
		var protectErr error
		if err := conn.Control(func(fd uintptr) {
			protectErr = ProtectSocket(ctx, int(fd))
		}); err != nil {
			return ErrSocketProtection
		}
		return protectErr
	}
}
