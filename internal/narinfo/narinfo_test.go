package narinfo_test

import (
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
	// uint64 max: 18446744073709551615 — verify it parses correctly
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
