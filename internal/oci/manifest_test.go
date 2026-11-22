package oci

import (
	"errors"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, n := range []string{"tool", "team/tool", "a.b/c_d/e-f", "a__b", "x/y/z"} {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", "Tool", "a//b", "/a", "a/", "a/blobs", "manifests/x", "a/tags/b", "-a", strings.Repeat("a", 256)} {
		if ValidName(n) == nil {
			t.Errorf("ValidName(%q) accepted", n)
		}
	}
}

func TestParseManifest(t *testing.T) {
	cfg := `{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + strings.Repeat("a", 64) + `","size":1}`
	layer := `{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:` + strings.Repeat("b", 64) + `","size":1}`
	foreign := `{"mediaType":"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip","digest":"sha256:` + strings.Repeat("c", 64) + `","size":1,"urls":["https://x"]}`
	tests := []struct {
		name, body, contentType string
		wantType                string
		wantBlobs               int
		wantErr                 string
	}{
		{"oci by header", `{"schemaVersion":2,"config":` + cfg + `,"layers":[` + layer + `]}`, MediaOCIManifest, MediaOCIManifest, 2, ""},
		{"oci inferred", `{"schemaVersion":2,"config":` + cfg + `}`, "", MediaOCIManifest, 1, ""},
		{"docker", `{"schemaVersion":2,"mediaType":"` + MediaDockerManifest + `","config":` + cfg + `,"layers":[` + layer + `,` + foreign + `]}`, MediaDockerManifest, MediaDockerManifest, 2, ""},
		{"mismatch", `{"schemaVersion":2,"mediaType":"` + MediaDockerManifest + `","config":` + cfg + `}`, MediaOCIManifest, "", 0, "does not match"},
		{"schema 1", `{"schemaVersion":1,"name":"x"}`, "application/vnd.docker.distribution.manifest.v1+prettyjws", "", 0, "unsupported"},
		{"no config", `{"schemaVersion":2,"mediaType":"` + MediaOCIManifest + `"}`, "", "", 0, "no config"},
		{"sha512", `{"schemaVersion":2,"config":{"digest":"sha512:abc","size":1}}`, MediaOCIManifest, "", 0, "only sha256"},
		{"not json", `nope`, MediaOCIManifest, "", 0, "not valid JSON"},
		{"index", `{"schemaVersion":2,"mediaType":"` + MediaOCIIndex + `","manifests":[]}`, "", MediaOCIIndex, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tt.body), tt.contentType)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.MediaType != tt.wantType || len(m.Blobs()) != tt.wantBlobs {
				t.Errorf("type %s, %d blobs", m.MediaType, len(m.Blobs()))
			}
		})
	}
	if _, err := ParseManifest([]byte(`{"schemaVersion":1}`), "application/vnd.docker.distribution.manifest.v1+json"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("schema 1: %v", err)
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`)
	if scheme != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" ||
		p["scope"] != "repository:library/alpine:pull" {
		t.Errorf("%s %v", scheme, p)
	}
	if pullScope("/v2/library/alpine/manifests/3") != "repository:library/alpine:pull" {
		t.Error(pullScope("/v2/library/alpine/manifests/3"))
	}
}
