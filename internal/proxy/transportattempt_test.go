package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

type cancelingRequestSigner struct{ cancel context.CancelFunc }

func (s cancelingRequestSigner) Sign(ctx context.Context, _ *http.Request) error {
	s.cancel()
	return ctx.Err()
}

type transportchainRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f transportchainRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBuildUpstreamTransportDoesNotMarkAttemptBeforeSigning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &upstreamAttemptState{}
	ctx = withUpstreamAttemptState(ctx, state)
	ctx = transcode.WithRequestSigner(ctx, cancelingRequestSigner{cancel: cancel})
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil).WithContext(ctx)

	var upstreamCalled bool
	transport := buildUpstreamTransport(&proxyConfig{
		transport: transportchainRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			upstreamCalled = true
			return nil, errors.New("unexpected upstream call")
		}),
	})
	_, err := transport.RoundTrip(req)

	if err == nil {
		t.Fatal("RoundTrip error = nil, want signing cancellation")
	}
	if state.started.Load() {
		t.Fatal("upstream attempt marked before signing completed")
	}
	if upstreamCalled {
		t.Fatal("upstream transport called after signing cancellation")
	}
}
