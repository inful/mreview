package config

import "os"

// osWriteFile is a package-level indirection so tests can stub
// the filesystem call if needed (current tests don't, but having
// the indirection means future tests can swap implementations).
var osWriteFile = os.WriteFile
