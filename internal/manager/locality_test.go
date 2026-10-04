package manager

import (
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func shaOf(c byte) string { return strings.Repeat(string(c), 64) }

func prefixOf(sha string) string { return sha[:domain.InputCachePrefixLen] }

// cachingNode is a ready node advertising the input cache.
func cachingNode(t *testing.T, r *Registry, id domain.NodeID, slots int, memory uint64) {
	t.Helper()
	readyNode(t, r, id, slots)
	r.mu.Lock()
	r.nodes[id].AgentFeatures = []string{domain.FeatureArtifacts, domain.FeatureInputCache}
	r.nodes[id].LastMetrics.MemoryAvailableBytes = memory
	r.mu.Unlock()
}

// Between equally busy nodes the one holding the input wins, even over
// more free memory and a lower node ID.
func TestLocalityBreaksTiesWithinOneLoadLevel(t *testing.T) {
	r := NewRegistry()
	cachingNode(t, r, "a-roomy", 2, 16<<30)
	cachingNode(t, r, "b-holder", 2, 2<<30)
	big := domain.ArtifactRef{Name: "video.mp4", SHA256: shaOf('a'), Size: 200 << 20}
	r.setCached("b-holder", &domain.InputCacheReport{Prefixes: []string{prefixOf(big.SHA256)}})
	s := &Server{Registry: r, Workloads: NewWorkloadRegistry()}
	p := placement{capability: domain.CapabilitySystemExecute, inputs: []domain.ArtifactRef{big}}

	if _, id, err := s.resolve(p, nil); err != nil || id != "b-holder" {
		t.Fatalf("idle fleet, b holds the file: got %s %v", id, err)
	}
	// Without the file in the picture, memory decides as before.
	p.inputs = []domain.ArtifactRef{{Name: "other", SHA256: shaOf('c'), Size: 1 << 20}}
	if _, id, _ := s.resolve(p, nil); id != "a-roomy" {
		t.Fatalf("no holder: got %s, want the roomier a", id)
	}
	// More bytes held beats fewer.
	small := domain.ArtifactRef{Name: "small", SHA256: shaOf('d'), Size: 1 << 20}
	r.setCached("a-roomy", &domain.InputCacheReport{Prefixes: []string{prefixOf(small.SHA256)}})
	p.inputs = []domain.ArtifactRef{small, big}
	if _, id, _ := s.resolve(p, nil); id != "b-holder" {
		t.Fatalf("b holds 200 MiB, a 1 MiB: got %s", id)
	}
}

// Load comes first: a busier node never wins on locality, so a job's
// tasks on one file still spread.
func TestLocalityNeverOutranksLoad(t *testing.T) {
	r := NewRegistry()
	cachingNode(t, r, "a", 2, 8<<30)
	cachingNode(t, r, "b", 2, 8<<30)
	f := domain.ArtifactRef{Name: "f", SHA256: shaOf('e'), Size: 100 << 20}
	r.setCached("a", &domain.InputCacheReport{Prefixes: []string{prefixOf(f.SHA256)}})
	s := &Server{Registry: r, Workloads: NewWorkloadRegistry()}
	p := placement{capability: domain.CapabilitySystemExecute, inputs: []domain.ArtifactRef{f}}

	usage := map[domain.NodeID]nodeUsage{}
	got := map[domain.NodeID]int{}
	for i := 0; i < 2; i++ {
		rec, id, err := s.resolve(p, usage)
		if err != nil {
			t.Fatal(err)
		}
		got[id]++
		u := usage[id]
		u.add(domain.Workload{Target: id})
		usage[id] = u
		_ = rec
	}
	if got["a"] != 1 || got["b"] != 1 {
		t.Fatalf("two tasks on the same file over two idle nodes: %v, want one each", got)
	}
	// Directly: a at 1 of 2 slots, b idle → b, though a holds the file.
	if rec, _ := selectNodeLocal([]*NodeRecord{r.nodes["a"], r.nodes["b"]}, map[domain.NodeID]nodeUsage{"a": {running: 1}},
		map[domain.NodeID]int64{"a": f.Size}, domain.CapabilitySystemExecute, domain.ResourceRequirements{}); rec.Node.Identity.NodeID != "b" {
		t.Fatalf("busier holder chosen: %s", rec.Node.Identity.NodeID)
	}
}

// Hints count each distinct file once, at the manager's recorded size,
// never the agent's word; malformed and excess entries are ignored.
func TestLocalBytesCountsDistinctInputsAtTheManagersSizes(t *testing.T) {
	r := NewRegistry()
	cachingNode(t, r, "n", 1, 1<<30)
	f := domain.ArtifactRef{Name: "a/x", SHA256: shaOf('1'), Size: 300}
	g := domain.ArtifactRef{Name: "g", SHA256: shaOf('b'), Size: 50}
	r.setCached("n", &domain.InputCacheReport{Prefixes: []string{
		prefixOf(f.SHA256), "NOT-HEX-AT-ALL!!", strings.ToUpper(prefixOf(g.SHA256)), prefixOf(g.SHA256) + "00",
	}})
	twice := []domain.ArtifactRef{f, {Name: "b/x", SHA256: f.SHA256, Size: 300}, g}
	if got := r.localBytes([]*NodeRecord{r.nodes["n"]}, twice)["n"]; got != 300 {
		t.Fatalf("held = %d, want 300 (f once; g's entries were malformed)", got)
	}
	if r.localBytes([]*NodeRecord{r.nodes["n"]}, nil) != nil {
		t.Fatal("a workload without files has no locality")
	}

	// Only the first MaxInputCacheReport entries are read.
	long := make([]string, domain.MaxInputCacheReport, domain.MaxInputCacheReport+1)
	for i := range long {
		long[i] = prefixOf(shaOf('3'))
	}
	long = append(long, prefixOf(g.SHA256))
	r.setCached("n", &domain.InputCacheReport{Prefixes: long})
	if r.HoldsInput("n", g.SHA256) {
		t.Fatal("an entry past the report's bound was trusted")
	}
	// A heartbeat without a report changes nothing; an empty one clears.
	r.setCached("n", nil)
	if !r.HoldsInput("n", shaOf('3')) {
		t.Fatal("no report must leave the last one in place")
	}
	r.setCached("n", &domain.InputCacheReport{})
	if r.HoldsInput("n", shaOf('3')) {
		t.Fatal("an empty report must clear what the node held")
	}
}

// Agents without FeatureInputCache (older builds) get no preference, even
// if something on the connection claims to hold the file; and a
// reconnect forgets the old claim until the new connection reports.
func TestCacheHintsOnlyFromAgentsWithTheFeature(t *testing.T) {
	r := NewRegistry()
	readyNode(t, r, "old", 1) // no FeatureInputCache
	f := domain.ArtifactRef{Name: "f", SHA256: shaOf('4'), Size: 10}
	r.setCached("old", &domain.InputCacheReport{Prefixes: []string{prefixOf(f.SHA256)}})
	if r.HoldsInput("old", f.SHA256) || r.localBytes([]*NodeRecord{r.nodes["old"]}, []domain.ArtifactRef{f}) != nil {
		t.Fatal("a hint from an agent without the feature was trusted")
	}

	cachingNode(t, r, "new", 1, 1<<30)
	r.setCached("new", &domain.InputCacheReport{Prefixes: []string{prefixOf(f.SHA256)}})
	if !r.HoldsInput("new", f.SHA256) {
		t.Fatal("a hint from an agent with the feature was ignored")
	}
	m := testNode("new")
	m.AgentFeatures = []string{domain.FeatureArtifacts, domain.FeatureInputCache}
	r.Upsert(m, &fakeConn{tag: "again"})
	if r.HoldsInput("new", f.SHA256) {
		t.Fatal("a reconnect must forget the previous connection's report")
	}
}
