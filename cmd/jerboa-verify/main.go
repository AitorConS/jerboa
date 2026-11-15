// Command jerboa-verify verifies a local manifest against the embedded release key.
//
// The release key signs several kinds of document, told apart by the
// signature's trusted comment. -purpose P also requires that comment to be P
// or to start with "P:", so a document signed for something else (a release
// manifest offered as the package index, say) is refused.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/AitorConS/jerboa/internal/release"
)

func main() {
	purpose := flag.String("purpose", "", "require the trusted comment to be PURPOSE or start with PURPOSE:")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: jerboa-verify [-purpose PURPOSE] MANIFEST")
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)
	key, err := release.ParsePublicKey(release.PublicKeyB64)
	if err == nil {
		var data, signature []byte
		data, err = os.ReadFile(path)
		if err == nil {
			signature, err = os.ReadFile(path + ".minisig")
		}
		var comment string
		if err == nil {
			comment, err = key.VerifyComment(data, signature)
		}
		if err == nil && *purpose != "" && comment != *purpose && !strings.HasPrefix(comment, *purpose+":") {
			err = fmt.Errorf("%s is signed for %q, not for %s", path, comment, *purpose)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
