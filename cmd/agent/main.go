// Command agent runs a harness node: it discovers (or is pointed at) the
// manager, registers with a persistent identity, and heartbeats until
// stopped, reconnecting automatically on any failure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/discovery/udp"
	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/instancelock"
	"home-harness/internal/mtls"
	"home-harness/internal/protocol"
	"home-harness/internal/sysinfo"
	"home-harness/internal/transport/relay"
	"home-harness/internal/transport/ws"
)

func main() {
	managerAddr := flag.String("manager-addr", "", "manager address host:port; if empty, discover it via LAN multicast")
	pairingToken := flag.String("pairing-token", "", "shared secret required to register (required unless -pair)")
	identityDir := flag.String("identity-dir", defaultIdentityDir(), "directory holding this node's persistent identity")
	name := flag.String("name", "", "friendly node name (defaults to hostname)")
	heartbeatInterval := flag.Duration("heartbeat-interval", 5*time.Second, "how often to send heartbeats")
	managerFingerprint := flag.String("manager-fingerprint", "", "expected SHA-256 fingerprint of the manager's TLS certificate, printed on manager startup (required unless -insecure)")
	insecure := flag.Bool("insecure", false, "disable TLS: connect over plaintext ws:// with no manager authentication (dev/local use only; must match the manager's -insecure)")
	relayAddr := flag.String("relay-addr", "", "relay server (cmd/relay) address to connect through, for a manager that isn't on this device's LAN; if set, -manager-addr/discovery are not used")
	slots := flag.Int("slots", 0, "how many workloads this agent runs at once (0 = half the CPU count, at most 16)")
	ollamaURL := flag.String("ollama-url", "", "where this device's Ollama listens, for the local-model task types (default $OLLAMA_HOST, else http://127.0.0.1:11434); they're offered only while it answers")
	disable := flag.String("disable-capabilities", "", "comma-separated capabilities this device won't offer, e.g. system.execute,filesystem.read to allow only the sandboxed built-in task types")
	workDir := flag.String("work-dir", "", "where workloads that take input/output files get their working directories (default: the user cache dir); never inside -identity-dir")
	inputCacheSize := flag.String("input-cache-size", "1GiB", "keep up to this much of the input files tasks downloaded (under -work-dir), so more work on the same file copies it from this device instead of downloading it again, and the manager prefers this device for it; 0 keeps none")
	relayToken := flag.String("relay-token", "", "the manager's relay session token (required if -relay-addr is set)")
	pair := flag.Bool("pair", false, "join by approval on the manager instead of a pairing token: this device waits, showing a pairing code, until someone approves it there")
	pairCode := flag.Bool("pair-code", false, "print this device's pairing code (for -manager-fingerprint, creating the identity if needed) and exit")
	addrFallback := flag.String("manager-addr-fallback", "", "manager address (host:port) to use when LAN discovery finds none")
	deviceStateFile := flag.String("device-state-file", "", "a file another program keeps current with this device's charging and screen state (the Android app writes one); it overrides what the agent reads itself while it is fresh")
	priority := flag.String("priority", "low", "low: run tasks below normal priority, so the owner's own apps come first; normal: don't")
	llamaDir := flag.String("llama-cpp-dir", agent.DefaultLlamaCppDir(), "where this device's own llama.cpp build is (ggml-rpc-server, llama-server): lets it take part in models split across devices. Never downloaded by the agent")
	toolsDir := flag.String("tools-dir", agent.DefaultToolsDir(), "where this device's own ffmpeg, whisper.cpp (whisper-cli, models in models/ggml-<name>.bin), poppler (pdftotext, pdftoppm) and tesseract are, for converting media, transcribing and reading documents; standard install locations and PATH are searched too. Never downloaded by the agent")
	keepAwake := flag.Bool("keep-awake", true, "ask the OS not to sleep while a task runs here (it never blocks closing the lid or choosing Sleep)")
	noSelfUpdate := flag.Bool("no-self-update", false, "don't offer self-update (this binary can't be replaced in place; it is updated some other way, e.g. with the Android app)")
	appAPK := flag.String("app-apk", "", "the installed app this agent is part of (the Android app passes its own APK): its hash tells the manager which app version runs here")
	appUpdateFile := flag.String("app-update-file", "", "where to download a newer app when the manager offers one, for the app to install (with -app-apk)")
	flag.Parse()

	// Without -manager-fingerprint, a device that paired before pins the
	// certificate it kept then (see firstUse).
	pinFile := filepath.Join(*identityDir, pinnedFingerprintFile)
	if *managerFingerprint == "" && !*insecure {
		if data, err := os.ReadFile(pinFile); err == nil {
			*managerFingerprint = strings.TrimSpace(string(data))
		}
	}

	if *pairCode {
		id, err := identity.LoadOrCreate(*identityDir)
		if err != nil {
			log.Fatalf("agent: %v", err)
		}
		if *managerFingerprint == "" {
			log.Fatal("agent: -pair-code needs -manager-fingerprint")
		}
		fmt.Println("Pairing code: " + protocol.PairingCode(*managerFingerprint, id.PublicKey))
		return
	}
	if *pairingToken == "" && !*pair {
		log.Fatal("agent: -pairing-token is required (or -pair, to join by approval on the manager)")
	}
	if *relayAddr != "" && *relayToken == "" {
		log.Fatal("agent: -relay-token is required when -relay-addr is set")
	}

	var transport domain.Transport
	var selfUpdateHTTPClient *http.Client
	var selfUpdateURL string
	var tofu *firstUse
	switch {
	case *relayAddr != "" && !*insecure:
		if *managerFingerprint == "" {
			log.Fatal("agent: -manager-fingerprint is required unless -insecure is set (get it from the manager's startup log)")
		}
		relayTransport := relay.NewTLSClient(*relayToken, mtls.PinnedClientConfig(*managerFingerprint))
		transport = relayTransport
		selfUpdateHTTPClient = relayTransport.HTTPClient(*relayAddr)
		selfUpdateURL = "http://manager"
	case *relayAddr != "":
		log.Println("agent: running with -insecure: plaintext transport, manager identity not verified")
		relayTransport := relay.NewClient(*relayToken)
		transport = relayTransport
		selfUpdateHTTPClient = relayTransport.HTTPClient(*relayAddr)
		selfUpdateURL = "http://manager"
	case !*insecure && *managerFingerprint == "" && *pair:
		// A pairing device given no fingerprint (the Android app's worker):
		// trust on first use, pinned for every later connection.
		tofu = &firstUse{file: pinFile}
		transport = ws.NewTLSClient(tofu.config())
	case !*insecure:
		if *managerFingerprint == "" {
			log.Fatal("agent: -manager-fingerprint is required unless -insecure is set (get it from the manager's startup log)")
		}
		transport = ws.NewTLSClient(mtls.PinnedClientConfig(*managerFingerprint))
	default:
		log.Println("agent: running with -insecure: plaintext transport, manager identity not verified")
		transport = ws.New()
	}

	effectiveManagerAddr := *managerAddr
	if *relayAddr != "" {
		effectiveManagerAddr = *relayAddr
	}

	cfg := agent.Config{
		ManagerAddr:               effectiveManagerAddr,
		Discoverer:                &udp.Discoverer{},
		PairingToken:              *pairingToken,
		IdentityDir:               *identityDir,
		WorkloadSlots:             *slots,
		WorkDir:                   *workDir,
		DisabledCapabilities:      splitCapabilities(*disable),
		OllamaURL:                 *ollamaURL,
		Name:                      *name,
		HeartbeatInterval:         *heartbeatInterval,
		InsecureWorkloadsDisabled: *insecure,
		// LaunchArgs/Insecure/ManagerFingerprint exist solely for
		// selfupdate.go: a self-relaunch execs this same binary with these
		// exact flags, and the download needs to know the scheme and (if
		// not insecure) the pinned fingerprint to trust for its own
		// short-lived HTTP connection to the manager.
		LaunchArgs:           os.Args[1:],
		Insecure:             *insecure,
		ManagerFingerprint:   *managerFingerprint,
		SelfUpdateHTTPClient: selfUpdateHTTPClient,
		SelfUpdateBaseURL:    selfUpdateURL,
		Pairing:              *pair,
		ManagerAddrFallback:  *addrFallback,
		SelfUpdateDisabled:   *noSelfUpdate,
		AppAPK:               *appAPK,
		AppUpdateFile:        *appUpdateFile,
		DeviceStateFile:      *deviceStateFile,
		KeepAwake:            *keepAwake,
		LlamaCppDir:          *llamaDir,
		ToolsDir:             *toolsDir,
		ToolsSearchSystem:    true,
	}
	cacheBytes, err := domain.ParseByteSize(*inputCacheSize)
	if err != nil || cacheBytes > 1<<50 {
		log.Fatalf("agent: -input-cache-size %q: give a size like 1GiB, or 0 for none", *inputCacheSize)
	}
	cfg.InputCacheBytes = int64(cacheBytes)
	switch *priority {
	case "low":
		if err := sysinfo.LowerPriority(); err != nil {
			log.Printf("agent: can't lower this process's priority: %v", err)
		}
	case "normal":
	default:
		log.Fatalf("agent: -priority must be low or normal")
	}
	if tofu != nil {
		cfg.ManagerFingerprintFunc = tofu.fingerprint
		cfg.OnRegistered = tofu.keep
	}
	a, err := agent.New(transport, cfg)
	if err != nil {
		log.Fatalf("agent: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// One live process per identity: a launcher restart can overlap a
	// self-update's relaunch (or someone starts a second copy by hand). A
	// second process waits here as a standby and takes over only once the
	// running one exits — never two agents with one identity.
	lock, err := instancelock.TryAcquire(*identityDir)
	if errors.Is(err, instancelock.ErrHeld) {
		log.Printf("agent: another agent with this identity is already running; waiting as a standby")
	}
	for errors.Is(err, instancelock.ErrHeld) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		lock, err = instancelock.TryAcquire(*identityDir)
	}
	if err != nil {
		log.Fatalf("agent: instance lock: %v", err)
	}
	defer lock.Release()
	defer stop()

	log.Printf("agent %s starting (identity dir: %s)", a.NodeID(), *identityDir)
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("agent: %v", err)
	}
}

func defaultIdentityDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ".home-harness-agent"
	}
	return filepath.Join(dir, "home-harness", "agent")
}

func splitCapabilities(list string) []domain.CapabilityName {
	var out []domain.CapabilityName
	for _, f := range strings.Split(list, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, domain.CapabilityName(f))
		}
	}
	return out
}
