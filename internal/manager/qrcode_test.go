package manager

import (
	"bytes"
	"testing"
)

func TestQRSVGIsSelfContainedAndBounded(t *testing.T) {
	svg, err := qrSVG("https://192.168.1.10:7420/enroll/0123456789abcdef")
	if err != nil {
		t.Fatalf("qrSVG: %v", err)
	}
	for _, want := range [][]byte{[]byte("<svg"), []byte("viewBox=\"0 0 390 390\""), []byte("<path")} {
		if !bytes.Contains(svg, want) {
			t.Fatalf("SVG missing %q", want)
		}
	}
	if bytes.Contains(svg, []byte("https://192.168.1.10")) {
		t.Fatal("SVG should encode the URL as modules, not expose it as external content")
	}
}

func TestQRSVGRejectsOversizedPayload(t *testing.T) {
	if _, err := qrSVG(string(bytes.Repeat([]byte{'x'}, 272))); err == nil {
		t.Fatal("expected oversized byte-mode payload to be rejected")
	}
}

func TestQRKnownFormatAndVersionBits(t *testing.T) {
	// Published QR BCH constants for error-correction level L/mask 0 and
	// Version 10. These catch polynomial, bit-order, and mask regressions.
	if got := qrFormatBits(0); got != 0x77C4 {
		t.Fatalf("format bits = %#x, want %#x", got, 0x77C4)
	}
	if got := qrVersionBits(10); got != 0x0A4D3 {
		t.Fatalf("version bits = %#x, want %#x", got, 0x0A4D3)
	}
}
