package a2a

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A rendezvous read that answers quickly but streams its body for longer than the client's
// HTTP / ShortHTTP overall timeouts must still complete: those timeouts used to cut every
// window slower than ~1 MB/s at the same byte (DefaultPollTimeout includes the body).
func TestRendezvousGetSlowBodyOutlivesClientTimeouts(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	body := []byte(fmt.Sprintf(`{"items":[{"seq":7,"data":%q}]}`, base64.StdEncoding.EncodeToString(payload)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fl.Flush()
		// ~150 ms of dribbling: well past the 60 ms client timeouts set below.
		const parts = 10
		step := len(body)/parts + 1
		for i := 0; i < len(body); i += step {
			_, _ = w.Write(body[i:min(i+step, len(body))])
			fl.Flush()
			time.Sleep(15 * time.Millisecond)
		}
	}))
	defer srv.Close()

	for _, wait := range []int{0, 5} { // 0 used ShortHTTP, >0 used HTTP before the fix
		pc := NewProxyClient(srv.URL, nil).WithDeliverTimeout(60 * time.Millisecond)
		pc.HTTP.Timeout = 60 * time.Millisecond
		items, err := pc.RendezvousGet(context.Background(), "pair-code-0123456789", 0, wait)
		if err != nil {
			t.Fatalf("wait=%d: slow body must be read to the end: %v", wait, err)
		}
		if len(items) != 1 || items[0].Seq != 7 || !bytes.Equal(items[0].Data, payload) {
			t.Fatalf("wait=%d: wrong items: %d items", wait, len(items))
		}
	}
}

// Response headers that never come still fail, after wait + rendezvousHeaderSlack.
func TestRendezvousGetHeaderTimeout(t *testing.T) {
	old := rendezvousHeaderSlack
	rendezvousHeaderSlack = 80 * time.Millisecond
	t.Cleanup(func() { rendezvousHeaderSlack = old })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never answers on its own
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	_, err := NewProxyClient(srv.URL, nil).RendezvousGet(context.Background(), "pair-code-0123456789", 0, 0)
	if !errors.Is(err, ErrRendezvousHeaderTimeout) {
		t.Fatalf("want ErrRendezvousHeaderTimeout, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("header timeout took %v", d)
	}
}

// The caller's ctx interrupts a body download in progress (it is the only bound on it).
func TestRendezvousGetCtxCancelInterruptsBody(t *testing.T) {
	headersSent := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"items":[{"seq":1,"data":"`))
		w.(http.Flusher).Flush()
		close(headersSent)
		select { // the rest of the body never comes
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewProxyClient(srv.URL, nil).RendezvousGet(ctx, "pair-code-0123456789", 0, 0)
		done <- err
	}()
	<-headersSent
	cancel()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrRendezvousHeaderTimeout) {
			t.Fatalf("want a cancellation error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx cancel did not interrupt the body read")
	}
}

// A custom RendezvousHTTP is honoured.
func TestRendezvousGetUsesRendezvousHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	var used bool
	pc := NewProxyClient(srv.URL, nil)
	pc.RendezvousHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}
	if _, err := pc.RendezvousGet(context.Background(), "pair-code-0123456789", 0, 0); err != nil || !used {
		t.Fatalf("RendezvousHTTP must serve RendezvousGet: used=%v err=%v", used, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
