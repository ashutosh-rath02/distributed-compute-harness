package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSplitShardsCutsAtLineEndsAndLosesNothing(t *testing.T) {
	var text strings.Builder
	for i := 0; i < 1000; i++ {
		text.WriteString("a line of training text number ")
		text.WriteString(strings.Repeat("x", i%17))
		text.WriteString("\n")
	}
	for _, n := range []int{1, 2, 3, 7} {
		shards := splitShards([]byte(text.String()), n)
		if len(shards) != n {
			t.Fatalf("n=%d gave %d shards", n, len(shards))
		}
		if string(bytes.Join(shards, nil)) != text.String() {
			t.Fatalf("n=%d: shards joined differ from the text", n)
		}
		for i, s := range shards[:n-1] {
			if len(s) > 0 && s[len(s)-1] != '\n' {
				t.Fatalf("n=%d: shard %d ends mid-line", n, i)
			}
			if n > 1 && len(s) < len(text.String())/n/2 {
				t.Fatalf("n=%d: shard %d is far too small (%d bytes)", n, i, len(s))
			}
		}
	}
}

func TestAllocateStepsMakesDevicesFinishTogether(t *testing.T) {
	if got := allocateSteps(20, []float64{0, 0}); got[0] != 20 || got[1] != 20 {
		t.Fatalf("unknown speeds get the base: %v", got)
	}
	got := allocateSteps(20, []float64{2, 4, 8})
	if got[0] != 20 || got[1] != 10 || got[2] != 5 {
		t.Fatalf("2s, 4s and 8s per step should get 20, 10, 5 steps, got %v", got)
	}
	if got := allocateSteps(2, []float64{1, 100}); got[1] != 1 {
		t.Fatalf("a very slow device still gets one step: %v", got)
	}
	if weightList([]int{20, 10, 5}) != "20,10,5" {
		t.Fatal("weightList")
	}
}
