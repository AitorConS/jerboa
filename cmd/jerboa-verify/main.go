// Command jerboa-verify verifies a local manifest against the embedded release key.
package main

import (
	"fmt"
	"os"

	"github.com/AitorConS/jerboa/internal/release"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: jerboa-verify MANIFEST")
		os.Exit(2)
	}
	key, err := release.ParsePublicKey(release.PublicKeyB64)
	if err == nil {
		var data, signature []byte
		data, err = os.ReadFile(os.Args[1])
		if err == nil {
			signature, err = os.ReadFile(os.Args[1] + ".minisig")
		}
		if err == nil {
			err = key.Verify(data, signature)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
