package narinfo

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Parsed representation of a Nix narinfo file.
type NarInfo struct {
	StorePath   string
	URL         string
	Compression string
	FileHash    string
	FileSize    uint64
	NarHash     string
	NarSize     uint64
	References  []string
	Deriver     string
	Sig         []string
	CA          string
}

// Parses a narinfo from r. Returns error on malformed input or missing StorePath.
func Parse(r io.Reader) (*NarInfo, error) {
	ni := &NarInfo{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			return nil, fmt.Errorf("malformed line: %q", line)
		}
		switch k {
		case "StorePath":
			ni.StorePath = v
		case "URL":
			ni.URL = v
		case "Compression":
			ni.Compression = v
		case "FileHash":
			ni.FileHash = v
		case "FileSize":
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("FileSize: %w", err)
			}
			ni.FileSize = n
		case "NarHash":
			ni.NarHash = v
		case "NarSize":
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("NarSize: %w", err)
			}
			ni.NarSize = n
		case "References":
			if v != "" {
				ni.References = strings.Fields(v)
			}
		case "Deriver":
			ni.Deriver = v
		case "Sig":
			ni.Sig = append(ni.Sig, v)
		case "CA":
			ni.CA = v
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if ni.StorePath == "" {
		return nil, fmt.Errorf("missing StorePath")
	}
	return ni, nil
}
