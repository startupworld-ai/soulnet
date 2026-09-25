package a2a

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ——— Vault: encrypted, content-addressed backup storage per mailbox ———
//
// The relay keeps, per mailbox (identity fingerprint), a bag of opaque blobs addressed by
// a client-computed 64-hex id, plus a few named "lanes" -- versioned head pointers that
// say "this manifest blob is the current state". The relay never reads a blob: ids are
// keyed hashes the client computes, contents are ciphertext. The one exception is the
// REFS blob of a head version: a plaintext list of the blob ids that version uses, so the
// relay can garbage-collect what no retained version references (it leaks nothing but a
// set of opaque ids).
//
// Refs blob format (the only blob the relay parses): one lowercase 64-hex blob id per
// line, LF-separated (a CR before the LF is tolerated), blank lines ignored, duplicates
// allowed, no other content. The root and refs blobs of a version are always retained and
// need not be listed. A refs blob is bounded by MaxVaultBlobBytes like any blob (about
// 64,000 ids). EncodeVaultRefs / ParseVaultRefs implement exactly this.
//
// See the wire spec §15 and relay/vault.go for the endpoints.

// Vault limits and lane names shared by the relay and its clients.
const (
	// MaxVaultBlobBytes caps one blob (request body of PUT /vault/{box}/blob/{id}).
	MaxVaultBlobBytes = 4 << 20
	// MaxVaultHasIDs caps the ids of one POST /vault/{box}/has query.
	MaxVaultHasIDs = 10000
	// VaultKeepVersions is how many versions per lane the garbage collector retains
	// (current + previous ones): every blob their refs name survives.
	VaultKeepVersions = 3
	// VaultMainLane is the lane only the ACTIVE device of the mailbox may advance.
	VaultMainLane = "main"
	// VaultDevLanePrefix + device id names a lane only that device may advance (a kicked
	// device parks the changes it could not hand over there).
	VaultDevLanePrefix = "dev-"
)

// Machine-readable "error" codes of the vault endpoints (both ends key on them).
const (
	VaultConflictCode = "version conflict"     // 409: prev_version != current version
	VaultQuotaCode    = "vault quota exceeded" // 413: the upload would exceed the mailbox quota
	VaultMissingCode  = "missing blobs"        // 422: the head names blobs that are not stored
	VaultBadRefsCode  = "refs blob is not a list of blob ids"
	VaultNoLaneCode   = "no such lane" // 404 of GET/DELETE /vault/{box}/head/{lane}
	VaultNoBlobCode   = "no such blob" // 404 of GET /vault/{box}/blob/{id}
)

// Sentinel errors of the vault client; test with errors.Is.
var (
	// ErrVaultConflict: the lane moved since prev_version (someone else advanced it). The
	// concrete error is a *VaultConflictError carrying the current version.
	ErrVaultConflict = errors.New("vault: head version conflict")
	// ErrVaultQuota: the mailbox's vault quota is exhausted.
	ErrVaultQuota = errors.New("vault: quota exceeded")
	// ErrVaultMissing: a head update names blobs the relay does not have (upload them
	// first). The concrete error is a *VaultMissingError.
	ErrVaultMissing = errors.New("vault: head references blobs that are not stored")
	// ErrVaultNotFound: the requested blob is not stored.
	ErrVaultNotFound = errors.New("vault: blob not found")
	// ErrVaultUnsupported: the relay has no vault endpoints (an older relay).
	ErrVaultUnsupported = errors.New("vault: relay does not support the vault")
)

// VaultConflictError is the relay's 409 CAS verdict on a head update or delete.
// errors.Is(err, ErrVaultConflict) holds.
type VaultConflictError struct {
	Lane    string
	Current int64  // current version of the lane (0 = the lane does not exist)
	Root    string // current root (lets a retrying writer recognise its own earlier write)
}

func (e *VaultConflictError) Error() string {
	return fmt.Sprintf("vault: lane %q is at version %d (root %s)", e.Lane, e.Current, e.Root)
}

// Is makes errors.Is(err, ErrVaultConflict) true.
func (e *VaultConflictError) Is(target error) bool { return target == ErrVaultConflict }

// VaultMissingError is the relay's 422 verdict that a head update names blobs it does not
// store. IDs holds the first missing ones (at most 100), Count the total.
// errors.Is(err, ErrVaultMissing) holds.
type VaultMissingError struct {
	IDs   []string
	Count int
}

func (e *VaultMissingError) Error() string {
	return fmt.Sprintf("vault: %d referenced blob(s) not stored (first: %s)", e.Count, strings.Join(e.IDs, ","))
}

// Is makes errors.Is(err, ErrVaultMissing) true.
func (e *VaultMissingError) Is(target error) bool { return target == ErrVaultMissing }

// VaultHead is one version of a lane as the relay reports it.
type VaultHead struct {
	Lane    string    `json:"lane,omitempty"`
	Version int64     `json:"version"`
	Root    string    `json:"root"`             // id of the manifest blob (ciphertext)
	Refs    string    `json:"refs"`             // id of the plaintext refs blob (see the package notes above)
	Device  string    `json:"device,omitempty"` // device that wrote this version ("" = legacy client)
	Updated time.Time `json:"updated"`
}

// VaultUsage is the storage a mailbox's vault occupies.
type VaultUsage struct {
	Bytes int64 `json:"bytes"`
	Blobs int64 `json:"blobs"`
	Quota int64 `json:"quota"`
}

var vaultIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// vaultLaneRe: "main" or "dev-" + a device id (same alphabet and length as the relay's
// ValidDeviceID; relay/vault_test.go pins the two together).
var vaultLaneRe = regexp.MustCompile(`^(main|dev-[A-Za-z0-9._~=-]{1,128})$`)

// ValidVaultID reports whether id is a well-formed blob id (64 lowercase hex characters).
func ValidVaultID(id string) bool { return vaultIDRe.MatchString(id) }

// ValidVaultLane reports whether lane is "main" or "dev-<device id>".
func ValidVaultLane(lane string) bool { return vaultLaneRe.MatchString(lane) }

// VaultDevLane returns the lane name reserved for device.
func VaultDevLane(device string) string { return VaultDevLanePrefix + device }

// EncodeVaultRefs renders ids in the refs blob format (one id per line, LF, trailing LF).
func EncodeVaultRefs(ids []string) []byte {
	var b bytes.Buffer
	for _, id := range ids {
		b.WriteString(id)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ParseVaultRefs parses a refs blob; any line that is not a blob id is an error.
func ParseVaultRefs(data []byte) ([]string, error) {
	var ids []string
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			continue
		}
		id := string(line)
		if !ValidVaultID(id) {
			return nil, fmt.Errorf("refs line %d is not a blob id", i+1)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
