// Package version carries what this build calls itself.
//
// A package variable rather than something passed around: it is set by the
// linker at build time, so there is exactly one value and nothing to thread
// through constructors that have no other use for it.
package version

// Current is the release this binary was built from, such as "1.1.2".
//
// Stamped by the build:
//
//	go build -ldflags "-X github.com/schmidt-gabriel/homesync/server/internal/version.Current=1.1.2"
//
// A build with no tag behind it keeps "local", which is what tells someone
// reading the admin page that they are looking at a binary built from a working
// copy rather than at a released image.
var Current = "local"
