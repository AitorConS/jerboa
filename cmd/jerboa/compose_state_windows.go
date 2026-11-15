package main

import "os"

// checkStateFileOwner has nothing to check on Windows, where file mode bits
// do not describe who may write the file; the state's content is still
// checked against its project.
func checkStateFileOwner(os.FileInfo) error { return nil }
