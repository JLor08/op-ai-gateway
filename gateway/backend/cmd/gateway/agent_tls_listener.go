// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sniffPeekTimeout        = 5 * time.Second
	sniffMaxPending         = 256
	sniffAcceptRetryInitial = 5 * time.Millisecond
	sniffAcceptRetryMax     = time.Second
)

type certHolder struct {
	current atomic.Pointer[tls.Certificate]
}

func (h *certHolder) StorePEM(fullchainPEM, keyPEM string) error {
	cert, err := tls.X509KeyPair([]byte(fullchainPEM), []byte(keyPEM))
	if err != nil {
		return err
	}
	h.current.Store(&cert)
	return nil
}

func (h *certHolder) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := h.current.Load()
	if cert == nil {
		return nil, errors.New("mesh TLS certificate unavailable")
	}
	return cert, nil
}

type sniffOptions struct {
	PeekTimeout time.Duration
	MaxPending  int
}

func newSniffListener(raw net.Listener, tlsConfig *tls.Config) net.Listener {
	return newSniffListenerWithOptions(raw, tlsConfig, sniffOptions{
		PeekTimeout: sniffPeekTimeout,
		MaxPending:  sniffMaxPending,
	})
}

func newSniffListenerWithOptions(raw net.Listener, tlsConfig *tls.Config, opts sniffOptions) net.Listener {
	l := &sniffListener{
		raw:       raw,
		tlsConfig: tlsConfig,
		opts:      opts,
		ready:     make(chan sniffResult, opts.MaxPending),
		slots:     make(chan struct{}, opts.MaxPending),
		done:      make(chan struct{}),
		pending:   make(map[net.Conn]struct{}),
	}
	go l.acceptLoop()
	return l
}

type sniffListener struct {
	raw       net.Listener
	tlsConfig *tls.Config
	opts      sniffOptions

	ready chan sniffResult
	slots chan struct{}
	done  chan struct{}

	shutdownOnce sync.Once
	closeReady   sync.Once
	workers      sync.WaitGroup

	mu          sync.Mutex
	closed      bool
	terminalErr error
	pending     map[net.Conn]struct{}
}

type sniffResult struct {
	conn net.Conn
	raw  net.Conn
}

func (l *sniffListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, l.acceptError()
	default:
	}

	select {
	case <-l.done:
		return nil, l.acceptError()
	case result, ok := <-l.ready:
		if !ok {
			return nil, l.acceptError()
		}
		if !l.claimPending(result.raw) {
			return nil, l.acceptError()
		}
		select {
		case <-l.done:
			_ = result.conn.Close()
			return nil, l.acceptError()
		default:
			return result.conn, nil
		}
	}
}

func (l *sniffListener) Close() error {
	l.shutdown(net.ErrClosed)
	return nil
}

func (l *sniffListener) Addr() net.Addr {
	return l.raw.Addr()
}

func (l *sniffListener) acceptLoop() {
	defer func() {
		l.workers.Wait()
		l.closeReady.Do(func() { close(l.ready) })
	}()

	var retryDelay time.Duration
	for {
		conn, err := l.raw.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() { //nolint:staticcheck // net.Error.Temporary retained for accept-loop backoff
				if retryDelay == 0 {
					retryDelay = sniffAcceptRetryInitial
				} else {
					retryDelay *= 2
				}
				if retryDelay > sniffAcceptRetryMax {
					retryDelay = sniffAcceptRetryMax
				}
				if !l.waitAcceptRetry(retryDelay) {
					return
				}
				continue
			}
			l.shutdown(err)
			return
		}
		retryDelay = 0
		// Backpressure, NOT drop-newcomer: when every pending-peek slot is taken,
		// pause the accept loop until one frees instead of closing the connection we
		// just accepted. Closing it would selectively punish a (possibly legitimate)
		// agent while a flood of silent connections holds the slots -- the very
		// availability lever the cap is meant to prevent. Overflow connections wait
		// in the kernel backlog, which throttles every peer equally. Only a shutdown
		// aborts the wait.
		select {
		case l.slots <- struct{}{}:
		case <-l.done:
			_ = conn.Close()
			return
		}
		if !l.track(conn) {
			<-l.slots
			_ = conn.Close()
			continue
		}
		go l.classify(conn)
	}
}

func (l *sniffListener) waitAcceptRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	select {
	case <-timer.C:
		return true
	case <-l.done:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return false
	}
}

func (l *sniffListener) classify(conn net.Conn) {
	defer l.workers.Done()
	defer func() { <-l.slots }()

	if err := conn.SetReadDeadline(time.Now().Add(l.opts.PeekTimeout)); err != nil {
		l.closePending(conn)
		return
	}
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		l.closePending(conn)
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		l.closePending(conn)
		return
	}

	replayed := &replayConn{
		Conn:   conn,
		reader: io.MultiReader(bytes.NewReader(first[:]), conn),
	}
	var classified net.Conn = replayed
	if first[0] == 0x16 {
		classified = tls.Server(replayed, l.tlsConfig)
	}

	select {
	case <-l.done:
		l.closePending(conn)
	case l.ready <- sniffResult{conn: classified, raw: conn}:
	}
}

func (l *sniffListener) track(conn net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.workers.Add(1)
	l.pending[conn] = struct{}{}
	return true
}

func (l *sniffListener) claimPending(conn net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.pending[conn]; !ok {
		return false
	}
	delete(l.pending, conn)
	return true
}

func (l *sniffListener) closePending(conn net.Conn) {
	if l.claimPending(conn) {
		_ = conn.Close()
	}
}

func (l *sniffListener) shutdown(err error) {
	l.shutdownOnce.Do(func() {
		if err == nil {
			err = net.ErrClosed
		}

		l.mu.Lock()
		l.closed = true
		l.terminalErr = err
		close(l.done)
		pending := make([]net.Conn, 0, len(l.pending))
		for conn := range l.pending {
			pending = append(pending, conn)
			delete(l.pending, conn)
		}
		l.mu.Unlock()

		_ = l.raw.Close()
		for _, conn := range pending {
			_ = conn.Close()
		}
	})
}

func (l *sniffListener) acceptError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminalErr == nil {
		return net.ErrClosed
	}
	return l.terminalErr
}

type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
