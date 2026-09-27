package a2a

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRendezvousBinaryRoundTrip(t *testing.T) {
	items := []RendezvousItem{{Seq: 1, Data: []byte("a")}, {Seq: 2, Data: []byte{}}, {Seq: 9, Data: bytes.Repeat([]byte{0xff}, 70000)}}
	var buf bytes.Buffer
	if err := WriteRendezvousItemsBinary(&buf, items); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != RendezvousItemsBinarySize(items) {
		t.Fatalf("size %d != %d", buf.Len(), RendezvousItemsBinarySize(items))
	}
	got, err := ReadRendezvousItemsBinary(&buf)
	if err != nil || len(got) != 3 || got[0].Seq != 1 || string(got[0].Data) != "a" || len(got[1].Data) != 0 || got[2].Seq != 9 || !bytes.Equal(got[2].Data, items[2].Data) {
		t.Fatalf("round trip: %v %v", got, err)
	}
	// Empty body = empty (non-nil) list.
	if got, err := ReadRendezvousItemsBinary(bytes.NewReader(nil)); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	// A blob over the cap is refused on the writing side too.
	if err := WriteRendezvousItemsBinary(&bytes.Buffer{}, []RendezvousItem{{Seq: 1, Data: make([]byte, RendezvousMaxBlob+1)}}); err == nil {
		t.Fatal("oversized blob must not be written")
	}
}

func TestRendezvousBinaryRejectsMalformed(t *testing.T) {
	frame := func(seq int64, n uint32, data []byte) []byte {
		var h [12]byte
		binary.BigEndian.PutUint64(h[:8], uint64(seq))
		binary.BigEndian.PutUint32(h[8:], n)
		return append(h[:], data...)
	}
	cases := map[string][]byte{
		"cut header":     frame(1, 1, []byte("x"))[:7],
		"cut blob":       frame(1, 5, []byte("ab")),
		"oversized":      frame(1, RendezvousMaxBlob+1, nil),
		"seq not rising": append(frame(2, 1, []byte("x")), frame(2, 1, []byte("y"))...),
		"seq zero":       frame(0, 1, []byte("x")),
	}
	for name, body := range cases {
		if _, err := ReadRendezvousItemsBinary(bytes.NewReader(body)); !errors.Is(err, ErrRendezvousFrame) {
			t.Fatalf("%s: want ErrRendezvousFrame, got %v", name, err)
		}
	}
}

func TestAcceptsRendezvousBinary(t *testing.T) {
	for accept, want := range map[string]bool{
		"":                    false,
		"application/json":    false,
		"*/*":                 false,
		RendezvousItemsBinary: true,
		RendezvousItemsBinary + ";q=0.5, application/json": true,
		"application/json, " + RendezvousItemsBinary:       true,
		RendezvousItemsBinary + ";q=0":                     false,
		RendezvousItemsBinary + ";q=0.000":                 false,
	} {
		if got := AcceptsRendezvousBinary(accept); got != want {
			t.Fatalf("Accept %q: got %v want %v", accept, got, want)
		}
	}
}

// The client asks for the binary form and reads whichever form the server chose: a server
// that does not know the media type (the relay today, a LAN rendezvous of an older build)
// answers JSON and must keep working.
func TestRendezvousGetReadsBinaryOrJSON(t *testing.T) {
	payload := bytes.Repeat([]byte{1, 2, 3}, 1000)
	for _, binaryReply := range []bool{true, false} {
		var sawAccept string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawAccept = r.Header.Get("Accept")
			items := []RendezvousItem{{Seq: 3, Data: payload}, {Seq: 4, Data: []byte("z")}}
			if binaryReply {
				w.Header().Set("Content-Type", RendezvousItemsBinary)
				_ = WriteRendezvousItemsBinary(w, items)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		}))
		items, err := NewProxyClient(srv.URL, nil).RendezvousGet(context.Background(), "pair-code-0123456789", 0, 0)
		srv.Close()
		if !AcceptsRendezvousBinary(sawAccept) {
			t.Fatalf("client must ask for the binary form, sent Accept %q", sawAccept)
		}
		if err != nil || len(items) != 2 || items[0].Seq != 3 || !bytes.Equal(items[0].Data, payload) || string(items[1].Data) != "z" {
			t.Fatalf("binary=%v: %v %v", binaryReply, items, err)
		}
	}
}
