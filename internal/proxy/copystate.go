// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package proxy

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

type copyErrorStateKeyType struct{}

var copyErrorStateKey = copyErrorStateKeyType{}

type upstreamAttemptStateKeyType struct{}

var upstreamAttemptStateKey = upstreamAttemptStateKeyType{}

type copyErrorState struct {
	mu              sync.Mutex
	upstreamReadErr error
}

func withCopyErrorState(ctx context.Context, state *copyErrorState) context.Context {
	return context.WithValue(ctx, copyErrorStateKey, state)
}

func copyErrorStateFromContext(ctx context.Context) *copyErrorState {
	state, _ := ctx.Value(copyErrorStateKey).(*copyErrorState)
	return state
}

type upstreamAttemptState struct {
	started atomic.Bool
}

func withUpstreamAttemptState(ctx context.Context, state *upstreamAttemptState) context.Context {
	return context.WithValue(ctx, upstreamAttemptStateKey, state)
}

func upstreamAttemptStateFromContext(ctx context.Context) *upstreamAttemptState {
	state, _ := ctx.Value(upstreamAttemptStateKey).(*upstreamAttemptState)
	return state
}

func (s *copyErrorState) recordUpstreamReadError(err error) {
	if s == nil || err == nil || errors.Is(err, io.EOF) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upstreamReadErr == nil {
		s.upstreamReadErr = err
	}
}

func (s *copyErrorState) upstreamError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upstreamReadErr
}

type trackingReadCloser struct {
	io.ReadCloser
	state *copyErrorState
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.state.recordUpstreamReadError(err)
	return n, err
}

type trackingReadWriteCloser struct {
	io.ReadWriteCloser
	state *copyErrorState
}

func (b *trackingReadWriteCloser) Read(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Read(p)
	b.state.recordUpstreamReadError(err)
	return n, err
}

type closeWriter interface {
	CloseWrite() error
}

type trackingReadWriteCloseWriter struct {
	io.ReadWriteCloser
	closeWriter
	state *copyErrorState
}

func (b *trackingReadWriteCloseWriter) Read(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Read(p)
	b.state.recordUpstreamReadError(err)
	return n, err
}
