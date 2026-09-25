package relay

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// seen sends the heartbeat for device with an optional name header.
func (f *deviceFixture) seen(t *testing.T, device, name string) (int, map[string]any) {
	t.Helper()
	priv, err := f.id.EdPrivate()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"box": f.box})
	req := signedReq(t, priv, "POST", f.srv.URL+"/box/seen", "/box/seen", raw)
	if device != "" {
		req.Header.Set(a2a.HeaderDevice, device)
	}
	if name != "" {
		req.Header.Set(a2a.HeaderDeviceName, name)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *deviceFixture) devices(t *testing.T) []a2a.DeviceSeen {
	t.Helper()
	priv, err := f.id.EdPrivate()
	if err != nil {
		t.Fatal(err)
	}
	req := signedReq(t, priv, "GET", f.srv.URL+"/box/devices?box="+f.box, "/box/devices", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /box/devices: %d", resp.StatusCode)
	}
	var out struct {
		Devices []a2a.DeviceSeen `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Devices
}

func findDevice(ds []a2a.DeviceSeen, id string) *a2a.DeviceSeen {
	for i := range ds {
		if ds[i].Device == id {
			return &ds[i]
		}
	}
	return nil
}

func TestPresenceHeartbeatFromNonActiveDevice(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.claim(t, "dev-A", "Desktop"); code != 200 {
		t.Fatalf("claim: %d", code)
	}
	// dev-B is frozen: its mailbox requests are kicked...
	code, body := f.poll(t, "dev-B")
	assertKicked(t, code, body, "dev-A")
	// ...but its heartbeat is accepted and never kicked.
	if code, body := f.seen(t, "dev-B", "Laptop"); code != 200 || body["ok"] != true {
		t.Fatalf("heartbeat from a non-active device: %d %v", code, body)
	}
	ds := f.devices(t)
	a, b := findDevice(ds, "dev-A"), findDevice(ds, "dev-B")
	if a == nil || b == nil {
		t.Fatalf("both devices must be listed: %+v", ds)
	}
	if !a.Active || b.Active || b.Name != "Laptop" || b.LastSeen.IsZero() {
		t.Fatalf("device records: %+v", ds)
	}
	if ds[0].Device != "dev-B" {
		t.Fatalf("most recently seen first: %+v", ds)
	}
	// The heartbeat does not change who is active.
	if ad := f.s.ActiveDevice(f.box); ad == nil || ad.Device != "dev-A" {
		t.Fatalf("heartbeat must not claim the mailbox: %+v", ad)
	}
	// Without a device header, or unsigned, the heartbeat is refused.
	if code, _ := f.seen(t, "", ""); code != 400 {
		t.Fatalf("heartbeat without a device: want 400, got %d", code)
	}
	req, _ := http.NewRequest("POST", f.srv.URL+"/box/seen", nil)
	req.Header.Set(a2a.HeaderDevice, "dev-X")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 && resp.StatusCode != 401 {
		t.Fatalf("unsigned heartbeat: want 400/401, got %d", resp.StatusCode)
	}
	if findDevice(f.devices(t), "dev-X") != nil {
		t.Fatal("an unauthenticated request must not record presence")
	}
}

func TestPresenceLastSeenAdvancesAndNameSticks(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.seen(t, "dev-B", "Laptop"); code != 200 {
		t.Fatal("heartbeat")
	}
	first := findDevice(f.devices(t), "dev-B").LastSeen
	time.Sleep(20 * time.Millisecond)
	// Any signed owner request with the device header counts, even one that is kicked.
	if code, _ := f.claim(t, "dev-A", "Desktop"); code != 200 {
		t.Fatal("claim")
	}
	f.poll(t, "dev-B")
	b := findDevice(f.devices(t), "dev-B")
	if !b.LastSeen.After(first) {
		t.Fatalf("last_seen must advance: %v then %v", first, b.LastSeen)
	}
	if b.Name != "Laptop" {
		t.Fatalf("a request without a name header keeps the known name: %+v", b)
	}
	if findDevice(f.devices(t), "dev-A") == nil {
		t.Fatal("the claiming device must be recorded too")
	}
}

func TestPresenceSurvivesRestart(t *testing.T) {
	f := newDeviceFixture(t)
	f.s.psFlushGap = time.Hour // only the explicit Flush may write after the first record
	if code, _ := f.seen(t, "dev-A", "Desktop"); code != 200 {
		t.Fatal("heartbeat A")
	}
	if code, _ := f.seen(t, "dev-B", "Laptop"); code != 200 {
		t.Fatal("heartbeat B")
	}
	want := f.devices(t)
	f.s.Flush()
	if _, err := os.Stat(f.s.presencePath(f.box)); err != nil {
		t.Fatalf("presence file: %v", err)
	}
	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Devices(f.box)
	if len(got) != 2 {
		t.Fatalf("after restart: %+v", got)
	}
	for _, w := range want {
		g := findDevice(got, w.Device)
		if g == nil || g.Name != w.Name || !g.LastSeen.Equal(w.LastSeen) {
			t.Fatalf("device %s after restart: %+v, want %+v", w.Device, g, w)
		}
	}
}

func TestPresenceKeepsAtMostMaxDevices(t *testing.T) {
	f := newDeviceFixture(t)
	f.s.psFlushGap = time.Hour
	for i := 0; i < maxSeenDevices+3; i++ {
		f.s.noteDevice(fakeDeviceReq("dev-"+string(rune('a'+i%26))+string(rune('A'+i/26))), f.box)
		time.Sleep(time.Millisecond)
	}
	ds := f.s.Devices(f.box)
	if len(ds) != maxSeenDevices {
		t.Fatalf("want %d devices kept, got %d", maxSeenDevices, len(ds))
	}
	if findDevice(ds, "dev-aA") != nil {
		t.Fatal("the longest-unseen device should have been dropped first")
	}
}

func fakeDeviceReq(device string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(a2a.HeaderDevice, device)
	return r
}
