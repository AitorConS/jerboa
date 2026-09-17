package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	pkg "github.com/AitorConS/jerboa/internal/package"
)

// sizeGroup is one line of a build size report.
type sizeGroup struct {
	Group string `json:"group"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

// sizeReport breaks the files that will enter an image down into groups a
// user can act on: the program, each runtime package, each npm module or
// Python distribution, and each other top-level project path.
type sizeReport struct {
	TotalBytes int64       `json:"total_bytes"`
	TotalFiles int         `json:"total_files"`
	Groups     []sizeGroup `json:"groups"`
}

// distInfoSuffix strips wheel metadata suffixes so "flask-3.0.3.dist-info"
// groups with "flask".
var distInfoSuffix = regexp.MustCompile(`-[0-9][^/]*\.(dist-info|egg-info)$|\.(dist-info|egg-info)$`)

// sizeGroupKey classifies one image file.
func sizeGroupKey(f pkg.File) string {
	if !f.FromContext {
		if f.Reference != nil {
			name := f.Reference.Name
			if f.Reference.Version != "" {
				name += ":" + f.Reference.Version
			}
			return "package " + name
		}
		return "package files"
	}
	rel := strings.Trim(filepath.ToSlash(f.GuestPath), "/")
	segs := strings.Split(rel, "/")
	// The innermost node_modules decides the owning module, so nested
	// dependencies are charged to the module that is actually installed.
	for i := len(segs) - 2; i >= 0; i-- {
		if segs[i] != "node_modules" {
			continue
		}
		mod := segs[i+1]
		if strings.HasPrefix(mod, "@") && i+2 < len(segs) {
			mod += "/" + segs[i+2]
		}
		return "npm " + mod
	}
	for i := 0; i < len(segs)-1; i++ {
		if segs[i] == "site-packages" || (i == 0 && segs[i] == "packages") {
			dist := strings.ToLower(distInfoSuffix.ReplaceAllString(segs[i+1], ""))
			dist = strings.TrimSuffix(dist, ".py")
			return "python " + strings.ReplaceAll(dist, "-", "_")
		}
	}
	if len(segs) > 1 {
		return segs[0] + "/"
	}
	return rel
}

// buildSizeReport stats every file (the program included) and groups sizes.
func buildSizeReport(binaryPath string, files []pkg.File) (sizeReport, error) {
	byGroup := map[string]*sizeGroup{}
	var r sizeReport
	add := func(key, hostPath string) error {
		st, err := os.Stat(hostPath)
		if err != nil {
			return fmt.Errorf("size report: %w", err)
		}
		g := byGroup[key]
		if g == nil {
			g = &sizeGroup{Group: key}
			byGroup[key] = g
		}
		g.Bytes += st.Size()
		g.Files++
		r.TotalBytes += st.Size()
		r.TotalFiles++
		return nil
	}
	if binaryPath != "" {
		if err := add("program "+path.Base(filepath.ToSlash(binaryPath)), binaryPath); err != nil {
			return r, err
		}
	}
	for _, f := range files {
		if f.IsDir || f.HostPath == "" || f.HostPath == binaryPath {
			continue
		}
		if err := add(sizeGroupKey(f), f.HostPath); err != nil {
			return r, err
		}
	}
	for _, g := range byGroup {
		r.Groups = append(r.Groups, *g)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		if r.Groups[i].Bytes != r.Groups[j].Bytes {
			return r.Groups[i].Bytes > r.Groups[j].Bytes
		}
		return r.Groups[i].Group < r.Groups[j].Group
	})
	return r, nil
}

// sizeReportTopN bounds the text report; the JSON report lists every group.
const sizeReportTopN = 20

// writeSizeReport renders r as a table ("text") or JSON ("json").
func writeSizeReport(w io.Writer, r sizeReport, format string) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Fprintf(w, "Image contents: %s in %d files (uncompressed file sizes)\n", formatSize(r.TotalBytes), r.TotalFiles)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "SIZE\tSHARE\tFILES\t GROUP")
	var shown int64
	for i, g := range r.Groups {
		if i == sizeReportTopN {
			break
		}
		shown += g.Bytes
		fmt.Fprintf(tw, "%s\t%s\t%d\t %s\n", formatSize(g.Bytes), share(g.Bytes, r.TotalBytes), g.Files, g.Group)
	}
	if rest := len(r.Groups) - sizeReportTopN; rest > 0 {
		fmt.Fprintf(tw, "%s\t%s\t\t %d more groups\n", formatSize(r.TotalBytes-shown), share(r.TotalBytes-shown, r.TotalBytes), rest)
	}
	return tw.Flush()
}

func share(part, total int64) string {
	if total == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(part)/float64(total))
}
