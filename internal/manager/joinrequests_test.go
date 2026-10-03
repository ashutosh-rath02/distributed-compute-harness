package manager

import (
	"fmt"
	"testing"
	"time"

	"home-harness/internal/domain"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestJoinRequests() (*joinRequests, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)}
	j := newJoinRequests()
	j.now = clock.now
	return j, clock
}

func req(id, host string) JoinRequest {
	return JoinRequest{NodeID: domain.NodeID(id), Name: id, Remote: host, Code: "123 456"}
}

func TestJoinRequestApprovedAtTheNextRegister(t *testing.T) {
	j, clock := newTestJoinRequests()
	if d, isNew := j.request(req("node-a", "10.0.0.2"), true); d != joinPending || !isNew {
		t.Fatalf("first ask: %v new=%v", d, isNew)
	}
	clock.advance(2 * time.Second)
	if d, isNew := j.request(req("node-a", "10.0.0.2"), true); d != joinPending || isNew {
		t.Fatalf("asking again must be the same request, still pending: %v new=%v", d, isNew)
	}
	if got := j.list(); len(got) != 1 || got[0].Approved {
		t.Fatalf("list: %+v", got)
	}
	if _, ok := j.approve("node-a"); !ok {
		t.Fatal("approve failed")
	}
	if _, ok := j.approve("node-a"); ok {
		t.Fatal("approving twice must report nothing to approve")
	}
	if d, _ := j.request(req("node-a", "10.0.0.2"), true); d != joinApproved {
		t.Fatalf("after approval the next ask must be admitted, got %v", d)
	}
	if len(j.list()) != 0 {
		t.Fatal("an admitted request must be gone")
	}
	if _, ok := j.approve("node-nobody"); ok {
		t.Fatal("approved a device that never asked")
	}
}

func TestJoinRequestRejectHoldsTheDeviceOff(t *testing.T) {
	j, clock := newTestJoinRequests()
	j.request(req("node-a", "10.0.0.2"), true)
	if _, ok := j.reject("node-a"); !ok {
		t.Fatal("reject failed")
	}
	if d, _ := j.request(req("node-a", "10.0.0.2"), true); d != joinRejected {
		t.Fatalf("a rejected device asking again must stay rejected, got %v", d)
	}
	if len(j.list()) != 0 {
		t.Fatal("a rejected device must not reappear as a request")
	}
	clock.advance(joinRejectHold + time.Second)
	if d, isNew := j.request(req("node-a", "10.0.0.2"), true); d != joinPending || !isNew {
		t.Fatalf("after the hold it may ask again: %v new=%v", d, isNew)
	}
}

func TestJoinRequestLimits(t *testing.T) {
	j, _ := newTestJoinRequests()
	for i := 0; i < maxJoinRequestsPerHost; i++ {
		if d, _ := j.request(req(fmt.Sprintf("node-%d", i), "10.0.0.2"), true); d != joinPending {
			t.Fatalf("request %d: %v", i, d)
		}
	}
	if d, _ := j.request(req("node-one-too-many", "10.0.0.2"), true); d != joinFull {
		t.Fatalf("one host may not hold more than %d requests, got %v", maxJoinRequestsPerHost, d)
	}
	// Other hosts still get in, up to the overall cap.
	for i := 0; len(j.list()) < maxJoinRequests; i++ {
		if d, _ := j.request(req(fmt.Sprintf("node-h%d", i), fmt.Sprintf("10.0.1.%d", i)), true); d != joinPending {
			t.Fatalf("host %d: %v", i, d)
		}
	}
	if d, _ := j.request(req("node-last", "10.0.9.9"), true); d != joinFull {
		t.Fatalf("more than %d requests in all must be refused, got %v", maxJoinRequests, d)
	}
	// A device already waiting keeps its place when the list is full.
	if d, _ := j.request(req("node-0", "10.0.0.2"), true); d != joinPending {
		t.Fatalf("a waiting device asking again must not be refused, got %v", d)
	}
}

func TestJoinRequestsExpire(t *testing.T) {
	j, clock := newTestJoinRequests()
	j.request(req("node-gone", "10.0.0.2"), true)
	j.request(req("node-stays", "10.0.0.3"), true)
	j.request(req("node-approved", "10.0.0.4"), true)
	j.approve("node-approved")

	// node-stays keeps asking; node-gone stopped.
	for elapsed := time.Duration(0); elapsed <= joinRequestIdle; elapsed += 5 * time.Second {
		clock.advance(5 * time.Second)
		j.request(req("node-stays", "10.0.0.3"), true)
	}
	gone := j.prune()
	if len(gone) != 1 || gone[0].NodeID != "node-gone" {
		t.Fatalf("a device that stopped asking must expire alone, got %+v", gone)
	}
	// Even a device that keeps asking expires after the maximum age.
	for clock.advance(5 * time.Second); clock.t.Sub(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)) <= joinRequestMaxAge; clock.advance(5 * time.Second) {
		j.request(req("node-stays", "10.0.0.3"), true)
	}
	if d, isNew := j.request(req("node-stays", "10.0.0.3"), true); !isNew || d != joinPending {
		t.Fatalf("after the maximum age the request starts over: %v new=%v", d, isNew)
	}
	// An approved device that never came back is dropped too.
	for _, r := range j.list() {
		if r.NodeID == "node-approved" {
			t.Fatal("an approved request must not outlive the approval hold")
		}
	}
}
