package manager

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/store/persistent"
)

func boolp(b bool) *bool    { return &b }
func intp(i int) *int       { return &i }
func int64p(i int64) *int64 { return &i }
func at(h, m int) time.Time { return time.Date(2026, 10, 4, h, m, 0, 0, time.Local) }

func TestEvaluateAvailability(t *testing.T) {
	noon := at(12, 0)
	onBattery := &domain.DeviceUse{OnBattery: boolp(true), BatteryPercent: intp(40)}
	pluggedIn := &domain.DeviceUse{OnBattery: boolp(false), BatteryPercent: intp(90)}
	busyUser := &domain.DeviceUse{OnBattery: boolp(false), IdleSeconds: int64p(30)}
	awayUser := &domain.DeviceUse{OnBattery: boolp(false), IdleSeconds: int64p(20 * 60)}
	unknown := &domain.DeviceUse{}
	for _, c := range []struct {
		name       string
		rule       domain.Availability
		reports    bool
		use        *domain.DeviceUse
		now        time.Time
		want       bool
		wantReason string
	}{
		{"old agent: always available", domain.Availability{}, false, nil, noon, true, ""},
		{"old agent: still paused by the operator", domain.Availability{Mode: domain.AvailablePaused}, false, nil, noon, false, "paused"},
		{"no report yet: not now", domain.Availability{}, true, nil, noon, false, "hasn't reported"},
		{"auto on battery", domain.Availability{}, true, onBattery, noon, false, "on battery (40%)"},
		{"auto plugged in", domain.Availability{Mode: domain.AvailableAuto}, true, pluggedIn, noon, true, ""},
		{"auto, power unknown", domain.Availability{}, true, unknown, noon, true, ""},
		{"charging on battery", domain.Availability{Mode: domain.AvailableWhenCharging}, true, onBattery, noon, false, "on battery"},
		{"always on battery", domain.Availability{Mode: domain.AvailableAlways}, true, onBattery, noon, true, ""},
		{"idle while in use", domain.Availability{Mode: domain.AvailableWhenIdle}, true, busyUser, noon, false, "in use (idle 30s, needs 5 min)"},
		{"idle when away", domain.Availability{Mode: domain.AvailableWhenIdle, IdleMinutes: 15}, true, awayUser, noon, true, ""},
		{"idle, needs longer", domain.Availability{Mode: domain.AvailableWhenIdle, IdleMinutes: 30}, true, awayUser, noon, false, "needs 30 min"},
		{"idle, can't see input", domain.Availability{Mode: domain.AvailableWhenIdle}, true, unknown, noon, true, "can't see"},
		{"hours: inside a wrapping window", domain.Availability{Hours: "22:00-07:00"}, true, pluggedIn, at(3, 0), true, ""},
		{"hours: outside it", domain.Availability{Hours: "22:00-07:00"}, true, pluggedIn, noon, false, "outside its hours (22:00-07:00)"},
		{"hours: end is exclusive", domain.Availability{Hours: "09:00-17:00"}, true, pluggedIn, at(17, 0), false, "outside"},
		{"hours and battery both apply", domain.Availability{Hours: "09:00-17:00"}, true, onBattery, noon, false, "on battery"},
	} {
		got, reason := evaluateAvailability(c.rule, c.reports, c.use == nil, c.use, c.now)
		if got != c.want || !strings.Contains(reason, c.wantReason) {
			t.Errorf("%s: (%v, %q), want (%v, ~%q)", c.name, got, reason, c.want, c.wantReason)
		}
	}
}

func TestValidateAvailability(t *testing.T) {
	for _, ok := range []domain.Availability{
		{}, {Mode: domain.AvailableAuto}, {Mode: domain.AvailablePaused}, {Mode: domain.AvailableWhenIdle, IdleMinutes: 10},
		{Mode: domain.AvailableWhenCharging, Hours: "22:00-07:00"}, {Mode: domain.AvailableAlways, Hours: "08:30-17:45"},
	} {
		if err := validateAvailability(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []domain.Availability{
		{Mode: "sometimes"}, {Mode: domain.AvailableWhenIdle, IdleMinutes: -1}, {Mode: domain.AvailableAlways, IdleMinutes: 5},
		{Hours: "22-07"}, {Hours: "25:00-07:00"}, {Hours: "07:00-07:00"}, {Hours: "7:5-8:00"}, {Hours: "<b>"},
	} {
		if err := validateAvailability(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// The rule is set on its own; renaming keeps it, a device with only a
// rule is persisted, and the default rule leaves nothing behind.
func TestAvailabilityRulePersistsBesideAliasAndLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	store, err := persistent.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, store, Config{})
	s.Registry.Upsert(testNode("n1"), &fakeConn{tag: "n1"})
	if _, err := s.SetNodeAvailability("n1", domain.Availability{Mode: domain.AvailableWhenIdle, IdleMinutes: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNodeMeta("n1", domain.NodeMeta{Alias: "Desk PC"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNodeAvailability("ghost", domain.Availability{}); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("unknown node: %v", err)
	}
	if _, err := s.SetNodeAvailability("n1", domain.Availability{Mode: "nope"}); err == nil {
		t.Fatal("bad mode accepted")
	}
	store.Close()

	store, err = persistent.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	metas, _ := store.ListNodeMeta()
	m := metas["n1"]
	if m.Alias != "Desk PC" || m.Availability == nil || m.Availability.Mode != domain.AvailableWhenIdle || m.Availability.IdleMinutes != 10 {
		t.Fatalf("after a rename and a reopen: %+v", m)
	}
	// Only a rule, no alias or labels: still kept.
	s = NewServer(nil, store, Config{})
	s.Registry.Upsert(testNode("n2"), &fakeConn{tag: "n2"})
	s.SetNodeAvailability("n2", domain.Availability{Mode: domain.AvailablePaused})
	if metas, _ := store.ListNodeMeta(); metas["n2"].Availability == nil {
		t.Fatal("a device with only an availability rule wasn't persisted")
	}
	// Back to the default: nothing left to keep.
	s.SetNodeAvailability("n2", domain.Availability{Mode: domain.AvailableAuto})
	if metas, _ := store.ListNodeMeta(); len(metas) != 1 {
		t.Fatalf("the default rule left a record: %+v", metas)
	}
}

func TestWaitingReasonNamesDevices(t *testing.T) {
	err := &noRoomError{detail: "[node-a: on battery]", reasons: []string{"Laptop: on battery (40%)", "Desk PC: in use (idle 30s, needs 5 min)"}}
	if !errors.Is(err, errNoRoom) || err.Error() != errNoRoom.Error()+" ([node-a: on battery])" {
		t.Fatalf("error: %v", err)
	}
	if got := waitingReason(err); got != "Laptop: on battery (40%); Desk PC: in use (idle 30s, needs 5 min)" {
		t.Fatalf("waiting reason %q", got)
	}
	if got := waitingReason(ErrNodeNotConnected); got != "node is not connected" {
		t.Fatalf("other error: %q", got)
	}
}

// The manager holds its keep-awake request while work runs, for a grace
// period after, and not for queued work alone.
func TestManagerKeepsAwakeOnlyWhileWorkRuns(t *testing.T) {
	s := NewServer(nil, nil, Config{KeepAwake: true})
	defer func() {
		if s.awake != nil {
			s.awake.Hold(false)
		}
	}()
	now := time.Now()
	s.keepAwakeTick(now)
	if s.KeepingAwake() {
		t.Fatal("held with nothing to do")
	}
	s.Workloads.Put(domain.Workload{ID: "q"}, domain.WorkloadStatus{ID: "q", State: domain.WorkloadQueued})
	s.keepAwakeTick(now)
	if s.KeepingAwake() {
		t.Fatal("held for queued work alone")
	}
	s.Workloads.Put(domain.Workload{ID: "r"}, domain.WorkloadStatus{ID: "r", State: domain.WorkloadRunning})
	s.keepAwakeTick(now)
	if !s.KeepingAwake() {
		t.Fatal("not held while a workload runs")
	}
	s.Workloads.UpdateStatus(domain.WorkloadStatus{ID: "r", State: domain.WorkloadCompleted})
	s.keepAwakeTick(now.Add(time.Minute))
	if !s.KeepingAwake() {
		t.Fatal("released before the grace period ended")
	}
	s.keepAwakeTick(now.Add(keepAwakeGrace + time.Second))
	if s.KeepingAwake() {
		t.Fatal("still held after the grace period")
	}
}
