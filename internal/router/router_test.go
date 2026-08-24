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

package router

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// pathProbe records the last r.URL.Path an upstream observed.
type pathProbe struct {
	mu   sync.Mutex
	path string
}

func (p *pathProbe) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.path
}

// echo returns an httptest upstream that echoes the request path into the
// probe, plus a reverse-proxy handler for it (so the router can delegate to it
// the same way main.go does).
func echo(t *testing.T) (http.Handler, *pathProbe) {
	t.Helper()
	probe := &pathProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.mu.Lock()
		probe.path = r.URL.Path
		probe.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream %q: %v", srv.URL, err)
	}
	return httputil.NewSingleHostReverseProxy(u), probe
}

func TestDispatch(t *testing.T) {
	acme, pa := echo(t)
	acme2, pb := echo(t)

	h, err := New([]Provider{
		{Name: "acme", Prefix: "/acme", Proxy: acme},
		{Name: "acme2", Prefix: "/acme2", Proxy: acme2},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// True mount: /acme/v1/messages reaches the acme upstream as /v1/messages.
	if r, err := http.Get(srv.URL + "/acme/v1/messages"); err != nil {
		t.Fatal(err)
	} else if r.StatusCode != http.StatusNoContent {
		t.Fatalf("acme status %d", r.StatusCode)
	}
	if got := pa.get(); got != "/v1/messages" {
		t.Errorf("acme upstream saw %q, want %q", got, "/v1/messages")
	}

	// /acme2/... must route to the second provider, not be absorbed by /acme.
	if r, err := http.Get(srv.URL + "/acme2/chat"); err != nil {
		t.Fatal(err)
	} else if r.StatusCode != http.StatusNoContent {
		t.Fatalf("acme2 status %d", r.StatusCode)
	}
	if got := pb.get(); got != "/chat" {
		t.Errorf("acme2 upstream saw %q, want %q", got, "/chat")
	}

	// /acmeextra shares only a string prefix, not a segment prefix: 404.
	if r, err := http.Get(srv.URL + "/acmeextra"); err != nil {
		t.Fatal(err)
	} else if r.StatusCode != http.StatusNotFound {
		t.Errorf("/acmeextra status %d, want 404", r.StatusCode)
	}

	// Unmatched mount: 404.
	if r, err := http.Get(srv.URL + "/nomatch/x"); err != nil {
		t.Fatal(err)
	} else if r.StatusCode != http.StatusNotFound {
		t.Errorf("/nomatch/x status %d, want 404", r.StatusCode)
	}
}

func TestBarePassThrough(t *testing.T) {
	up, probe := echo(t)

	h, err := New([]Provider{{Name: "", Prefix: "", Proxy: up}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// A bare single provider is a transparent pass-through: the path is not
	// stripped.
	if r, err := http.Get(srv.URL + "/v1/messages"); err != nil {
		t.Fatal(err)
	} else if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", r.StatusCode)
	}
	if got := probe.get(); got != "/v1/messages" {
		t.Errorf("upstream saw %q, want %q", got, "/v1/messages")
	}
}

func TestNewRejectsOverlap(t *testing.T) {
	up, _ := echo(t)
	_, err := New([]Provider{
		{Name: "a", Prefix: "/acme", Proxy: up},
		{Name: "b", Prefix: "/acme/v1", Proxy: up},
	})
	if err == nil {
		t.Fatal("expected overlap error")
	}
	if got := err.Error(); !strings.Contains(got, "overlap") {
		t.Errorf("error should mention overlap, got %q", got)
	}
}

func TestNewRejectsMultipleBare(t *testing.T) {
	up, _ := echo(t)
	_, err := New([]Provider{
		{Name: "a", Prefix: "", Proxy: up},
		{Name: "b", Prefix: "", Proxy: up},
	})
	if err == nil {
		t.Fatal("expected multiple-bare error")
	}
	if got := err.Error(); !strings.Contains(got, "bare root") {
		t.Errorf("error should mention bare root, got %q", got)
	}
}

func TestNewRejectsNilProxy(t *testing.T) {
	_, err := New([]Provider{{Name: "a", Prefix: "/acme", Proxy: nil}})
	if err == nil {
		t.Fatal("expected nil-handler error")
	}
}

func TestSinglePrefixedProvider(t *testing.T) {
	up, probe := echo(t)

	h, err := New([]Provider{{Name: "acme", Prefix: "/acme", Proxy: up}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	if r, err := http.Get(srv.URL + "/acme/v1/messages"); err != nil {
		t.Fatal(err)
	} else if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", r.StatusCode)
	}
	if got := probe.get(); got != "/v1/messages" {
		t.Errorf("upstream saw %q, want %q", got, "/v1/messages")
	}

	if r, err := http.Get(srv.URL + "/unmounted"); err != nil {
		t.Fatal(err)
	} else if r.StatusCode != http.StatusNotFound {
		t.Errorf("/unmounted status %d, want 404", r.StatusCode)
	}
}

func TestNewRejectsNilProxyUnnamed(t *testing.T) {
	// A nil handler with no name falls back to quoting the prefix in the
	// error message.
	_, err := New([]Provider{{Name: "", Prefix: "/acme", Proxy: nil}})
	if err == nil {
		t.Fatal("expected nil-handler error")
	}
	if got := err.Error(); !strings.Contains(got, `"/acme"`) {
		t.Errorf("error should quote the unnamed prefix, got %q", got)
	}
}

func TestDispatchShortRequestPath(t *testing.T) {
	// A request with fewer segments than a mount's prefix cannot match it
	// (e.g. GET /acme against a /acme/v1 mount): 404, upstream untouched.
	up, probe := echo(t)

	h, err := New([]Provider{{Name: "a", Prefix: "/acme/v1", Proxy: up}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/acme", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/acme against /acme/v1 mount: status %d, want 404", rec.Code)
	}
	if got := probe.get(); got != "" {
		t.Errorf("upstream should not have been hit, saw %q", got)
	}
}

func TestMountRootPath(t *testing.T) {
	// A request equal to the mount prefix strips to the root path "/": the
	// suffix is empty, so joinSegments maps it to a single leading slash.
	up, probe := echo(t)

	h, err := New([]Provider{{Name: "a", Prefix: "/acme", Proxy: up}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/acme", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	if got := probe.get(); got != "/" {
		t.Errorf("upstream saw %q, want %q", got, "/")
	}
}

func TestNewRejectsOverlapLongerFirst(t *testing.T) {
	// The same segment-wise overlap with the LONGER prefix registered first:
	// prefixesOverlap must swap before comparing, or the longer-first order
	// would slip through the check.
	up, _ := echo(t)
	_, err := New([]Provider{
		{Name: "a", Prefix: "/acme/v1", Proxy: up},
		{Name: "b", Prefix: "/acme", Proxy: up},
	})
	if err == nil {
		t.Fatal("expected overlap error with longer prefix registered first")
	}
	if got := err.Error(); !strings.Contains(got, "overlap") {
		t.Errorf("error should mention overlap, got %q", got)
	}
}

func TestTraversalResolution(t *testing.T) {
	// The dispatcher resolves ".." before matching, so a request that tries to
	// escape a mount does not alias a sibling mount.
	a, pa := echo(t)
	b, pb := echo(t)

	h, err := New([]Provider{
		{Name: "a", Prefix: "/a", Proxy: a},
		{Name: "b", Prefix: "/b", Proxy: b},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/a/x/../y", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	if got := pa.get(); got != "/y" {
		t.Errorf("a upstream saw %q, want %q", got, "/y")
	}
	if got := pb.get(); got != "" {
		t.Errorf("b upstream should not have been hit, saw %q", got)
	}
}
