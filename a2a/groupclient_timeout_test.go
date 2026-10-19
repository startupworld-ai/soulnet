package a2a

import (
	"net/http"
	"testing"
	"time"
)

// A /group/mail body that is nearly 1 MiB (inline attachment or artifact_chunk) must not
// be bound by the 15s short-request timeout: a slow uplink made the relay deliver while
// the client had given up, and the sender's retry duplicated the post.
func TestGroupDeliverTimeoutScalesWithBody(t *testing.T) {
	if got := GroupDeliverTimeout(15*time.Second, 100); got != 15*time.Second {
		t.Fatalf("tiny body keeps the base timeout, got %v", got)
	}
	if got := GroupDeliverTimeout(15*time.Second, 932*1024); got != 15*time.Second+58*time.Second {
		t.Fatalf("932 KB -> base + 58s, got %v", got)
	}
	if got := GroupDeliverTimeout(15*time.Second, 10<<20); got != GroupDeliverMaxTimeout {
		t.Fatalf("huge body is capped at %v, got %v", GroupDeliverMaxTimeout, got)
	}
	if got := GroupDeliverTimeout(0, 0); got != DefaultDeliverTimeout {
		t.Fatalf("zero base falls back to the default, got %v", got)
	}
}

func TestGroupHTTPKeepsTransportAndWidensTimeout(t *testing.T) {
	c := NewProxyClient("http://relay.test", nil).WithDeliverTimeout(15 * time.Second)
	c.ShortHTTP.Transport = &http.Transport{}
	small := c.groupHTTP(100)
	if small != c.ShortHTTP {
		t.Fatal("a small body should reuse the short client as is")
	}
	big := c.groupHTTP(932 * 1024)
	if big == c.ShortHTTP || big.Timeout != 73*time.Second || big.Transport != c.ShortHTTP.Transport {
		t.Fatalf("a big body should get a copy with the same transport and a wider timeout, got %+v", big)
	}
	if c.ShortHTTP.Timeout != 15*time.Second {
		t.Fatal("the shared short client must not be mutated")
	}
}
