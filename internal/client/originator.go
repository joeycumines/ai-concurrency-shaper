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

// Package client provides originator identification and client state tracking
// for smarter failure handling in the proxy.
package client

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// Originator identifies the entity that initiated an HTTP request.
// It combines network-level, TLS, and process-level information so
// that CEL policies can target specific clients or application groups.
type Originator struct {
	// RemoteAddr is the raw network address of the client.
	RemoteAddr string
	// IP is the resolved IP address string (host portion of RemoteAddr).
	IP string
	// PID is the process ID of the local process that originated the
	// request. Zero when the request is not from a local process.
	PID int
	// AppName is the identified application name for the originator.
	// For local processes, this is filled in by the heuristic system.
	AppName string
	// IsLocal indicates whether the request originated from a local
	// process (e.g., a UNIX socket connection or loopback interface).
	IsLocal bool
	// TLSCertSubject is the subject from the client TLS certificate,
	// populated when mTLS or client certificate authentication is used.
	TLSCertSubject string
}

// Fingerprint returns a stable string key for the originator.
// For local processes, the PID is included. For remote clients,
// the IP address is used. For local UNIX sockets without a PID,
// the raw RemoteAddr is used to avoid collapsing all anonymous
// local connections into a single tracking bucket.
func (o Originator) Fingerprint() string {
	if o.IsLocal && o.PID > 0 {
		return "pid:" + strconv.Itoa(o.PID)
	}
	ip := o.IP
	if ip == "" {
		ip = o.RemoteAddr
	}
	return "ip:" + ip
}

// ExtractOriginator extracts the Originator from an HTTP request.
// It inspects the request's TLS state, remote address, and headers
// to determine the identity and locality of the client.
func ExtractOriginator(req *http.Request) Originator {
	o := Originator{
		RemoteAddr: req.RemoteAddr,
	}

	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if err == nil {
		o.IP = ip
	}

	// Check for TLS peer certificate (mTLS / client cert auth).
	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
		cert := req.TLS.PeerCertificates[0]
		o.TLSCertSubject = cert.Subject.String()
	}

	// Determine if the request is from a local process.
	o.IsLocal = isLocalIP(o.IP) || isLocalAddr(req.RemoteAddr)

	// If the connection is from a local UNIX socket, try to extract
	// the PID from the SO_PEERCRED socket option.
	if o.IsLocal {
		o.PID, o.AppName = extractLocalProcessInfo(req)
	}

	return o
}

// isLocalIP returns true for loopback addresses only.
// Private RFC1918 addresses (10.x, 172.16-31.x, 192.168.x) are
// not local — they can belong to remote clients on the same LAN.
func isLocalIP(ip string) bool {
	if ip == "localhost" {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return parsed.IsLoopback()
}

// isLocalAddr returns true for UNIX socket or other local-only addresses.
func isLocalAddr(addr string) bool {
	return strings.HasPrefix(addr, "@") || strings.HasPrefix(addr, "/")
}

// extractLocalProcessInfo attempts to identify the local process that
// originated the request. The actual PID extraction mechanism depends
// on the transport and OS; this function provides the hook point where
// heuristic identification will occur once the PID is known.
func extractLocalProcessInfo(req *http.Request) (int, string) {
	// PID extraction from connection credentials is transport-dependent.
	// On Linux, SO_PEERCRED provides the PID for UNIX sockets.
	// On macOS, LOCAL_PEERCRED provides similar info.
	// The heuristic system uses the PID to identify the application.
	// Currently, without OS-specific syscalls integrated at the
	// transport level, the PID cannot be extracted from the request.
	// This function provides the structural hook for future integration.
	return 0, ""
}

// SetLocalProcessInfo sets the PID and AppName on the Originator
// when the process identity is known (e.g., from a transport-level
// credential extraction). This is called by the proxy when it has
// successfully identified the local process behind the request.
func SetLocalProcessInfo(o *Originator, pid int, appName string) {
	o.PID = pid
	o.AppName = appName
	if pid > 0 {
		o.IsLocal = true
	}
}
