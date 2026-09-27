package a2a

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"
)

// Binary reply of GET /rendezvous/{id}.
//
// The JSON reply {"items":[{seq,data}]} carries every blob base64 inside a JSON string, so a
// reader scans and decodes 4/3 of the payload twice (once for the JSON scanner, once for
// base64). For the few-KB handshake blobs that is nothing; for a data window of tens of MB it
// is the dominant CPU cost on the reading side -- about 70 MB/s on a desktop core, a fraction
// of that on a phone, well below a local network.
//
// A client that can read the binary form asks for it with
//
//	Accept: application/vnd.soulnet.rendezvous-items
//
// and a server that supports it answers with that Content-Type and a body of frames
//
//	seq  int64  big endian
//	len  uint32 big endian (<= RendezvousMaxBlob)
//	data len bytes
//
// back to back, in seq order; an empty list is an empty body. A server that does not know
// the media type ignores the Accept header and answers JSON as always, so the client must
// branch on the reply's Content-Type, never on what it asked for. Errors stay JSON
// ({"error": msg} with a non-200 status) in both forms.
const RendezvousItemsBinary = "application/vnd.soulnet.rendezvous-items"

// rendezvousFrameHeader is the size of one frame header (seq + len).
const rendezvousFrameHeader = 12

// AcceptsRendezvousBinary reports whether an Accept header value lists RendezvousItemsBinary
// (with a non-zero q, if any).
func AcceptsRendezvousBinary(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || mt != RendezvousItemsBinary {
			continue
		}
		if q, ok := params["q"]; ok {
			if v, err := strconv.ParseFloat(q, 64); err != nil || v <= 0 {
				return false
			}
		}
		return true
	}
	return false
}

// IsRendezvousBinary reports whether a reply Content-Type is the binary form.
func IsRendezvousBinary(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == RendezvousItemsBinary
}

// RendezvousItemsBinarySize is the length of the binary body for items (for Content-Length).
func RendezvousItemsBinarySize(items []RendezvousItem) int {
	n := 0
	for _, it := range items {
		n += rendezvousFrameHeader + len(it.Data)
	}
	return n
}

// WriteRendezvousItemsBinary writes items as binary frames (see RendezvousItemsBinary).
func WriteRendezvousItemsBinary(w io.Writer, items []RendezvousItem) error {
	var hdr [rendezvousFrameHeader]byte
	for _, it := range items {
		if len(it.Data) > RendezvousMaxBlob {
			return fmt.Errorf("rendezvous: blob %d exceeds %d bytes", it.Seq, RendezvousMaxBlob)
		}
		binary.BigEndian.PutUint64(hdr[0:8], uint64(it.Seq))
		binary.BigEndian.PutUint32(hdr[8:12], uint32(len(it.Data)))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.Write(it.Data); err != nil {
			return err
		}
	}
	return nil
}

// ErrRendezvousFrame reports a malformed binary rendezvous reply.
var ErrRendezvousFrame = errors.New("rendezvous: malformed binary reply")

// ReadRendezvousItemsBinary reads binary frames until EOF. A frame cut short, a blob over
// RendezvousMaxBlob or seqs not strictly increasing yield ErrRendezvousFrame (wrapped).
func ReadRendezvousItemsBinary(r io.Reader) ([]RendezvousItem, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	items := []RendezvousItem{}
	var hdr [rendezvousFrameHeader]byte
	var last int64
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return items, nil
			}
			return nil, fmt.Errorf("%w: header: %v", ErrRendezvousFrame, err)
		}
		seq := int64(binary.BigEndian.Uint64(hdr[0:8]))
		n := binary.BigEndian.Uint32(hdr[8:12])
		if n > RendezvousMaxBlob {
			return nil, fmt.Errorf("%w: blob of %d bytes", ErrRendezvousFrame, n)
		}
		if seq <= last {
			return nil, fmt.Errorf("%w: seq %d after %d", ErrRendezvousFrame, seq, last)
		}
		last = seq
		data := make([]byte, n)
		if _, err := io.ReadFull(br, data); err != nil {
			return nil, fmt.Errorf("%w: blob %d: %v", ErrRendezvousFrame, seq, err)
		}
		items = append(items, RendezvousItem{Seq: seq, Data: data})
	}
}
