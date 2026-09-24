package git

import "os"

// writeFileAtomic is a thin os.WriteFile wrapper so the
// test file doesn't need a writeFile helper of its own.
// Inlined here to avoid an awkward import cycle should this
// file need other helpers in the future.
func writeFileAtomic(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
