package config

import (
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ByteSize is a size in bytes, written in the file as a plain number or with
// a unit: "2GiB", "500MB", "64KiB".
type ByteSize int64

var sizeUnits = []struct {
	suffix string
	factor int64
}{
	// Longest suffixes first, so "GiB" is not read as "B".
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
	{"B", 1},
}

// ParseByteSize parses a size such as "2GiB" or "1048576".
func ParseByteSize(s string) (ByteSize, error) {
	t := strings.TrimSpace(s)
	factor := int64(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(t, u.suffix) {
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			factor = u.factor
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q (use a number with an optional unit such as MiB or GiB)", s)
	}
	if n > 0 && factor > (1<<63-1)/n {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return ByteSize(n * factor), nil
}

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}
