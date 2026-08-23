package main

import (
	"crypto/rand"
	"math/big"
)

// fsid.go generates the filesystem_id Synapse assigns to remote media.
//
// This must match synapse/util/stringutils.py's random_string(24), which
// Synapse calls at media_repository.py:909 and :1031:
//
//	return "".join(secrets.choice(string.ascii_letters) for _ in range(length))
//
// Note the alphabet is letters only -- Python's string.ascii_letters is
// A-Za-z with no digits. Including digits would still work day to day, but it
// would make our rows visibly distinguishable from Synapse's, which defeats the
// point of being a drop-in.

const fsidAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

const fsidLength = 24

// newFilesystemID returns a fresh random filesystem_id.
//
// It panics if the system random source fails: continuing with a predictable
// ID would let a remote server guess where its media lands on disk.
func newFilesystemID() string {
	buf := make([]byte, fsidLength)
	max := big.NewInt(int64(len(fsidAlphabet)))
	for i := range buf {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("reading random bytes for filesystem_id: " + err.Error())
		}
		buf[i] = fsidAlphabet[n.Int64()]
	}
	return string(buf)
}
