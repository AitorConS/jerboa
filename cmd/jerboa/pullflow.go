package main

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/attest"
	"github.com/AitorConS/jerboa/internal/registry/transfer"
)

// pullOptions is what a pull, from jerboa pull or from a run that needs the
// image, is asked for.
type pullOptions struct {
	platform string
	// identity and verify are jerboa pull's --identity and --verify.
	identity string
	verify   bool
}

// pullImage pulls ref through the daemon, as jerboa pull does: the tag is
// resolved and its signatures checked before the daemon fetches anything,
// and the daemon pulls the recipe that was checked. Each step is a line
// with a spinner on a terminal, and a plain line otherwise; the result ends
// with the image's name and its disk digest.
func pullImage(cmd *cobra.Command, endpoint string, arg string, o pullOptions) (api.ImagePullResult, error) {
	out := cmd.OutOrStdout()
	ref, err := naming.ParseImageReference(arg)
	if err != nil {
		return api.ImagePullResult{}, err
	}
	v, err := verificationFor(ref, o.identity, o.verify)
	if err != nil {
		return api.ImagePullResult{}, err
	}
	c, err := api.Dial(endpoint)
	if err != nil {
		return api.ImagePullResult{}, fmt.Errorf("connect to daemon: %w", err)
	}
	defer func() { _ = c.Close() }()

	fmt.Fprintf(out, "Pulling %s\n", termField(ref.String()))
	sp := newSpinner(out, false)

	sp.Start("Resolving " + termField(ref.String()))
	subject, err := resolveSubject(cmd.Context(), ref, o.platform)
	if err != nil {
		sp.Fail("Resolving " + termField(ref.String()))
		return api.ImagePullResult{}, err
	}
	sp.Done("Resolved to " + termField(shortDigest(subject)))

	// Verified before the daemon fetches anything, and the daemon pulls the
	// recipe that verified, not what the tag names a moment later: an image
	// that fails the check never reaches the store. What the check prints
	// (a signer it starts remembering) waits until the spinner line is done.
	sp.Start("Checking signatures")
	var notes bytes.Buffer
	results, err := verifySubject(cmd.Context(), &notes, ref, subject, v)
	if err != nil {
		sp.Fail("Checking signatures")
		return api.ImagePullResult{}, fmt.Errorf("%w; nothing was pulled", err)
	}
	if results == nil {
		sp.Done("Not signed: pulled without a signature check")
	} else {
		head, detail := signatureSummary(results)
		sp.Done("Signed by " + head)
		if detail != "" {
			fmt.Fprintf(out, "      %s\n", detail)
		}
	}
	if notes.Len() > 0 {
		for _, line := range strings.Split(strings.TrimRight(notes.String(), "\n"), "\n") {
			fmt.Fprintf(out, "      %s\n", line)
		}
	}

	start := time.Now()
	label := "Downloading files and assembling the disk"
	sp.Start(label)
	stop := make(chan struct{})
	if sp.tty {
		// The daemon reports nothing until the pull is done: the time spent
		// shows that it is still working.
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					sp.Update(fmt.Sprintf("%s (%s)", label, time.Since(start).Round(time.Second)))
				}
			}
		}()
	}
	res, err := c.ImagePull(cmd.Context(), api.ImagePullParams{Reference: arg, NoCount: transfer.DoNotTrack(), Platform: o.platform, Recipe: subject})
	close(stop)
	if err != nil {
		sp.Fail(label)
		return api.ImagePullResult{}, err
	}
	// A daemon older than this CLI resolves the tag itself.
	if res.Recipe != subject {
		sp.Fail(label)
		err := fmt.Errorf("the daemon pulled %s, not %s, the recipe that was checked: update the daemon", res.Recipe, subject)
		if rmErr := c.ImageRemove(cmd.Context(), res.Image.Name+":"+res.Image.Tag); rmErr != nil {
			return api.ImagePullResult{}, fmt.Errorf("%w (and removing the pulled image failed: %w)", err, rmErr)
		}
		return api.ImagePullResult{}, fmt.Errorf("%w; the pulled image was removed", err)
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	if res.BlobsFetched == 0 {
		sp.Done(fmt.Sprintf("Up to date: all %d files were already here", res.Blobs))
	} else {
		sp.Done(fmt.Sprintf("Downloaded %d of %d files (%s) in %d requests, %s", res.BlobsFetched, res.Blobs, humanBytes(res.BytesFetched), res.Requests, took))
	}
	fmt.Fprintf(out, "Pulled %s:%s (%s)\nDigest: %s\n", termField(res.Image.Name), termField(res.Image.Tag), termField(res.Image.Platform), termField(res.Image.DiskDigest))
	return res, nil
}

// signatureSummary describes the attestations that verified in two lines:
// who signed (repository, workflow file and branch), then what was signed
// and the job that signed it. Every attestation of a pull is signed by the
// same workflow, which the check required; a signer that does not parse as
// a GitHub workflow is shown as it is.
func signatureSummary(results []attest.Result) (string, string) {
	kinds := make([]string, 0, len(results))
	for _, r := range results {
		kinds = append(kinds, predicateName(r.PredicateType))
	}
	r := results[len(results)-1]
	head := termField(describeSigner(r.Signer))
	var parts []string
	parts = append(parts, strings.Join(kinds, " and "))
	if r.Commit != "" {
		c := r.Commit
		if len(c) > 12 {
			c = c[:12]
		}
		parts = append(parts, "commit "+termField(c))
	}
	if r.Run != "" {
		parts = append(parts, termField(describeRun(r.Run)))
	}
	return head, strings.Join(parts, " · ")
}

// describeSigner turns a workflow identity such as
// https://github.com/owner/repo/.github/workflows/publish.yml@refs/heads/main
// into "owner/repo (publish.yml on main)".
func describeSigner(signer string) string {
	u, err := url.Parse(signer)
	if err != nil || u.Host != "github.com" {
		return signer
	}
	p, ref, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "@")
	owner, rest, ok := strings.Cut(p, "/")
	if !ok {
		return signer
	}
	repo, workflow, ok := strings.Cut(rest, "/.github/workflows/")
	if !ok || workflow == "" {
		return signer
	}
	where := path.Base(workflow)
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		where += " on " + strings.TrimPrefix(ref, "refs/heads/")
	case strings.HasPrefix(ref, "refs/tags/"):
		where += " at " + strings.TrimPrefix(ref, "refs/tags/")
	case ref != "":
		where += " at " + ref
	}
	return fmt.Sprintf("%s/%s (%s)", owner, repo, where)
}

// describeRun shortens a GitHub Actions run URL to "run <id>", with its
// attempt when it is not the first; anything else is shown as it is.
func describeRun(run string) string {
	u, err := url.Parse(run)
	if err != nil || u.Host != "github.com" {
		return run
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] != "runs" {
			continue
		}
		s := "run " + parts[i+1]
		if i+3 < len(parts) && parts[i+2] == "attempts" && parts[i+3] != "1" {
			s += " (attempt " + parts[i+3] + ")"
		}
		return s
	}
	return run
}
