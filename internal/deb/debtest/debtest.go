// Package debtest builds minimal .deb files for tests in other packages.
package debtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
)

// Build returns a .deb with the given control fields, a gzip control.tar
// and a data.tar holding one file whose content is payload. Different
// payloads give different files for the same control.
func Build(name, version, arch, payload string) []byte {
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.com>\nDescription: test package %s\n",
		name, version, arch, name)
	var ctl bytes.Buffer
	zw := gzip.NewWriter(&ctl)
	writeTar(zw, "./control", control)
	zw.Close()
	var data bytes.Buffer
	writeTar(&data, "./usr/share/doc/"+name+"/payload", payload)

	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		data []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", ctl.Bytes()},
		{"data.tar", data.Bytes()},
	} {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		b.Write(m.data)
		if len(m.data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

func writeTar(w interface{ Write([]byte) (int, error) }, name, content string) {
	tw := tar.NewWriter(w)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
	tw.Write([]byte(content))
	tw.Close()
}
