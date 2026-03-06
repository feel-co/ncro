package narinfo_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"notashelf.dev/ncro/internal/narinfo"
)

var realWorldNarinfo = `StorePath: /nix/store/s66mzxpvicwklp6cpph4dc53k5l6bfhe-hello-2.12.1
URL: nar/1wwh37nhg4f5zhb2vsn1a81p3ixn69gkg5k6fvmw3nhcn19fg8xj.nar.xz
Compression: xz
FileHash: sha256:1wwh37nhg4f5zhb2vsn1a81p3ixn69gkg5k6fvmw3nhcn19fg8xj
FileSize: 50088
NarHash: sha256:04rrn5x6lnzrfkcy3bh7gf7x6hq3w1kap4wdss2n6n4s19pgbkr7
NarSize: 226512
References: s66mzxpvicwklp6cpph4dc53k5l6bfhe-hello-2.12.1 4nlgxhzzvsnr6bva0b9afnq8lbr9rk2b-glibc-2.38-23
Sig: cache.nixos.org-1:abc123+base64signature=
`

func TestParseRealWorld(t *testing.T) {
	ni, err := narinfo.Parse(strings.NewReader(realWorldNarinfo))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ni.StorePath != "/nix/store/s66mzxpvicwklp6cpph4dc53k5l6bfhe-hello-2.12.1" {
		t.Errorf("StorePath = %q", ni.StorePath)
	}
	if ni.URL != "nar/1wwh37nhg4f5zhb2vsn1a81p3ixn69gkg5k6fvmw3nhcn19fg8xj.nar.xz" {
		t.Errorf("URL = %q", ni.URL)
	}
	if ni.Compression != "xz" {
		t.Errorf("Compression = %q, want xz", ni.Compression)
	}
	if ni.FileSize != 50088 {
		t.Errorf("FileSize = %d, want 50088", ni.FileSize)
	}
	if ni.NarHash != "sha256:04rrn5x6lnzrfkcy3bh7gf7x6hq3w1kap4wdss2n6n4s19pgbkr7" {
		t.Errorf("NarHash = %q", ni.NarHash)
	}
	if ni.NarSize != 226512 {
		t.Errorf("NarSize = %d, want 226512", ni.NarSize)
	}
	if len(ni.References) != 2 {
		t.Errorf("References len = %d, want 2", len(ni.References))
	}
	if len(ni.Sig) != 1 {
		t.Errorf("Sig len = %d, want 1", len(ni.Sig))
	}
}

func TestParseNoneCompression(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nURL: nar/abc.nar\nCompression: none\n"
	ni, err := narinfo.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ni.Compression != "none" {
		t.Errorf("Compression = %q, want none", ni.Compression)
	}
}

func TestParseMultipleReferences(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nReferences: pkg-a pkg-b pkg-c pkg-d\n"
	ni, err := narinfo.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(ni.References) != 4 {
		t.Errorf("References = %v, want 4 entries", ni.References)
	}
}

func TestParseEmptyReferences(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nReferences: \n"
	ni, err := narinfo.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(ni.References) != 0 {
		t.Errorf("References = %v, want empty", ni.References)
	}
}

func TestParseMultipleSigs(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nSig: key1:aaa=\nSig: key2:bbb=\n"
	ni, err := narinfo.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(ni.Sig) != 2 {
		t.Errorf("Sig len = %d, want 2", len(ni.Sig))
	}
	if ni.Sig[0] != "key1:aaa=" || ni.Sig[1] != "key2:bbb=" {
		t.Errorf("Sig = %v", ni.Sig)
	}
}

func TestParseMissingStorePath(t *testing.T) {
	input := "URL: nar/abc.nar\nNarHash: sha256:abc\n"
	_, err := narinfo.Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected error for missing StorePath")
	}
}

func TestParseMalformedLine(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nbadline\n"
	_, err := narinfo.Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected error for malformed line")
	}
}

func TestParseNarSizeOverflow(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nNarSize: 18446744073709551615\n"
	ni, err := narinfo.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ni.NarSize != 18446744073709551615 {
		t.Errorf("NarSize = %d", ni.NarSize)
	}
}

func TestParseDeriverCA(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nDeriver: abc-drv\nCA: fixed:r:sha256:abc\n"
	ni, err := narinfo.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ni.Deriver != "abc-drv" {
		t.Errorf("Deriver = %q", ni.Deriver)
	}
	if ni.CA != "fixed:r:sha256:abc" {
		t.Errorf("CA = %q", ni.CA)
	}
}

func TestParseIgnoresBlankLines(t *testing.T) {
	input := "\n\nStorePath: /nix/store/abc-test\n\nNarHash: sha256:abc\n\n"
	ni, err := narinfo.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ni.StorePath == "" {
		t.Error("StorePath should be set")
	}
}

func TestParseInvalidNarSize(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nNarSize: not-a-number\n"
	_, err := narinfo.Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected error for invalid NarSize")
	}
}

func TestParseInvalidFileSize(t *testing.T) {
	input := "StorePath: /nix/store/abc-test\nFileSize: not-a-number\n"
	_, err := narinfo.Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected error for invalid FileSize")
	}
}

// Fingerprint and signature verification
func TestFingerprint(t *testing.T) {
	ni := &narinfo.NarInfo{
		StorePath:  "/nix/store/s66mzxpvicwklp6cpph4dc53k5l6bfhe-hello-2.12.1",
		NarHash:    "sha256:04rrn5x6lnzrfkcy3bh7gf7x6hq3w1kap4wdss2n6n4s19pgbkr7",
		NarSize:    226512,
		References: []string{"s66mzxpvicwklp6cpph4dc53k5l6bfhe-hello-2.12.1"},
	}
	fp := ni.Fingerprint()
	want := "1;/nix/store/s66mzxpvicwklp6cpph4dc53k5l6bfhe-hello-2.12.1;" +
		"sha256:04rrn5x6lnzrfkcy3bh7gf7x6hq3w1kap4wdss2n6n4s19pgbkr7;226512;" +
		"/nix/store/s66mzxpvicwklp6cpph4dc53k5l6bfhe-hello-2.12.1"
	if fp != want {
		t.Errorf("Fingerprint() =\n%q\nwant\n%q", fp, want)
	}
}

func TestFingerprintNoRefs(t *testing.T) {
	ni := &narinfo.NarInfo{
		StorePath: "/nix/store/abc-test",
		NarHash:   "sha256:abc",
		NarSize:   1234,
	}
	fp := ni.Fingerprint()
	if !strings.HasSuffix(fp, ";") {
		t.Errorf("Fingerprint with no refs should end with ';', got: %q", fp)
	}
}

func TestFingerprintRefsAlreadyPrefixed(t *testing.T) {
	ni := &narinfo.NarInfo{
		StorePath:  "/nix/store/abc-test",
		NarHash:    "sha256:abc",
		NarSize:    1234,
		References: []string{"/nix/store/dep-pkg"}, // already prefixed
	}
	fp := ni.Fingerprint()
	if strings.Contains(fp, "/nix/store//nix/store/") {
		t.Errorf("Fingerprint double-prefixed refs: %q", fp)
	}
}

func TestParsePublicKeyValid(t *testing.T) {
	name, key, err := narinfo.ParsePublicKey("cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY=")
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if name != "cache.nixos.org-1" {
		t.Errorf("name = %q", name)
	}
	if len(key) != ed25519.PublicKeySize {
		t.Errorf("key len = %d, want %d", len(key), ed25519.PublicKeySize)
	}
}

func TestParsePublicKeyMissingColon(t *testing.T) {
	_, _, err := narinfo.ParsePublicKey("no-colon-here")
	if err == nil {
		t.Error("expected error for missing ':'")
	}
}

func TestParsePublicKeyBadBase64(t *testing.T) {
	_, _, err := narinfo.ParsePublicKey("name:!!!not-base64!!!")
	if err == nil {
		t.Error("expected error for invalid base64")
	}
}

func TestParsePublicKeyWrongSize(t *testing.T) {
	// 16 bytes encoded in base64 = 24 chars with padding
	b16 := base64.StdEncoding.EncodeToString(make([]byte, 16))
	_, _, err := narinfo.ParsePublicKey("name:" + b16)
	if err == nil {
		t.Error("expected error for wrong key size (16 bytes, not 32)")
	}
}

// Generates a fresh ed25519 key, signs a narinfo fingerprint,
// embeds the signature, and verifies it. This covers the full sign/verify path.
func TestVerifyRoundtrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	ni := &narinfo.NarInfo{
		StorePath:  "/nix/store/abc123-test-pkg",
		NarHash:    "sha256:abcdef123456",
		NarSize:    98765,
		References: []string{"abc123-test-pkg"},
	}

	fp := ni.Fingerprint()
	sig := ed25519.Sign(priv, []byte(fp))
	pubKeyStr := "test-key-1:" + base64.StdEncoding.EncodeToString(pub)
	ni.Sig = []string{"test-key-1:" + base64.StdEncoding.EncodeToString(sig)}

	ok, err := ni.Verify(pubKeyStr)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !ok {
		t.Error("Verify returned false for valid signature")
	}
}

func TestVerifyWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader) // different key

	ni := &narinfo.NarInfo{
		StorePath: "/nix/store/abc123-test-pkg",
		NarHash:   "sha256:abcdef",
		NarSize:   1234,
	}
	fp := ni.Fingerprint()
	sig := ed25519.Sign(priv, []byte(fp))
	// Register wrong public key but correct key name
	wrongKeyStr := "test-key-1:" + base64.StdEncoding.EncodeToString(wrongPub)
	ni.Sig = []string{"test-key-1:" + base64.StdEncoding.EncodeToString(sig)}

	ok, err := ni.Verify(wrongKeyStr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("Verify should return false for mismatched key")
	}
}

func TestVerifyNoMatchingKeyName(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	ni := &narinfo.NarInfo{
		StorePath: "/nix/store/abc123-test-pkg",
		NarHash:   "sha256:abcdef",
		NarSize:   1234,
	}
	ni.Sig = []string{"other-key-1:invalidsig="}
	pubKeyStr := "my-key-1:" + base64.StdEncoding.EncodeToString(pub)

	ok, err := ni.Verify(pubKeyStr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("Verify should return false when no Sig line matches key name")
	}
}
