// Package image holds the job image's build context, embedded in the binary
// so the box needs nothing but boxrunner to build it.
package image

import "embed"

//go:embed Dockerfile docker entrypoint.sh node.boxrunner
var Context embed.FS
