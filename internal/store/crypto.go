package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// At-rest formats and their sizes (research.md R4, data-model.md §3).
// Everything the package writes or parses is derived from these constants.
const (
	// keyMasterSize is the AES-256 master key length. A supplied key and the
	// contents of master.key must both decode to exactly this many bytes.
	keyMasterSize = 32

	// keyPlainSize is one Z-Wave network key: 16 bytes, 32 hex characters.
	keyPlainSize = 16

	// keyNonceSize and keyTagSize are GCM's, restated because the stored blob
	// layout names them explicitly.
	keyNonceSize = 12
	keyTagSize   = 16

	// keyVersion is the first blob byte. It exists so a later format is
	// recognised and refused rather than misread; no other value is written.
	keyVersion = 0x01

	// keyHeaderSize is the version byte plus the nonce: everything a blob
	// carries before its ciphertext.
	keyHeaderSize = 1 + keyNonceSize

	// keyMinBlobSize is the shortest blob the layout can produce — header plus
	// tag with an empty ciphertext — and therefore the ≥ 29-byte floor the
	// security_keys table documents.
	keyMinBlobSize = keyHeaderSize + keyTagSize

	// keyAADPrefix binds a blob to the format version; the security class is
	// appended to it, so a blob cannot be moved between rows (R4).
	keyAADPrefix = "zwave-hub/key/v1|"

	// keyFileName is the fallback master key inside the data directory, and
	// keyFileMode / keyDirMode are its permissions and the directory's.
	keyFileName = "master.key"
	keyFileMode = 0o600
	keyDirMode  = 0o700
)

// ErrNoMasterKey reports that the Store holds no usable 32-byte master key, so
// key material can be neither sealed nor opened. SealKey and OpenKey return it
// unwrapped, so a caller may compare it directly as well as with errors.Is.
var ErrNoMasterKey = errors.New("store: no master key is configured")

// ErrKeyCorrupt reports that a stored blob cannot be parsed or does not
// authenticate under the current master key: a damaged row, a blob copied from
// another class, or a different master key. SealKey and OpenKey wrap it with
// the security class so a caller can name the affected class without parsing
// the message. No error in this file ever carries key bytes, ciphertext, a
// nonce or any part of a blob.
var ErrKeyCorrupt = errors.New("key blob is malformed or failed authentication")

// LoadMasterKey returns the 32-byte master key the store seals key material
// with. A non-empty supplied argument wins and is copied; LoadMasterKey never
// reads the environment, because config is the only package allowed to
// (constitution III). Otherwise the key lives in <dataDir>/master.key, which is
// created with mode 0600 on first use and read on every start after that.
//
// The data directory is created first (0700) so the file can always be written.
// An existing file is trimmed, decoded as standard padded or raw unpadded
// base64, and required to be exactly 32 bytes. A malformed or short file is an
// error and never a reason to generate a new key: a new key would strand every
// sealed value, so a file left truncated by a crash stays an error until an
// operator supplies the right bytes or removes it deliberately.
func LoadMasterKey(dataDir string, supplied []byte) ([]byte, error) {
	if err := os.MkdirAll(dataDir, keyDirMode); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}
	if len(supplied) > 0 {
		if len(supplied) != keyMasterSize {
			return nil, errors.New("store: supplied master key must be exactly 32 bytes")
		}
		key := make([]byte, keyMasterSize)
		copy(key, supplied)
		return key, nil
	}

	path := filepath.Join(dataDir, keyFileName)
	key, err := masterReadFile(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return masterCreateFile(path)
}

// masterReadFile reads and decodes an existing master key file. The error is
// wrapped, so errors.Is(err, fs.ErrNotExist) still identifies the one case in
// which LoadMasterKey may create the file.
func masterReadFile(path string) ([]byte, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("store: read master key file %s: %w", path, err)
	}
	key, err := keyDecode(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("store: master key file %s: %w", path, err)
	}
	return key, nil
}

// masterCreateFile generates a master key and stores it in path as standard
// padded base64 plus a trailing newline. O_EXCL is what keeps a first start
// safe against a second hub process: the loser of the race reads the winner's
// file instead of generating a key that would immediately strand the winner's
// blobs. An existing file — including one a crash left short — is never
// overwritten, because the read path reports it as malformed and only an
// operator can decide that it is disposable.
func masterCreateFile(path string) ([]byte, error) {
	key := make([]byte, keyMasterSize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("store: generate master key: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if errors.Is(err, fs.ErrExist) {
		return masterReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("store: create master key file %s: %w", path, err)
	}
	// A failed write leaves the file in place on purpose: it is unreadable, so
	// the next start reports an error instead of silently minting a new key.
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		file.Close()
		return nil, fmt.Errorf("store: write master key file %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, fmt.Errorf("store: sync master key file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("store: close master key file %s: %w", path, err)
	}
	return key, nil
}

// keyDecode turns the contents of master.key into a master key. Surrounding
// whitespace is dropped; padded standard base64 is tried first and unpadded
// second, which covers both `head -c 32 /dev/urandom | base64` output and a
// file whose trailing `=` an editor stripped. The decoded length is checked so
// a truncated file can never pass as a shorter key.
func keyDecode(encoded string) ([]byte, error) {
	text := strings.TrimSpace(encoded)
	key, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(text)
	}
	if err != nil || len(key) != keyMasterSize {
		return nil, errors.New("must be base64 encoding of exactly 32 bytes")
	}
	return key, nil
}

// SealKey encrypts one 16-byte Z-Wave key for a security class and returns the
// stored blob, laid out as [0x01][12-byte random nonce][ciphertext ‖ 16-byte
// tag] under AES-256-GCM with the store's master key. The class is the
// additional authenticated data, so a blob moved to another class's row fails
// to open rather than silently supplying that class a wrong key.
//
// The caller keeps ownership of key and is responsible for wiping it.
func (s *Store) SealKey(class string, key []byte) ([]byte, error) {
	if len(s.masterKey) != keyMasterSize {
		return nil, ErrNoMasterKey
	}
	if !keyClassValid(class) {
		return nil, fmt.Errorf("store: seal key: unknown security class %q", class)
	}
	if len(key) != keyPlainSize {
		return nil, fmt.Errorf("store: seal key for %s: key must be %d bytes, got %d", class, keyPlainSize, len(key))
	}
	return sealKeyBlob(s.masterKey, class, key)
}

// OpenKey decrypts one stored blob and returns the 16-byte Z-Wave key it holds.
// It is the inverse of SealKey: same class and master-key requirements, same
// additional authenticated data, and the same layout. A blob shorter than the
// minimum, carrying an unknown version byte, or failing GCM authentication is
// reported as ErrKeyCorrupt; the row is left untouched for a backup restore.
//
// On success the returned slice belongs to the caller, which wipes it.
func (s *Store) OpenKey(class string, blob []byte) ([]byte, error) {
	if len(s.masterKey) != keyMasterSize {
		return nil, ErrNoMasterKey
	}
	if !keyClassValid(class) {
		return nil, fmt.Errorf("store: open key: unknown security class %q", class)
	}
	return openKeyBlob(s.masterKey, class, blob)
}

// sealKeyBlob does the encryption, given a master key already known to be 32
// bytes and a class already known to be valid.
func sealKeyBlob(masterKey []byte, class string, key []byte) ([]byte, error) {
	gcm, err := keyGCM(masterKey)
	if err != nil {
		return nil, fmt.Errorf("store: seal key for %s: %w", class, err)
	}

	blob := make([]byte, keyHeaderSize, keyHeaderSize+len(key)+keyTagSize)
	blob[0] = keyVersion
	if _, err := rand.Read(blob[1:keyHeaderSize]); err != nil {
		return nil, fmt.Errorf("store: seal key for %s: generate nonce: %w", class, err)
	}
	return gcm.Seal(blob, blob[1:keyHeaderSize], key, keyAdditionalData(class)), nil
}

// openKeyBlob does the decryption, given a master key already known to be 32
// bytes and a class already known to be valid.
func openKeyBlob(masterKey []byte, class string, blob []byte) ([]byte, error) {
	if len(blob) < keyMinBlobSize || blob[0] != keyVersion {
		return nil, fmt.Errorf("store: open key for %s: %w", class, ErrKeyCorrupt)
	}
	gcm, err := keyGCM(masterKey)
	if err != nil {
		return nil, fmt.Errorf("store: open key for %s: %w", class, err)
	}

	key, err := gcm.Open(nil, blob[1:keyHeaderSize], blob[keyHeaderSize:], keyAdditionalData(class))
	if err != nil {
		return nil, fmt.Errorf("store: open key for %s: %w", class, ErrKeyCorrupt)
	}
	if len(key) != keyPlainSize {
		// Wipe before discarding: the bytes are key material even though the
		// length says the blob cannot be used.
		for i := range key {
			key[i] = 0
		}
		return nil, fmt.Errorf("store: open key for %s: plaintext is %d bytes, want %d: %w",
			class, len(key), keyPlainSize, ErrKeyCorrupt)
	}
	return key, nil
}

// keyGCM returns AES-256-GCM for a master key already known to be 32 bytes. The
// size check keeps the blob layout honest: were the standard library's GCM to
// use another nonce or tag length, sealing would produce a blob this file's own
// parser could not read, so it is an error rather than a mismatch.
func keyGCM(masterKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if gcm.NonceSize() != keyNonceSize || gcm.Overhead() != keyTagSize {
		return nil, fmt.Errorf("GCM uses a %d-byte nonce and a %d-byte tag, want %d and %d",
			gcm.NonceSize(), gcm.Overhead(), keyNonceSize, keyTagSize)
	}
	return gcm, nil
}

// keyAdditionalData is the additional authenticated data for a class. The class
// is in it so swapping two rows cannot go undetected.
func keyAdditionalData(class string) []byte {
	return []byte(keyAADPrefix + class)
}

// keyClassValid reports whether class is one of the six names the
// security_keys table accepts (data-model.md §4). It mirrors that table's CHECK
// constraint, so a class the store refuses here could never be written anyway.
func keyClassValid(class string) bool {
	switch class {
	case "S2_AccessControl", "S2_Authenticated", "S2_Unauthenticated", "S0_Legacy",
		"S2_Authenticated_LR", "S2_AccessControl_LR":
		return true
	default:
		return false
	}
}
