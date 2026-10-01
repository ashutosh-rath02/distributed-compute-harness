package manager

import (
	"fmt"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func recordOn(id, fingerprint string, cores, memory float64) *NodeRecord {
	return &NodeRecord{
		Node: domain.Node{Identity: domain.Identity{NodeID: domain.NodeID(id)}, HostFingerprint: fingerprint},
		Resources: []domain.Resource{
			{Kind: domain.ResourceCPUCores, Capacity: cores},
			{Kind: domain.ResourceMemoryBytes, Capacity: memory},
		},
	}
}

func TestHostRelationsCorroborateOnlyWithMatchingHardware(t *testing.T) {
	records := []*NodeRecord{
		recordOn("node-a", "fp-desk", 16, 32e9), // two identities on one desktop
		recordOn("node-b", "fp-desk", 16, 32e9),
		recordOn("node-c", "fp-clone", 8, 16e9), // cloned VM images: same fingerprint,
		recordOn("node-d", "fp-clone", 4, 8e9),  // different hardware — a conflict
		recordOn("node-e", "", 2, 4e9),          // older agent: no fingerprint
		recordOn("node-f", "fp-alone", 2, 4e9),
	}
	rel := hostRelations(records)
	if r := rel["node-a"]; r.conflict || len(r.sameHostAs) != 1 || r.sameHostAs[0] != "node-b" {
		t.Errorf("node-a: %+v", r)
	}
	if r := rel["node-c"]; !r.conflict || len(r.sameHostAs) != 1 || r.sameHostAs[0] != "node-d" {
		t.Errorf("node-c should be a conflict with node-d: %+v", r)
	}
	for _, id := range []domain.NodeID{"node-e", "node-f"} {
		if _, grouped := rel[id]; grouped {
			t.Errorf("%s should not be grouped", id)
		}
	}

	var ids []string
	for _, r := range dedupedForTotals(records) {
		ids = append(ids, string(r.Node.Identity.NodeID))
	}
	// One of a/b (the lower ID), both conflicting c/d, and e/f.
	if got := strings.Join(ids, ","); got != "node-a,node-c,node-d,node-e,node-f" {
		t.Fatalf("records counted toward totals: %s", got)
	}
}

func TestRegistryTotalsCountACorroboratedMachineOnce(t *testing.T) {
	r := NewRegistry()
	for _, rec := range []*NodeRecord{recordOn("node-a", "fp-desk", 16, 32e9), recordOn("node-b", "fp-desk", 16, 32e9), recordOn("node-c", "", 4, 8e9)} {
		r.Upsert(domain.Manifest{Node: rec.Node, Resources: rec.Resources}, nil)
	}
	totals := r.TotalResources()
	if totals[domain.ResourceCPUCores] != 20 || totals[domain.ResourceMemoryBytes] != 40e9 {
		t.Fatalf("expected the shared desktop counted once (16+4 cores), got %+v", totals)
	}
}

func TestValidateNodeMeta(t *testing.T) {
	ok := domain.NodeMeta{Alias: "Living-room PC 🎮", Labels: map[string]string{"gpu": "iris-xe", "room.floor": "1"}}
	if err := validateNodeMeta(ok); err != nil {
		t.Fatalf("valid metadata refused: %v", err)
	}
	many := map[string]string{}
	for i := 0; i < maxLabels+1; i++ {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	for name, bad := range map[string]domain.NodeMeta{
		"long alias":         {Alias: strings.Repeat("x", maxAliasLen+1)},
		"control in alias":   {Alias: "evil\nname"},
		"uppercase key":      {Labels: map[string]string{"GPU": "x"}},
		"key with space":     {Labels: map[string]string{"a b": "x"}},
		"key leading dash":   {Labels: map[string]string{"-a": "x"}},
		"empty value":        {Labels: map[string]string{"a": ""}},
		"control in value":   {Labels: map[string]string{"a": "x\x00"}},
		"oversize value":     {Labels: map[string]string{"a": strings.Repeat("v", maxLabelValueLen+1)}},
		"too many labels":    {Labels: many},
		"invalid utf8 alias": {Alias: "\xff"},
	} {
		if err := validateNodeMeta(bad); err == nil {
			t.Errorf("%s: expected refusal", name)
		}
	}
}
