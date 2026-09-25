package peer

import (
	"context"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

func TestHeartbeatAndDevicesThroughPeer(t *testing.T) {
	ctx := context.Background()
	a, b, _ := vaultTwoDevices(t)
	if _, err := a.ClaimActive(ctx, ""); err != nil {
		t.Fatalf("A claims: %v", err)
	}
	// B is frozen, yet its heartbeat goes through (never kicked).
	if err := b.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat from the non-active device: %v", err)
	}
	// Asking is itself a signed request from A, so A is always the freshest entry here.
	ds, err := a.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	find := func(ds []a2a.DeviceSeen, id string) *a2a.DeviceSeen {
		for i := range ds {
			if ds[i].Device == id {
				return &ds[i]
			}
		}
		return nil
	}
	da, db := find(ds, "dev-A"), find(ds, "dev-B")
	if len(ds) != 2 || da == nil || db == nil || !da.Active || db.Active || db.Name != "Laptop" || ds[0].Device != "dev-A" {
		t.Fatalf("devices (A active and freshest, B listed): %+v", ds)
	}
	first := db.LastSeen
	time.Sleep(20 * time.Millisecond)
	if err := b.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	ds, err = b.Devices(ctx) // a frozen device may read the list too
	if err != nil {
		t.Fatal(err)
	}
	if db = find(ds, "dev-B"); db == nil || !db.LastSeen.After(first) {
		t.Fatalf("last_seen must advance: %v then %+v", first, db)
	}

	// Heartbeat needs a device id; both need an identity.
	a.DeviceID = ""
	if err := a.Heartbeat(ctx); err == nil {
		t.Fatal("Heartbeat without a DeviceID must fail")
	}
}
