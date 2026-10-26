package pkg

import "errors"

// Sentinel errors for integrity failures during package/ops downloads. They are
// exported so callers and tests can classify a failure with errors.Is rather
// than matching on message text: an integrity failure (corrupt or tampered
// artifact) is a distinct, security-relevant outcome from a transport error.
var (
	// ErrChecksumMismatch is returned when a downloaded artifact's SHA-256 does
	// not match the value advertised in the index/manifest.
	ErrChecksumMismatch = errors.New("sha256 mismatch")
	// ErrSizeMismatch is returned when a downloaded artifact's byte count does
	// not match the size advertised in the index.
	ErrSizeMismatch = errors.New("size mismatch")
	// ErrChecksumMissing is returned when the manifest gives no SHA-256 for an
	// artifact that is about to be downloaded: without one the download could
	// not be checked, so it is not made.
	ErrChecksumMissing = errors.New("no sha256 to check the download against")
)
