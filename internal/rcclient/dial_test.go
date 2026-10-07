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

package rcclient

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// startEchoServer runs a TCP echo listener for the duration of the test.
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln
}

// roundTrip dials address through dialer and verifies a byte string echoes back.
func roundTrip(t *testing.T, d *Dialer, address string) {
	t.Helper()
	conn, err := d.DialContext(context.Background(), "tcp", address)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	msg := []byte("hello via RemoteControl.NetConn")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: got %q want %q", buf, msg)
	}
}

func TestDialerRoundTripLegacy(t *testing.T) {
	echoLn := startEchoServer(t)
	echoAddr := echoLn.Addr().String()

	srv, err := NewServer(ServerConfig{Bind: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	d, err := NewDialer(ChannelConfig{Server: srv.Addr().String()}, DialConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if d.RemoteTerminate() {
		t.Error("RemoteTerminate: want false for a plaintext dial config")
	}

	roundTrip(t, d, echoAddr)
}

func TestDialerRoundTripFlowControl(t *testing.T) {
	echoLn := startEchoServer(t)
	echoAddr := echoLn.Addr().String()

	srv, err := NewServer(ServerConfig{Bind: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	d, err := NewDialer(ChannelConfig{Server: srv.Addr().String()}, DialConfig{FlowControl: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	roundTrip(t, d, echoAddr)
}
