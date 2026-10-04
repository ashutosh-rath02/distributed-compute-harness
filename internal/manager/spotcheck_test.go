package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

func countJob(id domain.JobID, n int, withReduce bool) domain.Job {
	j := domain.Job{ID: id}
	for i := 0; i < n; i++ {
		j.Tasks = append(j.Tasks, domain.TaskSpec{Capability: "text.count"})
	}
	if withReduce {
		j.Reduce = &domain.TaskSpec{Capability: "file.hash"}
	}
	return j
}

func sampleOf(key []byte, job domain.Job, percent int) map[string]bool {
	_, keys := spotSampleKeys(key, job, percent)
	return keys
}

func TestSpotSampleTakesAShareOfDeterministicTasksAtLeastOne(t *testing.T) {
	key := []byte("k")
	if got := sampleOf(key, countJob("j", 10, false), 0); len(got) != 0 {
		t.Fatalf("0%% samples %v", got)
	}
	for _, c := range []struct{ tasks, percent, want int }{{10, 1, 1}, {10, 10, 1}, {10, 11, 2}, {10, 50, 5}, {10, 100, 10}, {1, 5, 1}, {1000, 10, 100}} {
		if got := sampleOf(key, countJob("j", c.tasks, false), c.percent); len(got) != c.want {
			t.Errorf("%d tasks at %d%%: %d sampled, want %d", c.tasks, c.percent, len(got), c.want)
		}
	}
	// Only deterministic types count: raw commands, archives, fractals never.
	mixed := domain.Job{ID: "m", Tasks: []domain.TaskSpec{
		{Capability: domain.CapabilitySystemExecute}, {Capability: "render.fractal"}, {Capability: "image.resize"}, {Capability: "llm.generate"},
	}, Reduce: &domain.TaskSpec{Capability: "archive.zip"}}
	if got := sampleOf(key, mixed, 100); len(got) != 1 || !got["0002"] {
		t.Fatalf("mixed job sampled %v, want only the image.resize task", got)
	}
	if got := sampleOf(key, domain.Job{ID: "x", Tasks: []domain.TaskSpec{{Capability: "cpu.burn"}}}, 100); len(got) != 0 {
		t.Fatalf("a job with nothing checkable sampled %v", got)
	}
	// The reduce is checkable too.
	if got := sampleOf(key, countJob("r", 3, true), 100); len(got) != 4 || !got[domain.ReduceTask] {
		t.Fatalf("with reduce: %v", got)
	}
	// Decided once: the same job gives the same sample, a larger share
	// keeps it, and the secret (and the job) changes which tasks.
	a := sampleOf(key, countJob("j", 40, false), 10)
	if b := sampleOf(key, countJob("j", 40, false), 10); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("not stable: %v vs %v", a, b)
	}
	wider := sampleOf(key, countJob("j", 40, false), 30)
	for k := range a {
		if !wider[k] {
			t.Fatalf("raising the share dropped %s", k)
		}
	}
	differs := func(other map[string]bool) bool { return fmt.Sprint(other) != fmt.Sprint(a) }
	if !differs(sampleOf([]byte("other"), countJob("j", 40, false), 10)) || !differs(sampleOf(key, countJob("j2", 40, false), 10)) {
		t.Fatal("the sample must depend on the secret and the job")
	}
}

func outs(sha string) []domain.ArtifactRef {
	return []domain.ArtifactRef{{Name: "counts.json", SHA256: sha, Size: int64(len(sha))}}
}

func run(node domain.NodeID, state domain.WorkloadState, sha string) domain.SpotCheckRun {
	r := domain.SpotCheckRun{Workload: domain.WorkloadID("w-" + node), Node: node, State: state}
	if state == domain.WorkloadCompleted {
		r.Outputs = outs(sha)
	}
	return r
}

func TestJudgeSpotCheck(t *testing.T) {
	name := func(id domain.NodeID) string { return string(id) }
	base := domain.SpotCheck{Node: "orig", Outputs: outs("aaaa")}
	for _, c := range []struct {
		name     string
		runs     []domain.SpotCheckRun
		run      bool
		wait     bool
		state    domain.SpotCheckState
		suspects []domain.NodeID
	}{
		{name: "nothing yet", run: true},
		{name: "queued", runs: []domain.SpotCheckRun{run("", domain.WorkloadQueued, "")}, wait: true},
		{name: "running", runs: []domain.SpotCheckRun{run("b", domain.WorkloadRunning, "")}, wait: true},
		{name: "same", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "aaaa")}, state: domain.SpotMatched},
		{name: "a failed check is no evidence", runs: []domain.SpotCheckRun{run("b", domain.WorkloadFailed, ""), run("c", domain.WorkloadUnknown, "")}, run: true},
		{name: "failed then same", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCanceled, ""), run("c", domain.WorkloadCompleted, "aaaa")}, state: domain.SpotMatched},
		{name: "differs: tiebreak next", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "bbbb")}, run: true},
		{name: "differs: tiebreak running", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "bbbb"), run("c", domain.WorkloadPending, "")}, wait: true},
		{name: "tiebreak sides with the original", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "bbbb"), run("c", domain.WorkloadCompleted, "aaaa")}, state: domain.SpotMismatch, suspects: []domain.NodeID{"b"}},
		{name: "tiebreak sides with the check", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "bbbb"), run("c", domain.WorkloadCompleted, "bbbb")}, state: domain.SpotMismatch, suspects: []domain.NodeID{"orig"}},
		{name: "a failed tiebreak doesn't count", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "bbbb"), run("c", domain.WorkloadFailed, ""), run("d", domain.WorkloadCompleted, "bbbb")}, state: domain.SpotMismatch, suspects: []domain.NodeID{"orig"}},
		{name: "no two agree", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "bbbb"), run("c", domain.WorkloadCompleted, "cccc")}, state: domain.SpotUnresolved},
		{name: "out of re-runs before any result", runs: []domain.SpotCheckRun{run("b", domain.WorkloadFailed, ""), run("c", domain.WorkloadFailed, ""), run("d", domain.WorkloadFailed, ""), run("e", domain.WorkloadFailed, "")}, state: domain.SpotSkipped},
		{name: "out of re-runs before a tiebreak", runs: []domain.SpotCheckRun{run("b", domain.WorkloadCompleted, "bbbb"), run("c", domain.WorkloadFailed, ""), run("d", domain.WorkloadFailed, ""), run("e", domain.WorkloadFailed, "")}, state: domain.SpotUnresolved},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := base
			sc.Runs = c.runs
			v := judgeSpotCheck(sc, name)
			if v.run != c.run || v.wait != c.wait || v.state != c.state || fmt.Sprint(v.suspects) != fmt.Sprint(c.suspects) {
				t.Fatalf("got %+v, want run=%v wait=%v state=%q suspects=%v", v, c.run, c.wait, c.state, c.suspects)
			}
			if c.state != "" && !strings.HasPrefix(v.detail, map[domain.SpotCheckState]string{
				domain.SpotMatched: "matched", domain.SpotMismatch: "mismatch:", domain.SpotUnresolved: "mismatch, unresolved", domain.SpotSkipped: "skipped",
			}[c.state]) {
				t.Fatalf("detail %q doesn't say %s", v.detail, c.state)
			}
		})
	}
}

func TestSameOutputsComparesNamesHashesAndSizes(t *testing.T) {
	a := []domain.ArtifactRef{{Name: "x", SHA256: "1", Size: 1}, {Name: "y", SHA256: "2", Size: 2}}
	if !sameOutputs(a, []domain.ArtifactRef{a[1], a[0]}) {
		t.Fatal("order must not matter")
	}
	for _, b := range [][]domain.ArtifactRef{
		{{Name: "x", SHA256: "1", Size: 1}, {Name: "y", SHA256: "3", Size: 2}},
		{{Name: "x", SHA256: "1", Size: 1}, {Name: "y", SHA256: "2", Size: 3}},
		{{Name: "x", SHA256: "1", Size: 1}, {Name: "z", SHA256: "2", Size: 2}},
		{{Name: "x", SHA256: "1", Size: 1}},
		nil,
	} {
		if sameOutputs(a, b) {
			t.Errorf("%v reported equal to %v", b, a)
		}
	}
	if sameOutputs(nil, nil) {
		t.Fatal("two empty results prove nothing")
	}
}

// A spot check never runs where the result it checks came from — not
// even when that node is the only one (unlike AvoidNodes).
func TestExcludedNodesAreNeverUsed(t *testing.T) {
	r := NewRegistry()
	readyNode(t, r, "n1", 1)
	readyNode(t, r, "n2", 1)
	s := &Server{Registry: r, Workloads: NewWorkloadRegistry()}
	p := placement{capability: domain.CapabilitySystemExecute, exclude: []domain.NodeID{"n1"}}
	if p.unconstrained() {
		t.Fatal("an excluding placement is constrained: its miss says nothing about other work")
	}
	if _, id, err := s.resolve(p, nil); err != nil || id != "n2" {
		t.Fatalf("got %s %v, want n2", id, err)
	}
	s.Workloads.Put(domain.Workload{ID: "busy", Target: "n2"}, domain.WorkloadStatus{State: domain.WorkloadRunning})
	if _, _, err := s.resolve(p, nil); !errors.Is(err, errNoRoom) {
		t.Fatalf("n2 busy: got %v, want to wait for it (errNoRoom)", err)
	}
	p.exclude = []domain.NodeID{"n1", "n2"}
	if _, id, err := s.resolve(p, nil); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("every node excluded: got %s %v, want ErrNoEligibleNode", id, err)
	}
	p.exclude, p.target = []domain.NodeID{"n1"}, "n1"
	if _, _, err := s.resolve(p, nil); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("pinned to an excluded node: got %v", err)
	}
	if got := placementFor(domain.Workload{ExcludeNodes: []domain.NodeID{"n1"}}).exclude; len(got) != 1 {
		t.Fatalf("placementFor dropped ExcludeNodes: %v", got)
	}
}

// The device doing a check is told nothing that marks it as one.
func TestAssignDoesNotTellTheDeviceItIsACheck(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	conn := &recordingConn{sent: make(chan []byte, 4)} // selfupdate_test.go
	s.assign(context.Background(), conn, domain.Workload{ID: "w", Target: "n2", Capability: "text.count", ExcludeNodes: []domain.NodeID{"n1"}})
	var wire []byte
	select {
	case wire = <-conn.sent:
	default:
		t.Fatal("nothing sent")
	}
	env, err := protocol.Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	var p protocol.WorkloadAssignPayload
	if err := env.DecodePayload(&p); err != nil {
		t.Fatal(err)
	}
	if p.Workload.ID != "w" || len(p.Workload.ExcludeNodes) != 0 || strings.Contains(string(wire), "excludeNodes") {
		t.Fatalf("the assignment names the excluded nodes: %s", wire)
	}
}

func TestSpotCheckPolicyIsValidatedAndKept(t *testing.T) {
	for _, bad := range []int{-1, 101} {
		if err := validatePolicy(domain.Policy{SpotCheckPercent: bad}); err == nil {
			t.Errorf("%d%% accepted", bad)
		}
	}
	if err := validatePolicy(domain.Policy{SpotCheckPercent: 100}); err != nil {
		t.Fatal(err)
	}
	if got := clonePolicy(domain.Policy{SpotCheckPercent: 25}); got.SpotCheckPercent != 25 {
		t.Fatalf("clonePolicy dropped the spot-check share: %+v", got)
	}
}

// A device that is wrong on every task of a big job gets one security
// log entry for that job, not one per task; its mark counts them all.
func TestMismatchAuditIsOncePerJobAndDevice(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	tbl := s.spotTable()
	report := func(job domain.JobID, task string, state domain.SpotCheckState, suspects ...domain.NodeID) {
		c := domain.SpotCheck{Job: job, Task: task, Capability: "text.count", Node: "liar", Original: "w", Outputs: outs("bbbb"), State: state, Suspects: suspects,
			Runs: []domain.SpotCheckRun{run("a", domain.WorkloadCompleted, "aaaa"), run("b", domain.WorkloadCompleted, "aaaa")}}
		s.reportSpotMismatch(tbl, &c)
		tbl.put(c)
	}
	entries := func() []domain.AuditEntry {
		all, _ := s.AuditEntries(domain.AuditSecurity, 0)
		var out []domain.AuditEntry
		for _, e := range all {
			if e.Kind == "node.result-mismatch" {
				out = append(out, e)
			}
		}
		return out
	}
	for i := 0; i < 5; i++ {
		report("j1", domain.TaskKey(i), domain.SpotMismatch, "liar")
	}
	got := entries()
	if len(got) != 1 || got[0].NodeID != "liar" || got[0].Actor != actorSpotCheck || got[0].Log != domain.AuditSecurity {
		t.Fatalf("after 5 mismatches in one job: %+v", got)
	}
	if m, ok := s.SuspectMark("liar"); !ok || m.Count != 5 || m.Job != "j1" {
		t.Fatalf("mark = %+v %v", m, ok)
	}
	report("j2", "0000", domain.SpotMismatch, "liar") // another job: its own entry
	report("j2", "0001", domain.SpotUnresolved)       // nobody singled out: one entry per job
	report("j2", "0002", domain.SpotUnresolved)
	if got := entries(); len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if _, ok := s.SuspectMark("a"); ok {
		t.Fatal("an unresolved mismatch must not mark anyone")
	}
	if _, ok := s.ClearSuspect("liar"); !ok {
		t.Fatal("clear")
	}
	if _, ok := s.SuspectMark("liar"); ok {
		t.Fatal("still marked after clearing")
	}
}

// A check is skipped, not left open, when no other device exists, and a
// canceled job's open check is closed.
func TestSpotCheckSkipsWithoutAnotherDevice(t *testing.T) {
	store, err := artifacts.Open(artifacts.Config{Dir: t.TempDir(), MaxBytes: 1 << 20, TotalBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	in, err := store.Put(strings.NewReader("one two\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, nil, Config{Artifacts: store})
	readyNode(t, s.Registry, "only", 1)
	s.Registry.UpdateResources("only", nil, []domain.Capability{{Name: "text.count", Version: "1"}})
	s.Workloads.Put(domain.Workload{ID: "orig", Target: "only", Capability: "text.count", Job: "j", Task: "0000", Attempt: 1,
		Inputs: []domain.ArtifactRef{{Name: "a.txt", SHA256: in.SHA256, Size: in.Size}}, Outputs: []string{"counts.json"}},
		domain.WorkloadStatus{State: domain.WorkloadCompleted, Outputs: outs("aaaa")})
	tbl := s.spotTable()
	job := domain.Job{ID: "j", State: domain.JobRunning, Tasks: []domain.TaskSpec{{Capability: "text.count"}}, MaxAttempts: 1}
	orig, _ := s.Workloads.Get("orig")
	s.startSpotCheck(context.Background(), tbl, job, "0000", orig, time.Now())
	c, ok := tbl.get("j", "0000")
	if !ok || c.State != domain.SpotSkipped || c.Detail != "skipped: no other device" || len(c.Runs) != 0 {
		t.Fatalf("check = %+v", c)
	}
	if got := s.spotCheckLabel("j", "0000"); got != "skipped: no other device" {
		t.Fatalf("label %q", got)
	}
	open := domain.SpotCheck{Job: "j", Task: "0001", State: domain.SpotChecking, CreatedAt: time.Now()}
	tbl.put(open)
	job.State = domain.JobCanceled
	s.stepSpotCheck(context.Background(), tbl, open, job, time.Now())
	if c, _ := tbl.get("j", "0001"); c.State != domain.SpotSkipped || !strings.Contains(c.Detail, "canceled") {
		t.Fatalf("canceled job's check = %+v", c)
	}
	if n := s.spotCheckCounts("j"); n == nil || n.Skipped != 2 {
		t.Fatalf("counts = %+v", n)
	}
}

// tenTaskJob is a finished 10-task text.count job whose attempts are in
// the registry: every task COMPLETED except those in failed.
func tenTaskJob(s *Server, percent int, failed map[string]bool) domain.Job {
	job := countJob("ten", 10, false)
	job.State, job.MaxAttempts, job.SpotCheckPercent, job.FinishedAt = domain.JobFailed, 1, percent, time.Now().UTC()
	s.jobs.put(job)
	for i := range job.Tasks {
		key := domain.TaskKey(i)
		st := domain.WorkloadStatus{State: domain.WorkloadCompleted, Outputs: outs("aaaa" + key)}
		if failed[key] {
			st = domain.WorkloadStatus{State: domain.WorkloadFailed, Error: "boom"}
		}
		s.Workloads.Put(domain.Workload{ID: domain.WorkloadID("w" + key), Target: "n1", Capability: "text.count", Job: job.ID, Task: key, Attempt: 1}, st)
	}
	return job
}

// "At least one per job" holds even when the sampled task never
// completed: the first-ranked task that did is checked instead.
func TestSpotCheckAtLeastOnePerJobFallsBackToACompletedTask(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.policy.set(domain.Policy{AllowUnlisted: true, SpotCheckPercent: 10})
	job := countJob("ten", 10, false)
	_, sampled := spotSampleKeys(s.spots.key, job, 10)
	if len(sampled) != 1 {
		t.Fatalf("sampled %v", sampled)
	}
	tenTaskJob(s, 10, sampled) // the sampled task failed
	s.advanceSpotChecks(context.Background())
	checks := s.spots.ofJob("ten")
	if len(checks) != 1 || sampled[checks[0].Task] {
		t.Fatalf("checks %+v: want exactly one, on a task that completed", checks)
	}
	s.advanceSpotChecks(context.Background())
	if n := len(s.spots.ofJob("ten")); n != 1 {
		t.Fatalf("a second pass made it %d checks", n)
	}
}

// A job keeps the share it was submitted with; spot checks turned off
// stop new checks for it too.
func TestSpotCheckFollowsTheJobsShareAndThePolicySwitch(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.policy.set(domain.Policy{AllowUnlisted: true, SpotCheckPercent: 100})
	tenTaskJob(s, 0, nil) // submitted while off
	s.advanceSpotChecks(context.Background())
	if n := len(s.spots.ofJob("ten")); n != 0 {
		t.Fatalf("a job submitted with checks off got %d", n)
	}
	s = NewServer(nil, nil, Config{})
	s.policy.set(domain.Policy{AllowUnlisted: true, SpotCheckPercent: 0})
	tenTaskJob(s, 100, nil) // submitted while on, then turned off
	s.advanceSpotChecks(context.Background())
	if n := len(s.spots.ofJob("ten")); n != 0 {
		t.Fatalf("checks off, yet %d started", n)
	}
	s.policy.set(domain.Policy{AllowUnlisted: true, SpotCheckPercent: 30})
	s.advanceSpotChecks(context.Background())
	if n := len(s.spots.ofJob("ten")); n != 3 {
		t.Fatalf("at 30%% of 10 tasks: %d checks, want 3", n)
	}
}

func TestSpotCheckTableKeepsTheNewestFinished(t *testing.T) {
	tbl := newSpotCheckTable()
	base := time.Now()
	for i := 0; i < 5; i++ {
		tbl.put(domain.SpotCheck{Job: "j", Task: domain.TaskKey(i), State: domain.SpotMatched, CreatedAt: base.Add(time.Duration(i) * time.Second)})
	}
	tbl.put(domain.SpotCheck{Job: "k", Task: "0000", State: domain.SpotChecking, CreatedAt: base.Add(-time.Hour)})
	tbl.put(domain.SpotCheck{Job: "j", Task: "0004", State: domain.SpotMatched, CreatedAt: base.Add(4 * time.Second)}) // replaces, not adds
	dropped := tbl.prune(3)
	if len(dropped) != 3 || dropped[0].Task != "0000" || dropped[2].Task != "0002" {
		t.Fatalf("dropped %+v", dropped)
	}
	if _, ok := tbl.get("k", "0000"); !ok {
		t.Fatal("an open check must never be dropped")
	}
	if len(tbl.ofJob("j")) != 2 || tbl.prune(3) != nil {
		t.Fatalf("left %+v", tbl.ofJob("j"))
	}
}

// Clearing a mark is an operator action: no credential, or the AI key
// (good only on /v1), can't do it.
func TestClearSuspectNeedsTheOperator(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken, AIKey: "ai-key-for-the-test"})
	s.markSuspect(s.spotTable(), "n1", domain.SpotCheck{Job: "j", Task: "0000", Capability: "text.count"})
	h := s.NewHTTPHandler()
	for _, bearer := range []string{"", "wrong", "ai-key-for-the-test"} {
		if rec := operatorRequest(t, h, "DELETE", "/nodes/n1/suspect", bearer, ""); rec.Code != 401 {
			t.Errorf("DELETE with %q: %d, want 401", bearer, rec.Code)
		}
	}
	if _, ok := s.SuspectMark("n1"); !ok {
		t.Fatal("the mark was cleared without the operator")
	}
	if rec := operatorRequest(t, h, "DELETE", "/nodes/n1/suspect", testOperatorToken, ""); rec.Code != 200 {
		t.Fatalf("operator DELETE: %d %s", rec.Code, rec.Body)
	}
	if _, ok := s.SuspectMark("n1"); ok {
		t.Fatal("still marked")
	}
}
