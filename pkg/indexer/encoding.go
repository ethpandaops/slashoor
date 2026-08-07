package indexer

import (
	"encoding/binary"
	"fmt"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
)

// Key prefixes. Each keyspace is a single byte so ranges are easy to iterate
// and range-delete. Keys are big-endian for sorted iteration.
const (
	prefixMeta      byte = 'm' // metadata (config, cursor)
	prefixMinChunk  byte = 'n' // min-target chunks
	prefixMaxChunk  byte = 'x' // max-target chunks
	prefixDict      byte = 'd' // per-epoch attestation-data dictionary
	prefixGroup     byte = 'a' // per-(epoch, validator-chunk) attester refs
	prefixProposer  byte = 'p' // proposer block roots by (slot, proposer)
	prefixSubmitted byte = 's' // submitted/observed offense markers
	prefixSlashed   byte = 'v' // locally mirrored slashed validator set
)

// chunkKey builds a key for a min/max chunk identified by its epoch-chunk and
// validator-chunk indices.
func chunkKey(prefix byte, epochChunk, valChunk uint32) []byte {
	k := make([]byte, 9)
	k[0] = prefix
	binary.BigEndian.PutUint32(k[1:5], epochChunk)
	binary.BigEndian.PutUint32(k[5:9], valChunk)
	return k
}

// dictKey builds the key for an epoch's attestation-data dictionary.
func dictKey(epoch phase0.Epoch) []byte {
	k := make([]byte, 5)
	k[0] = prefixDict
	binary.BigEndian.PutUint32(k[1:5], uint32(epoch))
	return k
}

// groupKey builds the key for a (target epoch, validator-chunk) attester group.
func groupKey(epoch phase0.Epoch, valChunk uint32) []byte {
	k := make([]byte, 9)
	k[0] = prefixGroup
	binary.BigEndian.PutUint32(k[1:5], uint32(epoch))
	binary.BigEndian.PutUint32(k[5:9], valChunk)
	return k
}

// proposerKey builds the key for a proposer's block root at a slot.
func proposerKey(slot phase0.Slot, proposer phase0.ValidatorIndex) []byte {
	k := make([]byte, 14)
	k[0] = prefixProposer
	binary.BigEndian.PutUint64(k[1:9], uint64(slot))
	putUint40(k[9:14], uint64(proposer))
	return k
}

// slashedKey builds the key for a validator's slashed marker.
func slashedKey(index phase0.ValidatorIndex) []byte {
	k := make([]byte, 6)
	k[0] = prefixSlashed
	putUint40(k[1:6], uint64(index))
	return k
}

// submittedKey builds the key for an offense submission marker.
func submittedKey(id []byte) []byte {
	k := make([]byte, 1+len(id))
	k[0] = prefixSubmitted
	copy(k[1:], id)
	return k
}

// metaKey builds a metadata key.
func metaKey(name string) []byte {
	return append([]byte{prefixMeta}, name...)
}

// putUint40 writes v as a 5-byte big-endian integer.
func putUint40(b []byte, v uint64) {
	b[0] = byte(v >> 32)
	b[1] = byte(v >> 24)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 8)
	b[4] = byte(v)
}

// DictEntry describes one distinct attestation data seen at a target epoch,
// with a locator to re-fetch the full attestation from Dora/beacon.
type DictEntry struct {
	DataRoot8 [8]byte     // first 8 bytes of hash_tree_root(AttestationData)
	Slot      phase0.Slot // block slot containing the attestation
	BlockRoot phase0.Root // block root (canonical or orphaned)
	AttIndex  uint16      // index into the block body's attestation list
}

const dictEntrySize = 8 + 8 + 32 + 2 // 50 bytes

// EpochDict is the ordered set of distinct attestation data for one target
// epoch. Refs stored in attester groups are 1-based indices into Entries;
// ref 0 means "no attestation".
type EpochDict struct {
	Entries []DictEntry
}

// Lookup returns the 1-based ref for an existing entry with the given data
// root, or 0 if not present.
func (d *EpochDict) Lookup(dataRoot8 [8]byte) uint16 {
	for i := range d.Entries {
		if d.Entries[i].DataRoot8 == dataRoot8 {
			return uint16(i + 1)
		}
	}
	return 0
}

// Intern returns a stable 1-based ref for the entry, appending it if new.
// maxDictEntries bounds growth; on overflow it returns 0 (treated as absent).
func (d *EpochDict) Intern(e DictEntry) uint16 {
	if ref := d.Lookup(e.DataRoot8); ref != 0 {
		return ref
	}
	if len(d.Entries) >= maxDictEntries {
		return 0
	}
	d.Entries = append(d.Entries, e)
	return uint16(len(d.Entries))
}

// Get returns the entry for a 1-based ref, or false if out of range.
func (d *EpochDict) Get(ref uint16) (DictEntry, bool) {
	if ref == 0 || int(ref) > len(d.Entries) {
		return DictEntry{}, false
	}
	return d.Entries[ref-1], true
}

// maxDictEntries caps distinct attestation data per epoch. Refs are uint16, so
// the hard ceiling is 65534; a healthy epoch has a few dozen.
const maxDictEntries = 65534

func encodeDict(d *EpochDict) []byte {
	buf := make([]byte, len(d.Entries)*dictEntrySize)
	off := 0
	for i := range d.Entries {
		e := &d.Entries[i]
		copy(buf[off:off+8], e.DataRoot8[:])
		binary.BigEndian.PutUint64(buf[off+8:off+16], uint64(e.Slot))
		copy(buf[off+16:off+48], e.BlockRoot[:])
		binary.BigEndian.PutUint16(buf[off+48:off+50], e.AttIndex)
		off += dictEntrySize
	}
	return buf
}

func decodeDict(buf []byte) (*EpochDict, error) {
	if len(buf)%dictEntrySize != 0 {
		return nil, fmt.Errorf("corrupt dict blob: length %d not a multiple of %d", len(buf), dictEntrySize)
	}
	n := len(buf) / dictEntrySize
	d := &EpochDict{Entries: make([]DictEntry, n)}
	off := 0
	for i := 0; i < n; i++ {
		e := &d.Entries[i]
		copy(e.DataRoot8[:], buf[off:off+8])
		e.Slot = phase0.Slot(binary.BigEndian.Uint64(buf[off+8 : off+16]))
		copy(e.BlockRoot[:], buf[off+16:off+48])
		e.AttIndex = binary.BigEndian.Uint16(buf[off+48 : off+50])
		off += dictEntrySize
	}
	return d, nil
}

// encodeGroup / decodeGroup serialize a fixed K-length uint16 ref slice.
func encodeGroup(refs []uint16) []byte {
	buf := make([]byte, len(refs)*2)
	for i, r := range refs {
		binary.BigEndian.PutUint16(buf[i*2:i*2+2], r)
	}
	return buf
}

func decodeGroup(buf []byte, k int) ([]uint16, error) {
	if len(buf) != k*2 {
		return nil, fmt.Errorf("corrupt group blob: length %d, want %d", len(buf), k*2)
	}
	refs := make([]uint16, k)
	for i := 0; i < k; i++ {
		refs[i] = binary.BigEndian.Uint16(buf[i*2 : i*2+2])
	}
	return refs, nil
}

// encodeChunk / decodeChunk serialize a uint16 cell array (min/max chunk).
func encodeChunk(cells []uint16) []byte {
	buf := make([]byte, len(cells)*2)
	for i, c := range cells {
		binary.BigEndian.PutUint16(buf[i*2:i*2+2], c)
	}
	return buf
}

func decodeChunk(buf []byte, size int) ([]uint16, error) {
	if len(buf) != size*2 {
		return nil, fmt.Errorf("corrupt chunk blob: length %d, want %d", len(buf), size*2)
	}
	cells := make([]uint16, size)
	for i := 0; i < size; i++ {
		cells[i] = binary.BigEndian.Uint16(buf[i*2 : i*2+2])
	}
	return cells, nil
}
