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

// TestDispatchEncodedSlashAndDotSegments pins the decode-then-match invariant
// of prefix mounting: url.Parse has already percent-decoded r.URL.Path before
// the router sees it, and matching runs on those DECODED, traversal-resolved
// segments. Consequently:
//
//   - an encoded separator (%2F) splits segments exactly like a literal "/";
//   - an encoded dot-dot (%2E%2E) resolves exactly like a literal "..";
//   - a request whose RESOLVED path escapes its apparent mount never aliases
//     a sibling provider — it either resolves under the sibling (correct
//     dispatch by resolved identity) or 404s;
//   - the forwarded path is rebuilt from the resolved remainder (RawPath is
//     cleared), so upstreams always observe fully resolved paths.
//
// These cases pin CURRENT correct behavior as the contract; if any assertion
// fails against future changes, the change broke containment or silently
// altered mount semantics.
func TestDispatchEncodedSlashAndDotSegments(t *testing.T) {
	acme, pa := echo(t)
	beta, pb := echo(t)

	h, err := New([]Provider{
		{Name: "acme", Prefix: "/acme", Proxy: acme},
		{Name: "beta", Prefix: "/beta", Proxy: beta},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name       string
		target     string
		wantStatus int
		wantA      string // path observed by the acme upstream ("" = not hit)
		wantB      string // path observed by the beta upstream ("" = not hit)
	}{
		{
			// The encoded slash is a real separator post-decode: the request
			// lands on acme with a resolved remainder.
			name:       "encoded slash splits under mount",
			target:     "http://example.com/acme/foo%2Fbar",
			wantStatus: http.StatusNoContent,
			wantA:      "/foo/bar",
		},
		{
			// Encoded dots resolve like literal dots: escape above the mount
			// yields a path outside every mount => 404, never a sibling hit.
			name:       "encoded dot-dot escaping mount 404s",
			target:     "http://example.com/acme/%2E%2E/escape",
			wantStatus: http.StatusNotFound,
		},
		{
			// A hostile client encoding a SIBLING prefix plus traversal
			// resolves to a genuine /acme path: dispatch correctly follows
			// the resolved identity (this documents why resolution-before-
			// match cannot be smuggled into a wrong provider).
			name:       "resolved identity wins over wire spelling",
			target:     "http://example.com/beta%2F..%2F..%2Facme%2Fx",
			wantStatus: http.StatusNoContent,
			wantA:      "/x",
		},
		{
			name:       "dot-dot inside remainder resolves within mount",
			target:     "http://example.com/acme/a/%2E%2E/b",
			wantStatus: http.StatusNoContent,
			wantA:      "/b",
		},
		{
			// RawPath is cleared, so the proxy re-encodes from the decoded
			// Path: no double-encoding artifacts reach the upstream.
			name:       "trailing slash parity with bare mount",
			target:     "http://example.com/acme/",
			wantStatus: http.StatusNoContent,
			wantA:      "/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa.mu.Lock()
			pa.path = ""
			pa.mu.Unlock()
			pb.mu.Lock()
			pb.path = ""
			pb.mu.Unlock()

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := pa.get(); got != tt.wantA {
				t.Errorf("acme upstream saw %q, want %q", got, tt.wantA)
			}
			if got := pb.get(); got != tt.wantB {
				t.Errorf("beta upstream saw %q, want %q", got, tt.wantB)
			}
		})
	}
}

// TestRouter_DoesNotMutateInboundRequest proves that delegating through router.ServeHTTP
// does not mutate the caller's *http.Request or *url.URL pointers or fields (review-16 finding 1).
func TestRouter_DoesNotMutateInboundRequest(t *testing.T) {
	up, _ := echo(t)

	// 1. Prefixed provider
	{
		h, err := New([]Provider{{Name: "acme", Prefix: "/acme", Proxy: up}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://example.com/acme/v1/messages", nil)
		req.URL.RawPath = "/acme/v1/messages"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if req.URL.Path != "/acme/v1/messages" {
			t.Errorf("prefixed mount mutated caller's req.URL.Path: got %q, want %q", req.URL.Path, "/acme/v1/messages")
		}
		if req.URL.RawPath != "/acme/v1/messages" {
			t.Errorf("prefixed mount mutated caller's req.URL.RawPath: got %q, want %q", req.URL.RawPath, "/acme/v1/messages")
		}
	}

	// 2. Bare provider
	{
		h, err := New([]Provider{{Name: "bare", Prefix: "", Proxy: up}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://example.com/v1/messages/", nil)
		req.URL.RawPath = "/v1/messages/"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if req.URL.Path != "/v1/messages/" {
			t.Errorf("bare mount mutated caller's req.URL.Path: got %q, want %q", req.URL.Path, "/v1/messages/")
		}
		if req.URL.RawPath != "/v1/messages/" {
			t.Errorf("bare mount mutated caller's req.URL.RawPath: got %q, want %q", req.URL.RawPath, "/v1/messages/")
		}
	}
}
