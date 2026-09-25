package relay

import (
	"context"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// waitDevice polls GET /box/devices until cond holds for device (or fails after 5s).
func (f *deviceFixture) waitDevice(t *testing.T, device, what string, cond func(d *a2a.DeviceSeen) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if d := findDevice(f.devices(t), device); d != nil && cond(d) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s never became true; devices=%+v", device, what, f.devices(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPresenceConnConnectedThenOfflineOnClose(t *testing.T) {
	f := newDeviceFixture(t)
	pc := a2a.NewProxyClient(f.srv.URL, f.id).WithDevice("dev-A", "Desktop")
	conn, err := pc.DialPresence(context.Background(), 0)
	if err != nil {
		t.Fatalf("DialPresence: %v", err)
	}
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	f.waitDevice(t, "dev-A", "Connected", func(d *a2a.DeviceSeen) bool { return d.Connected && !d.Offline })
	_ = conn.Close()
	f.waitDevice(t, "dev-A", "Offline after close", func(d *a2a.DeviceSeen) bool { return d.Offline && !d.Connected })
}

// A device that stops answering pings (lid closed, power cut: the TCP connection stays
// half-open) is marked Offline within about two ping intervals.
func TestPresenceConnSilentClientGoesOffline(t *testing.T) {
	oldMin, oldSlack := minPresencePing, presenceSlack
	minPresencePing, presenceSlack = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { minPresencePing, presenceSlack = oldMin, oldSlack })

	f := newDeviceFixture(t)
	pc := a2a.NewProxyClient(f.srv.URL, f.id).WithDevice("dev-A", "Desktop")
	conn, err := pc.DialPresence(context.Background(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("DialPresence: %v", err)
	}
	defer conn.Close()
	// Never read: the pings go unanswered, the relay's read deadline expires.
	f.waitDevice(t, "dev-A", "Offline after silence", func(d *a2a.DeviceSeen) bool { return d.Offline && !d.Connected })
}

// Two connections of one device (e.g. a reconnect racing the old one): Offline only when both end.
func TestPresenceConnOfflineOnlyAfterLastConnection(t *testing.T) {
	f := newDeviceFixture(t)
	pc := a2a.NewProxyClient(f.srv.URL, f.id).WithDevice("dev-A", "Desktop")
	c1, err := pc.DialPresence(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := pc.DialPresence(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []interface{ ReadMessage() (int, []byte, error) }{c1, c2} {
		go func() {
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}
	f.waitDevice(t, "dev-A", "Connected", func(d *a2a.DeviceSeen) bool { return d.Connected })
	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)
	if d := findDevice(f.devices(t), "dev-A"); d == nil || !d.Connected || d.Offline {
		t.Fatalf("one connection still open: want Connected, got %+v", d)
	}
	_ = c2.Close()
	f.waitDevice(t, "dev-A", "Offline after last close", func(d *a2a.DeviceSeen) bool { return d.Offline })
}

func TestPresenceConnRequiresDevice(t *testing.T) {
	f := newDeviceFixture(t)
	if _, err := a2a.NewProxyClient(f.srv.URL, f.id).DialPresence(context.Background(), 0); err == nil {
		t.Fatal("DialPresence without a device id must fail")
	}
}

func TestParsePresencePingBounds(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": defaultPresencePing, "junk": defaultPresencePing, "1s": minPresencePing,
		"20s": 20 * time.Second, "10m": maxPresencePing,
	} {
		if got := parsePresencePing(in); got != want {
			t.Errorf("parsePresencePing(%q) = %v, want %v", in, got, want)
		}
	}
}
