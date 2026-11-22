package rpm

import (
	"slices"

	"github.com/avesst/garner/internal/config"
)

// multilib maps an architecture to the architectures whose packages its
// repository also carries. dnf on an x86_64 host installs i686 packages,
// but only from the x86_64 repository ($basearch); Fedora and EL ship
// multilib packages there too. No host runs i686 itself.
var multilib = map[string][]string{"x86_64": {"i686"}}

// repoArches returns the architectures that get a repository: the
// configured ones, except those another configured one carries.
func repoArches(c *config.Config) []string {
	var out []string
	for _, a := range c.RPM.Architectures {
		if carrier(c, a) == "" {
			out = append(out, a)
		}
	}
	return out
}

// carrier returns the configured architecture whose repository carries
// arch's packages, or "" if arch has its own.
func carrier(c *config.Config, arch string) string {
	for base, extra := range multilib {
		if slices.Contains(extra, arch) && slices.Contains(c.RPM.Architectures, base) {
			return base
		}
	}
	return ""
}

// inRepo reports whether a package of pkgArch is listed in the repository
// of repoArch.
func inRepo(c *config.Config, pkgArch, repoArch string) bool {
	return pkgArch == repoArch || pkgArch == "noarch" || carrier(c, pkgArch) == repoArch
}
