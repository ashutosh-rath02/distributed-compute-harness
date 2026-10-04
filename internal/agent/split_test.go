package agent

import "testing"

func TestParseLlamaVersion(t *testing.T) {
	for out, want := range map[string]string{
		// llama.cpp b11382 (2026), as printed on Windows.
		"version: 0.5.0-dev (build 11382, commit 11fe02151)\nbuilt with Clang 20.1.8 for Windows x86_64\n": "11382-11fe02151",
		"version: 0.5.0-dev (build 11383, commit 0a1b2c3d4)\n":                                             "11383-0a1b2c3d4",
		"version: 4567 (abc1234)\nbuilt with MSVC\n":                                                       "4567-abc1234",
		"version: 9999 (fakehash)\n":                                                                       "9999-fakehash",
		"usage: ggml-rpc-server [options]\n":                                                               "",
	} {
		if got := parseLlamaVersion(out); got != want {
			t.Errorf("%q: %q, want %q", out, got, want)
		}
	}
}
