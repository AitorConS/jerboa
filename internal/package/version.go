package pkg

import (
	"sort"
	"strconv"
	"strings"
)

// CompareVersions orders two package versions: negative when a < b, zero when
// equal, positive when a > b. A leading "v" is ignored. Versions are split into
// numeric and non-numeric runs; numeric runs compare as numbers ("20.11" >
// "20.9") and other runs compare as text. When one version is a prefix of the
// other, the longer one is greater ("20.11.1" > "20.11"), except that a
// pre-release suffix after "-" sorts before the release ("1.0-rc1" < "1.0").
func CompareVersions(a, b string) int {
	ra, pa := splitPrerelease(strings.TrimPrefix(a, "v"))
	rb, pb := splitPrerelease(strings.TrimPrefix(b, "v"))
	if c := compareRuns(versionRuns(ra), versionRuns(rb)); c != 0 {
		return c
	}
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	return compareRuns(versionRuns(pa), versionRuns(pb))
}

func splitPrerelease(v string) (release, pre string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// versionRuns splits v into alternating digit and non-digit runs, dropping
// separators.
func versionRuns(v string) []string {
	var runs []string
	cur := strings.Builder{}
	digit := false
	flush := func() {
		if cur.Len() > 0 {
			runs = append(runs, cur.String())
			cur.Reset()
		}
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			if !digit {
				flush()
			}
			digit = true
			cur.WriteRune(r)
		case r == '.' || r == '_' || r == '+':
			flush()
			digit = false
		default:
			if digit {
				flush()
			}
			digit = false
			cur.WriteRune(r)
		}
	}
	flush()
	return runs
}

func compareRuns(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		na, ea := strconv.ParseUint(a[i], 10, 64)
		nb, eb := strconv.ParseUint(b[i], 10, 64)
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		case ea == nil:
			return 1 // a number outranks text in the same position
		case eb == nil:
			return -1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return len(a) - len(b)
}

// VersionMatch reports how version have satisfies the requested want: 2 for an
// exact match, 1 when want is a leading part of have ("20" matches "20.11.0"
// and "v20.11.0", never "200.1"), 0 otherwise. An empty or "latest" want
// matches every version as a prefix.
func VersionMatch(want, have string) int {
	if want == "" || want == "latest" {
		return 1
	}
	w := strings.TrimPrefix(want, "v")
	h := strings.TrimPrefix(have, "v")
	switch {
	case w == h:
		return 2
	case strings.HasPrefix(h, w+"."), strings.HasPrefix(h, w+"-"):
		return 1
	}
	return 0
}

// rankByVersion returns the packages matching want, best first: exact matches
// before prefix matches, and within each group the highest version first. For
// an empty or "latest" request, releases rank above pre-releases ("2.0-rc1"),
// which are only chosen when nothing else exists or when asked for by name.
func rankByVersion(pkgs []Package, want string, keep func(Package) bool) []Package {
	floating := want == "" || want == "latest"
	type ranked struct {
		p     Package
		score int
	}
	var out []ranked
	for _, p := range pkgs {
		if keep != nil && !keep(p) {
			continue
		}
		if s := VersionMatch(want, p.Version); s > 0 {
			out = append(out, ranked{p, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if floating {
			pi, pj := isPrerelease(out[i].p.Version), isPrerelease(out[j].p.Version)
			if pi != pj {
				return !pi
			}
		}
		return CompareVersions(out[i].p.Version, out[j].p.Version) > 0
	})
	res := make([]Package, len(out))
	for i := range out {
		res[i] = out[i].p
	}
	return res
}

func isPrerelease(v string) bool {
	_, pre := splitPrerelease(strings.TrimPrefix(v, "v"))
	return pre != ""
}
