/*
   Copyright 2021 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package state

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/google/btree"
	"github.com/ledgerwatch/erigon-lib/recsplit"
	"github.com/ledgerwatch/erigon-lib/types"
	"golang.org/x/crypto/sha3"

	"github.com/ledgerwatch/erigon-lib/commitment"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/cryptozerocopy"
	"github.com/ledgerwatch/erigon-lib/common/length"
)

// Defines how to evaluate commitments
type CommitmentMode uint

const (
	CommitmentModeDisabled CommitmentMode = 0
	CommitmentModeDirect   CommitmentMode = 1
	CommitmentModeUpdate   CommitmentMode = 2
)

func (m CommitmentMode) String() string {
	switch m {
	case CommitmentModeDisabled:
		return "disabled"
	case CommitmentModeDirect:
		return "direct"
	case CommitmentModeUpdate:
		return "update"
	default:
		return "unknown"
	}
}

func ParseCommitmentMode(s string) CommitmentMode {
	var mode CommitmentMode
	switch s {
	case "off":
		mode = CommitmentModeDisabled
	case "update":
		mode = CommitmentModeUpdate
	default:
		mode = CommitmentModeDirect
	}
	return mode
}

type ValueMerger func(prev, current []byte) (merged []byte, err error)

type UpdateTree struct {
	tree   *btree.BTreeG[*commitmentItem]
	keccak cryptozerocopy.KeccakState
	keys   map[string]struct{}
	mode   CommitmentMode
}

func NewUpdateTree(m CommitmentMode) *UpdateTree {
	return &UpdateTree{
		tree:   btree.NewG[*commitmentItem](64, commitmentItemLessPlain),
		keccak: sha3.NewLegacyKeccak256().(cryptozerocopy.KeccakState),
		keys:   map[string]struct{}{},
		mode:   m,
	}
}

func (t *UpdateTree) get(key []byte) (*commitmentItem, bool) {
	c := &commitmentItem{plainKey: key, update: commitment.Update{CodeHashOrStorage: commitment.EmptyCodeHashArray}}
	el, ok := t.tree.Get(c)
	if ok {
		return el, true
	}
	c.plainKey = common.Copy(c.plainKey)
	return c, false
}

// TouchPlainKey marks plainKey as updated and applies different fn for different key types
// (different behaviour for Code, Account and Storage key modifications).
func (t *UpdateTree) TouchPlainKey(key string, val []byte, fn func(c *commitmentItem, val []byte)) {
	switch t.mode {
	case CommitmentModeUpdate:
		item, _ := t.get([]byte(key))
		fn(item, val)
		t.tree.ReplaceOrInsert(item)
	case CommitmentModeDirect:
		t.keys[key] = struct{}{}
	default:
	}
}

func (t *UpdateTree) Size() uint64 {
	return uint64(len(t.keys))
}

func (t *UpdateTree) TouchAccount(c *commitmentItem, val []byte) {
	if len(val) == 0 {
		c.update.Flags = commitment.DeleteUpdate
		return
	}
	if c.update.Flags&commitment.DeleteUpdate != 0 {
		c.update.Flags ^= commitment.DeleteUpdate
	}
	nonce, balance, chash := types.DecodeAccountBytesV3(val)
	if c.update.Nonce != nonce {
		c.update.Nonce = nonce
		c.update.Flags |= commitment.NonceUpdate
	}
	if !c.update.Balance.Eq(balance) {
		c.update.Balance.Set(balance)
		c.update.Flags |= commitment.BalanceUpdate
	}
	if !bytes.Equal(chash, c.update.CodeHashOrStorage[:]) {
		if len(chash) == 0 {
			c.update.ValLength = length.Hash
			copy(c.update.CodeHashOrStorage[:], commitment.EmptyCodeHash)
		} else {
			copy(c.update.CodeHashOrStorage[:], chash)
			c.update.ValLength = length.Hash
			c.update.Flags |= commitment.CodeUpdate
		}
	}
}

func (t *UpdateTree) UpdatePrefix(prefix, val []byte, fn func(c *commitmentItem, val []byte)) {
	t.tree.AscendGreaterOrEqual(&commitmentItem{}, func(item *commitmentItem) bool {
		if !bytes.HasPrefix(item.plainKey, prefix) {
			return false
		}
		fn(item, val)
		return true
	})
}

func (t *UpdateTree) TouchStorage(c *commitmentItem, val []byte) {
	c.update.ValLength = len(val)
	if len(val) == 0 {
		c.update.Flags = commitment.DeleteUpdate
	} else {
		c.update.Flags |= commitment.StorageUpdate
		copy(c.update.CodeHashOrStorage[:], val)
	}
}

func (t *UpdateTree) TouchCode(c *commitmentItem, val []byte) {
	t.keccak.Reset()
	t.keccak.Write(val)
	t.keccak.Read(c.update.CodeHashOrStorage[:])
	if c.update.Flags == commitment.DeleteUpdate && len(val) == 0 {
		c.update.Flags = commitment.DeleteUpdate
		c.update.ValLength = 0
		return
	}
	c.update.ValLength = length.Hash
	if len(val) != 0 {
		c.update.Flags |= commitment.CodeUpdate
	}
}

// Returns list of both plain and hashed keys. If .mode is CommitmentModeUpdate, updates also returned.
// No ordering guarantees is provided.
func (t *UpdateTree) List(clear bool) ([][]byte, []commitment.Update) {
	switch t.mode {
	case CommitmentModeDirect:
		plainKeys := make([][]byte, len(t.keys))
		i := 0
		for key := range t.keys {
			plainKeys[i] = []byte(key)
			i++
		}
		slices.SortFunc(plainKeys, func(i, j []byte) int { return bytes.Compare(i, j) })
		if clear {
			t.keys = make(map[string]struct{}, len(t.keys)/8)
		}

		return plainKeys, nil
	case CommitmentModeUpdate:
		plainKeys := make([][]byte, t.tree.Len())
		updates := make([]commitment.Update, t.tree.Len())
		i := 0
		t.tree.Ascend(func(item *commitmentItem) bool {
			plainKeys[i], updates[i] = item.plainKey, item.update
			i++
			return true
		})
		if clear {
			t.tree.Clear(true)
		}
		return plainKeys, updates
	default:
		return nil, nil
	}
}

type commitmentState struct {
	txNum     uint64
	blockNum  uint64
	trieState []byte
}

func (cs *commitmentState) Decode(buf []byte) error {
	if len(buf) < 10 {
		return fmt.Errorf("ivalid commitment state buffer size %d, expected at least 10b", len(buf))
	}
	pos := 0
	cs.txNum = binary.BigEndian.Uint64(buf[pos : pos+8])
	pos += 8
	cs.blockNum = binary.BigEndian.Uint64(buf[pos : pos+8])
	pos += 8
	cs.trieState = make([]byte, binary.BigEndian.Uint16(buf[pos:pos+2]))
	pos += 2
	if len(cs.trieState) == 0 && len(buf) == 10 {
		return nil
	}
	copy(cs.trieState, buf[pos:pos+len(cs.trieState)])
	return nil
}

func (cs *commitmentState) Encode() ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	var v [18]byte
	binary.BigEndian.PutUint64(v[:], cs.txNum)
	binary.BigEndian.PutUint64(v[8:16], cs.blockNum)
	binary.BigEndian.PutUint16(v[16:18], uint16(len(cs.trieState)))
	if _, err := buf.Write(v[:]); err != nil {
		return nil, err
	}
	if _, err := buf.Write(cs.trieState); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// nolint
func decodeU64(from []byte) uint64 {
	var i uint64
	for _, b := range from {
		i = (i << 8) | uint64(b)
	}
	return i
}

func encodeU64(i uint64, to []byte) (int, []byte) {
	// writes i to b in big endian byte order, using the least number of bytes needed to represent i.
	switch {
	case i < (1 << 8):
		return 1, append(to, byte(i))
	case i < (1 << 16):
		return 2, append(to, byte(i>>8), byte(i))
	case i < (1 << 24):
		return 3, append(to, byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 32):
		return 4, append(to, byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 40):
		return 5, append(to, byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 48):
		return 6, append(to, byte(i>>40), byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 56):
		return 7, append(to, byte(i>>48), byte(i>>40), byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	default:
		return 8, append(to, byte(i>>56), byte(i>>48), byte(i>>40), byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	}
}

func encodeShortenedKey(buf []byte, stepFrom uint64, stepTo uint64, offset uint64) []byte {
	if len(buf) < 2 {
		buf = make([]byte, 2)
	}

	var s0, s1, of int
	s0, buf = encodeU64(stepFrom, buf)
	s1, buf = encodeU64(stepTo, buf)
	of, buf = encodeU64(offset, buf)

	// to put them into 3 bits each normalized to 0..7
	s0--
	s1--
	of--

	enc := uint16((s0&0x07)<<6 | (s1&0x07)<<3 | (of & 0x07))
	binary.BigEndian.PutUint16(buf[:2], enc)
	return buf
}

// Optimised key referencing a state file record (file number and offset within the file)
func decodeShortenedKey(shortened []byte) (stepFrom, stepTo, offset uint64) {
	if len(shortened) < 1 {
		return 0, 0, 0
	}

	encoded := binary.BigEndian.Uint16(shortened[:2])
	s0 := int((encoded>>6)&0x07) + 1
	s1 := int((encoded>>3)&0x07) + 1
	of := int(encoded&0x07) + 1
	// denormalize lengths

	shortened = shortened[2:]
	return decodeU64(shortened[:s0]),
		decodeU64(shortened[s0 : s0+s1]),
		decodeU64(shortened[s0+s1 : s0+s1+of])
}

type commitmentItem struct {
	plainKey []byte
	update   commitment.Update
}

func commitmentItemLessPlain(i, j *commitmentItem) bool {
	return bytes.Compare(i.plainKey, j.plainKey) < 0
}

func (dc *DomainContext) findKeyReplacement(fullKey []byte, startTxNum uint64, endTxNum uint64, idxList idxList, list ...*filesItem) (shortened []byte, found bool) {
	for _, item := range list {
		if item.startTxNum == startTxNum && item.endTxNum == endTxNum {
			g := NewArchiveGetter(item.decompressor.MakeGetter(), dc.d.compression)

			//TODO: existence filter existence should be checked for domain which filesItem list is provided, not in commitmnet
			//if idxList&withExistence != 0 {
			//	hi, lo := ic.hashKey(key)
			//	if !item.existence.ContainsHash(hi) {
			//		continue
			//	}
			//}
			var offset uint64
			if idxList&withHashMap != 0 {
				reader := recsplit.NewIndexReader(item.index)
				defer reader.Close()

				var ok bool
				if offset, ok = reader.Lookup(fullKey); !ok {
					return nil, false
				}

				g.Reset(offset)
				if !g.HasNext() {
					dc.d.logger.Warn("commitment branch key replacement seek failed",
						"key", fmt.Sprintf("%x", fullKey), "idx", "recsplit", "file", item.decompressor.FileName())
					return nil, false
				}

				k, _ := g.Next(nil)
				if !bytes.Equal(fullKey, k) {
					dc.d.logger.Warn("commitment branch key replacement seek failed",
						"key", fmt.Sprintf("%x", fullKey), "idx", "recsplit", "file", item.decompressor.FileName())

					return nil, false
				}
			}
			if idxList&withBTree != 0 {
				cur, err := item.bindex.Seek(g, fullKey)
				if err != nil {
					dc.d.logger.Warn("commitment branch key replacement seek failed",
						"key", fmt.Sprintf("%x", fullKey), "idx", "bt", "err", err, "file", item.decompressor.FileName())
				}

				if cur == nil {
					return nil, false
				}
				if !bytes.Equal(cur.Key(), fullKey) {
					return nil, false
				}
				offset = cur.offsetInFile()
			}

			stepFrom, stepTo := item.startTxNum/dc.d.aggregationStep, item.endTxNum/dc.d.aggregationStep
			shortened := encodeShortenedKey(nil, stepFrom, stepTo, offset)
			return shortened, true
		}
	}
	return nil, false
}

// searches in given list of files for a key or searches in domain files if list is empty
func (dc *DomainContext) lookupByShortenedKey(shortKey []byte, list []*filesItem) (fullKey []byte, found bool) {
	stepFrom, stepTo, offset := decodeShortenedKey(shortKey)
	txFrom, txTo := stepFrom*dc.d.aggregationStep, stepTo*dc.d.aggregationStep

	var item *filesItem
	if len(list) > 0 {
		for _, f := range list {
			if f.startTxNum == txFrom && f.endTxNum == txTo {
				item = f
				break
			}
		}
	} else {
		for _, f := range dc.files {
			if f.startTxNum == txFrom && f.endTxNum == txTo {
				item = f.src
				break
			}
		}
	}
	if item == nil {
		fileStepsss := ""
		for _, item := range dc.d.files.Items() {
			fileStepsss += fmt.Sprintf("%d-%d;", item.startTxNum/dc.d.aggregationStep, item.endTxNum/dc.d.aggregationStep)
		}
		dc.d.logger.Warn("lookupByShortenedKey file not found",
			"stepFrom", stepFrom, "stepTo", stepTo, "offset", offset,
			"domain", dc.d.keysTable, "fileSteps", fileStepsss,
			"listSize", len(list), "filesCount", dc.d.files.Len())
		return nil, false
	}

	g := NewArchiveGetter(item.decompressor.MakeGetter(), dc.d.compression)
	g.Reset(offset)
	if !g.HasNext() {
		dc.d.logger.Warn("lookupByShortenedKey failed",
			"stepFrom", stepFrom, "stepTo", stepTo, "offset", offset, "file", item.decompressor.FileName())
		return nil, false
	}

	fullKey, _ = g.Next(nil)
	// dc.d.logger.Debug(fmt.Sprintf("lookupByShortenedKey [%x]=>{%x}", shortKey, fullKey),
	// 	"stepFrom", stepFrom, "stepTo", stepTo, "offset", offset, "file", item.decompressor.FileName())
	return fullKey, true
}

// commitmentValTransform parses the value of the commitment record to extract references
// to accounts and storage items, then looks them up in the new, merged files, and replaces them with
// the updated references

func (dc *DomainContext) commitmentValTransform(
	filesAccount []*filesItem, mergedAccount *filesItem, idxListAccount idxList,
	filesStorage []*filesItem, mergedStorage *filesItem, idxListStorage idxList,
	startTxNum, endTxNum uint64,

) valueTransformer {

	return func(valBuf []byte) (transValBuf []byte, err error) {
		if !dc.d.replaceKeysInValues || len(valBuf) == 0 {
			return valBuf, nil
		}

		//type pair struct {
		//	b []byte
		//	d string
		//	o []byte
		//}

		//seen := make(map[string]struct{})
		//shortens := make(map[string]pair)

		return commitment.BranchData(valBuf).
			ReplacePlainKeysIter(nil, func(key []byte, isStorage bool) []byte {
				var found bool
				//if _, ok := seen[string(key)]; ok {
				//	fmt.Printf("key %x already seen\n", key)
				//}
				//seen[string(key)] = struct{}{}
				var buf []byte
				if isStorage {
					if len(key) == length.Addr+length.Hash {
						// Non-optimised key originating from a database record
						buf = append(buf[:0], key...)
					} else {
						// Optimised key referencing a state file record (file number and offset within the file)
						buf, found = dc.lookupByShortenedKey(key, filesStorage)
						if !found {
							dc.d.logger.Crit("lost storage full key", "shortened", fmt.Sprintf("%x", key))
							panic("lost storage full key")
						}
					}

					shortened, found := dc.findKeyReplacement(buf, startTxNum, endTxNum, idxListStorage, mergedStorage)
					if !found {
						// if plain key is lost, we can save original fullkey
						if len(key) == length.Addr+length.Hash {
							return nil
						}
						// if shortened key lost, we can't continue
						dc.d.logger.Crit("valTransform: replacement for full storage key was not found",
							"step", fmt.Sprintf("%d-%d", startTxNum/dc.d.aggregationStep, endTxNum/dc.d.aggregationStep),
							"shortened", fmt.Sprintf("%x", shortened))
						panic("valTransform: replacement for full storage key was not found")
					}
					//if s, seen := shortens[string(shortened)]; seen {
					//	fmt.Printf("short key %x already seen (%s)\n", shortened, s.d)
					//	shortened1, found1 := dc.findKeyReplacement(buf, startTxNum, endTxNum, idxListStorage, mergedStorage)
					//	fmt.Printf("shor1 %x, %t", shortened1, found1)
					//}
					//shortens[string(shortened)] = pair{shortened, "sto", buf}
					return shortened
				}

				if len(key) == length.Addr {
					// Non-optimised key originating from a database record
					buf = append(buf[:0], key...)
				} else {
					buf, found = dc.lookupByShortenedKey(key, filesAccount)
					if !found {
						dc.d.logger.Crit("lost account full key", "shortened", fmt.Sprintf("%x", key))
						panic(fmt.Sprintf("lost account full key: %x", key))
					}
				}

				shortened, found := dc.findKeyReplacement(buf, startTxNum, endTxNum, idxListAccount, mergedAccount)
				if !found {
					if len(key) == length.Addr {
						return nil
					}
					dc.d.logger.Crit("valTransform: replacement for full account key was not found",
						"step", fmt.Sprintf("%d-%d", startTxNum/dc.d.aggregationStep, endTxNum/dc.d.aggregationStep),
						"shortened", fmt.Sprintf("%x", shortened))
					panic("valTransform: replacement for full account key was not found")
				}
				//if s, seen := shortens[string(shortened)]; seen {
				//	fmt.Printf("short key %x already seen (%s)\n", shortened, s.d)
				//}
				//shortens[string(shortened)] = pair{shortened, "acc", buf}
				return shortened
			})
	}
}
