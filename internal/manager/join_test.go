package manager

import "testing"

func TestJoinInfoListsTheCatalog(t *testing.T) {
	s := &Server{
		cfg:    Config{Fingerprint: "abc123", PairingToken: "shhh"},
		agents: &agentCatalog{builds: []agentBuild{windowsBuild, androidBuild}},
	}
	info := s.JoinInfo()
	if info.Fingerprint != "abc123" || info.PairingToken != "shhh" || info.Insecure {
		t.Fatalf("unexpected credentials in JoinInfo: %+v", info)
	}
	if len(info.AgentBinaries) != 2 || info.AgentBinaries[1].OS != "linux" || info.AgentBinaries[1].SHA256 != "android-hash" ||
		info.AgentBinaries[1].Path != "/agent-binaries/linux/arm64" {
		t.Fatalf("expected both catalog builds in JoinInfo, got %+v", info.AgentBinaries)
	}
	if info.LANAddr != "" {
		t.Fatalf("no -advertise-addr, yet lanAddr %q", info.LANAddr)
	}
}

// The Android app can't let the manager find its own Wi-Fi address, so
// it passes one; the dashboard offers it from /join-info.
func TestJoinInfoCarriesTheAdvertisedAddress(t *testing.T) {
	s := &Server{cfg: Config{AdvertiseAddr: "192.168.1.20:7420"}, agents: &agentCatalog{}}
	if got := s.JoinInfo().LANAddr; got != "192.168.1.20:7420" {
		t.Fatalf("lanAddr = %q", got)
	}
}

func TestJoinInfoInsecureWhenFingerprintEmpty(t *testing.T) {
	s := &Server{cfg: Config{PairingToken: "shhh"}, agents: &agentCatalog{}}
	info := s.JoinInfo()
	if !info.Insecure {
		t.Error("expected Insecure true when Fingerprint is empty")
	}
	if info.AgentBinaries == nil || len(info.AgentBinaries) != 0 {
		t.Errorf("expected an empty (not null) agentBinaries list, got %#v", info.AgentBinaries)
	}
}

// scriptInfo must hand each onboarding platform its own build — and none
// at all when that platform has no build loaded, never another
// platform's (an Android script verifying a Windows hash would install an
// agent that can't run).
func TestScriptInfoPicksThePlatformsOwnBuild(t *testing.T) {
	s := &Server{agents: &agentCatalog{builds: []agentBuild{windowsBuild}}}
	win := s.scriptInfo("windows")
	if !win.AgentBinaryAvailable || win.AgentBinarySHA256 != "windows-hash" || win.AgentBinaryPath != "/agent-binaries/windows/amd64" {
		t.Fatalf("windows: got %+v", win)
	}
	if android := s.scriptInfo("android"); android.AgentBinaryAvailable || android.AgentBinarySHA256 != "" {
		t.Fatalf("android with only a Windows build loaded must have no binary, got %+v", android)
	}
}
