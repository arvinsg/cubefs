// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package metanode

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/cubefs/cubefs/proto"
	se "github.com/cubefs/cubefs/util/sortedextent"
)

const (
	DeleteMarkFlag       = 1 << 0
	BaseInodeLen         = 88
	BaseInodeKeyLen      = 8
	BaseInodeValueLen    = 72
	BaseInodeKeyOffset   = 4
	BaseInodeValueOffset = 16
	AccessTimeOffset     = 52
)

const (
	MinEkLen     = 5
)

// Inode wraps necessary properties of `Inode` information in the file system.
// Marshal exporterKey:
//  +-------+-------+
//  | item  | Inode |
//  +-------+-------+
//  | bytes |   8   |
//  +-------+-------+
// Marshal value:
//  +-------+------+------+-----+----+----+----+--------+------------------+
//  | item  | Type | Size | Gen | CT | AT | MT | ExtLen | MarshaledExtents |
//  +-------+------+------+-----+----+----+----+--------+------------------+
//  | bytes |  4   |  8   |  8  | 8  | 8  | 8  |   4    |      ExtLen      |
//  +-------+------+------+-----+----+----+----+--------+------------------+
// Marshal entity:
//  +-------+-----------+--------------+-----------+--------------+
//  | item  | KeyLength | MarshaledKey | ValLength | MarshaledVal |
//  +-------+-----------+--------------+-----------+--------------+
//  | bytes |     4     |   KeyLength  |     4     |   ValLength  |
//  +-------+-----------+--------------+-----------+--------------+
type Inode struct {
	sync.RWMutex
	Inode      uint64 // Inode ID
	Type       uint32
	Uid        uint32
	Gid        uint32
	Size       uint64
	Generation uint64
	CreateTime int64
	AccessTime int64
	ModifyTime int64
	LinkTarget []byte // SymLink target name
	NLink      uint32 // NodeLink counts
	Flag       int32
	Reserved   uint64 // reserved space
	//Extents    *ExtentsTree
	Extents *se.SortedExtents
}

type InodeBatch []*Inode

type InodeMerge struct {
	Inode      uint64 // Inode ID
	NewExtents []proto.ExtentKey
	OldExtents []proto.ExtentKey
}

// String returns the string format of the inode.
func (i *Inode) String() string {
	i.RLock()
	defer i.RUnlock()
	buff := bytes.NewBuffer(nil)
	buff.Grow(128)
	buff.WriteString("Inode{")
	buff.WriteString(fmt.Sprintf("Inode[%d]", i.Inode))
	buff.WriteString(fmt.Sprintf("Type[%d]", i.Type))
	buff.WriteString(fmt.Sprintf("Uid[%d]", i.Uid))
	buff.WriteString(fmt.Sprintf("Gid[%d]", i.Gid))
	buff.WriteString(fmt.Sprintf("Size[%d]", i.Size))
	buff.WriteString(fmt.Sprintf("Gen[%d]", i.Generation))
	buff.WriteString(fmt.Sprintf("CT[%d]", i.CreateTime))
	buff.WriteString(fmt.Sprintf("AT[%d]", i.AccessTime))
	buff.WriteString(fmt.Sprintf("MT[%d]", i.ModifyTime))
	buff.WriteString(fmt.Sprintf("LinkT[%s]", i.LinkTarget))
	buff.WriteString(fmt.Sprintf("NLink[%d]", i.NLink))
	buff.WriteString(fmt.Sprintf("Flag[%d]", i.Flag))
	buff.WriteString(fmt.Sprintf("Reserved[%d]", i.Reserved))
	buff.WriteString(fmt.Sprintf("Extents[%s]", i.Extents))
	buff.WriteString("}")
	return buff.String()
}

// NewInode returns a new Inode instance with specified Inode ID, name and type.
// The AccessTime and ModifyTime will be set to the current time.
func NewInode(ino uint64, t uint32) *Inode {
	ts := Now.GetCurrentTime().Unix()
	i := &Inode{
		Inode:      ino,
		Type:       t,
		Generation: 1,
		CreateTime: ts,
		AccessTime: ts,
		ModifyTime: ts,
		NLink:      1,
		Extents:    se.NewSortedExtents(),
	}
	if proto.IsDir(t) {
		i.NLink = 2
	}
	return i
}

// Less tests whether the current Inode item is less than the given one.
// This method is necessary fot B-Tree item implementation.
func (i *Inode) Less(than BtreeItem) bool {
	ino, ok := than.(*Inode)
	return ok && i.Inode < ino.Inode
}

// Copy returns a copy of the inode.
func (i *Inode) Copy() BtreeItem {
	newIno := NewInode(i.Inode, i.Type)
	i.RLock()
	newIno.Uid = i.Uid
	newIno.Gid = i.Gid
	newIno.Size = i.Size
	newIno.Generation = i.Generation
	newIno.CreateTime = i.CreateTime
	newIno.ModifyTime = i.ModifyTime
	newIno.AccessTime = i.AccessTime
	if size := len(i.LinkTarget); size > 0 {
		newIno.LinkTarget = make([]byte, size)
		copy(newIno.LinkTarget, i.LinkTarget)
	}
	newIno.NLink = i.NLink
	newIno.Flag = i.Flag
	newIno.Reserved = i.Reserved
	newIno.Extents = i.Extents.Clone()
	i.RUnlock()
	return newIno
}

// MarshalToJSON is the wrapper of json.Marshal.
func (i *Inode) MarshalToJSON() ([]byte, error) {
	i.RLock()
	defer i.RUnlock()
	return json.Marshal(i)
}

// Marshal marshals the inode into a byte array.
func (i *Inode) Marshal() (result []byte, err error) {
	keyBytes := i.MarshalKey()
	valBytes := i.MarshalValue()
	keyLen := uint32(len(keyBytes))
	valLen := uint32(len(valBytes))
	buff := bytes.NewBuffer(make([]byte, 0, 128))
	if err = binary.Write(buff, binary.BigEndian, keyLen); err != nil {
		return
	}
	if _, err = buff.Write(keyBytes); err != nil {
		return
	}
	if err = binary.Write(buff, binary.BigEndian, valLen); err != nil {
		return
	}
	if _, err = buff.Write(valBytes); err != nil {
		return
	}
	result = buff.Bytes()
	return
}

// Unmarshal unmarshals the inode.
func (i *Inode) Unmarshal(ctx context.Context, raw []byte) (err error) {

	var (
		keyLen uint32
		valLen uint32
	)
	buff := bytes.NewBuffer(raw)
	if err = binary.Read(buff, binary.BigEndian, &keyLen); err != nil {
		return
	}
	keyBytes := make([]byte, keyLen)
	if _, err = buff.Read(keyBytes); err != nil {
		return
	}
	if err = i.UnmarshalKey(keyBytes); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &valLen); err != nil {
		return
	}
	valBytes := make([]byte, valLen)
	if _, err = buff.Read(valBytes); err != nil {
		return
	}
	err = i.UnmarshalValue(ctx, valBytes)
	return
}

// Marshal marshals the inodeBatch into a byte array.
func (i InodeBatch) Marshal(ctx context.Context) ([]byte, error) {

	buff := bytes.NewBuffer(make([]byte, 0))
	if err := binary.Write(buff, binary.BigEndian, uint32(len(i))); err != nil {
		return nil, err
	}
	for _, inode := range i {
		bs, err := inode.Marshal()
		if err != nil {
			return nil, err
		}
		if err = binary.Write(buff, binary.BigEndian, uint32(len(bs))); err != nil {
			return nil, err
		}
		if _, err := buff.Write(bs); err != nil {
			return nil, err
		}
	}
	return buff.Bytes(), nil
}

// inodeid len(NewExtents) []byte(NewExtents) len(OldExtents) []byte(OldExtents)
func (im *InodeMerge) Marshal() ([]byte, error) {
	buff := bytes.NewBuffer(make([]byte, 0))
	if err := binary.Write(buff, binary.BigEndian, im.Inode); err != nil {
		return nil, err
	}
	if err := binary.Write(buff, binary.BigEndian, uint32(len(im.NewExtents))); err != nil {
		return nil, err
	}
	for _, extent := range im.NewExtents {
		ex, err := extent.MarshalBinary()
		if err != nil {
			return nil, err
		}
		if _, err := buff.Write(ex); err != nil {
			return nil, err
		}
	}
	if err := binary.Write(buff, binary.BigEndian, uint32(len(im.OldExtents))); err != nil {
		return nil, err
	}
	for _, extent := range im.OldExtents {
		ex, err := extent.MarshalBinary()
		if err != nil {
			return nil, err
		}
		if _, err := buff.Write(ex); err != nil {
			return nil, err
		}
	}
	return buff.Bytes(), nil
}

func InodeMergeUnmarshal(raw []byte) (*InodeMerge, error) {
	buff := bytes.NewBuffer(raw)
	im := &InodeMerge{}
	var inodeId uint64
	if err := binary.Read(buff, binary.BigEndian, &inodeId); err != nil {
		return nil, err
	}
	im.Inode = inodeId
	var newEksSize uint32
	if err := binary.Read(buff, binary.BigEndian, &newEksSize); err != nil {
		return nil, err
	}
	im.NewExtents = make([]proto.ExtentKey, newEksSize)
	for i := 0; i < int(newEksSize); i++ {
		ek := proto.ExtentKey{}
		if err := ek.UnmarshalBinary(buff); err != nil {
			return nil, err
		}
		im.NewExtents[i] = ek
	}
	var oldEksSize uint32
	if err := binary.Read(buff, binary.BigEndian, &oldEksSize); err != nil {
		return nil, err
	}
	im.OldExtents = make([]proto.ExtentKey, oldEksSize)
	for i := 0; i < int(oldEksSize); i++ {
		ek := proto.ExtentKey{}
		if err := ek.UnmarshalBinary(buff); err != nil {
			return nil, err
		}
		im.OldExtents[i] = ek
	}
	return im, nil
}

func (i *Inode) innerEncodeBinary(data []byte) {
	offset := 0
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], uint32(BaseInodeKeyLen))
	offset += Uint32Size
	binary.BigEndian.PutUint64(data[offset:offset+Uint64Size], i.Inode)
	offset += Uint64Size
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], uint32(i.Extents.Len()*proto.ExtentLength+BaseInodeValueLen+len(i.LinkTarget)))
	offset += Uint32Size
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], i.Type)
	offset += Uint32Size
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], i.Uid)
	offset += Uint32Size
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], i.Gid)
	offset += Uint32Size
	binary.BigEndian.PutUint64(data[offset:offset+Uint64Size], i.Size)
	offset += Uint64Size
	binary.BigEndian.PutUint64(data[offset:offset+Uint64Size], i.Generation)
	offset += Uint64Size
	binary.BigEndian.PutUint64(data[offset:offset+Uint64Size], uint64(i.CreateTime))
	offset += Uint64Size
	binary.BigEndian.PutUint64(data[offset:offset+Uint64Size], uint64(i.AccessTime))
	offset += Uint64Size
	binary.BigEndian.PutUint64(data[offset:offset+Uint64Size], uint64(i.ModifyTime))
	offset += Uint64Size
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], uint32(len(i.LinkTarget)))
	offset += Uint32Size
	copy(data[offset:offset+len(i.LinkTarget)], i.LinkTarget)
	offset += len(i.LinkTarget)
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], i.NLink)
	offset += Uint32Size
	binary.BigEndian.PutUint32(data[offset:offset+Uint32Size], uint32(i.Flag))
	offset += Uint32Size
	binary.BigEndian.PutUint64(data[offset:offset+Uint64Size], i.Reserved)
	offset += Uint64Size
	i.Extents.Range(func(ek proto.ExtentKey) bool {
		ek.EncodeBinary(data[offset : offset+proto.ExtentLength])
		offset += proto.ExtentLength
		return offset < len(data)
	})
}

func (i *Inode) MarshalV2() (result []byte, err error) {
	i.Lock()
	defer i.Unlock()

	if i.Extents == nil {
		i.Extents = se.NewSortedExtents()
	}
	result = make([]byte, BaseInodeLen+len(i.LinkTarget)+i.Extents.Len()*proto.ExtentLength)
	i.innerEncodeBinary(result)
	return result, nil
}

func (i *Inode) UnmarshalV2(ctx context.Context, raw []byte) (err error) {

	if len(raw) < BaseInodeLen {
		return fmt.Errorf("inode buff err, need at least %d, but buff len:%d", BaseInodeValueLen, len(raw))
	}
	offset := 0
	//keyLen = binary.BigEndian.Uint32(raw[:4])
	offset += 4
	i.Inode = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	//valLen = binary.BigEndian.Uint32(raw[offset:offset+4])
	offset += 4
	i.Type = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Uid = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Gid = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Size = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	i.Generation = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	i.CreateTime = int64(binary.BigEndian.Uint64(raw[offset : offset+8]))
	offset += 8
	i.AccessTime = int64(binary.BigEndian.Uint64(raw[offset : offset+8]))
	offset += 8
	i.ModifyTime = int64(binary.BigEndian.Uint64(raw[offset : offset+8]))
	offset += 8
	symSize := binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	if symSize > 0 {
		i.LinkTarget = i.LinkTarget[:0]
		i.LinkTarget = append(i.LinkTarget, raw[offset:offset+int(symSize)]...)
	}
	offset += len(i.LinkTarget)
	i.NLink = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Flag = int32(binary.BigEndian.Uint32(raw[offset : offset+4]))
	offset += 4
	i.Reserved = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	if len(raw[offset:]) == 0 {
		return
	}
	// unmarshal ExtentsKey
	if i.Extents == nil {
		i.Extents = se.NewSortedExtents()
	}
	if err = i.Extents.UnmarshalBinaryV2(ctx, raw[offset:], i.Inode); err != nil {
		return
	}
	return
}

func (i *Inode) UnmarshalV2WithKeyAndValue(ctx context.Context, key, value []byte) (err error) {
	i.UnmarshalKeyV2(key)
	err = i.UnmarshalValueV2(ctx, value)
	return
}

func (i *Inode) UnmarshalKeyV2(key []byte) {
	if len(key) < BaseInodeKeyLen {
		return
	}
	i.Inode = binary.BigEndian.Uint64(key)
	return
}

func (i *Inode) UnmarshalValueV2(ctx context.Context, raw []byte) (err error) {
	if len(raw) < BaseInodeValueLen {
		return fmt.Errorf("inode buff err, need at least %d, but buff len:%d", BaseInodeValueLen, len(raw))
	}
	offset := 0
	i.Type = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Uid = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Gid = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Size = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	i.Generation = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	i.CreateTime = int64(binary.BigEndian.Uint64(raw[offset : offset+8]))
	offset += 8
	i.AccessTime = int64(binary.BigEndian.Uint64(raw[offset : offset+8]))
	offset += 8
	i.ModifyTime = int64(binary.BigEndian.Uint64(raw[offset : offset+8]))
	offset += 8
	symSize := binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	if symSize > 0 {
		i.LinkTarget = i.LinkTarget[:0]
		i.LinkTarget = append(i.LinkTarget, raw[offset:offset+int(symSize)]...)
	}
	offset += len(i.LinkTarget)
	i.NLink = binary.BigEndian.Uint32(raw[offset : offset+4])
	offset += 4
	i.Flag = int32(binary.BigEndian.Uint32(raw[offset : offset+4]))
	offset += 4
	i.Reserved = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	if len(raw[offset:]) == 0 {
		return
	}
	// unmarshal ExtentsKey
	if i.Extents == nil {
		i.Extents = se.NewSortedExtents()
	}
	if err = i.Extents.UnmarshalBinaryV2(ctx, raw[offset:], i.Inode); err != nil {
		return
	}
	return nil
}

func (i *Inode) EncodeBinary(data []byte) (dataLen int, err error) {
	i.RLock()
	defer i.RUnlock()

	if i.Extents == nil {
		i.Extents = se.NewSortedExtents()
	}
	dataLen = BaseInodeLen+len(i.LinkTarget)+i.Extents.Len()*proto.ExtentLength
	if len(data) < dataLen {
		err = fmt.Errorf("date len %v less than inode len %v", len(data), dataLen)
		return
	}
	i.innerEncodeBinary(data)
	return
}

func (i *Inode) BinaryDataLen() int {
	i.RLock()
	defer i.RUnlock()
	return BaseInodeLen+len(i.LinkTarget)+i.Extents.Len()*proto.ExtentLength
}

// Unmarshal unmarshals the inodeBatch.
func InodeBatchUnmarshal(ctx context.Context, raw []byte) (InodeBatch, error) {

	buff := bytes.NewBuffer(raw)
	var batchLen uint32
	if err := binary.Read(buff, binary.BigEndian, &batchLen); err != nil {
		return nil, err
	}

	result := make(InodeBatch, 0, int(batchLen))

	var dataLen uint32
	for j := 0; j < int(batchLen); j++ {
		if err := binary.Read(buff, binary.BigEndian, &dataLen); err != nil {
			return nil, err
		}
		data := make([]byte, int(dataLen))
		if _, err := buff.Read(data); err != nil {
			return nil, err
		}
		ino := NewInode(0, 0)
		if err := ino.Unmarshal(ctx, data); err != nil {
			return nil, err
		}
		result = append(result, ino)
	}

	return result, nil
}

// MarshalKey marshals the exporterKey to bytes.
func (i *Inode) MarshalKey() (k []byte) {
	k = make([]byte, 8)
	binary.BigEndian.PutUint64(k, i.Inode)
	return
}

// UnmarshalKey unmarshals the exporterKey from bytes.
func (i *Inode) UnmarshalKey(k []byte) (err error) {
	i.Inode = binary.BigEndian.Uint64(k)
	return
}

// MarshalValue marshals the value to bytes.
func (i *Inode) MarshalValue() (val []byte) {
	var err error
	buff := bytes.NewBuffer(make([]byte, 0, 128))
	buff.Grow(64)
	i.RLock()
	if err = binary.Write(buff, binary.BigEndian, &i.Type); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.Uid); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.Gid); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.Size); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.Generation); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.CreateTime); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.AccessTime); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.ModifyTime); err != nil {
		panic(err)
	}
	// write SymLink
	symSize := uint32(len(i.LinkTarget))
	if err = binary.Write(buff, binary.BigEndian, &symSize); err != nil {
		panic(err)
	}
	if _, err = buff.Write(i.LinkTarget); err != nil {
		panic(err)
	}

	if err = binary.Write(buff, binary.BigEndian, &i.NLink); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.Flag); err != nil {
		panic(err)
	}
	if err = binary.Write(buff, binary.BigEndian, &i.Reserved); err != nil {
		panic(err)
	}
	// marshal ExtentsKey
	extData, err := i.Extents.MarshalBinary()
	if err != nil {
		panic(err)
	}
	if _, err = buff.Write(extData); err != nil {
		panic(err)
	}

	val = buff.Bytes()
	i.RUnlock()
	return
}

// UnmarshalValue unmarshals the value from bytes.
func (i *Inode) UnmarshalValue(ctx context.Context, val []byte) (err error) {
	buff := bytes.NewBuffer(val)
	if err = binary.Read(buff, binary.BigEndian, &i.Type); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.Uid); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.Gid); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.Size); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.Generation); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.CreateTime); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.AccessTime); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.ModifyTime); err != nil {
		return
	}
	// read symLink
	symSize := uint32(0)
	if err = binary.Read(buff, binary.BigEndian, &symSize); err != nil {
		return
	}
	if symSize > 0 {
		i.LinkTarget = make([]byte, symSize)
		if _, err = io.ReadFull(buff, i.LinkTarget); err != nil {
			return
		}
	}

	if err = binary.Read(buff, binary.BigEndian, &i.NLink); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.Flag); err != nil {
		return
	}
	if err = binary.Read(buff, binary.BigEndian, &i.Reserved); err != nil {
		return
	}
	if buff.Len() == 0 {
		return
	}
	// unmarshal ExtentsKey
	if i.Extents == nil {
		i.Extents = se.NewSortedExtents()
	}
	if err = i.Extents.UnmarshalBinary(ctx, buff.Bytes(), i.Inode); err != nil {
		return
	}
	return
}

// AppendExtents append the extent to the btree.
func (i *Inode) AppendExtents(ctx context.Context, eks []proto.ExtentKey, ct int64) (delExtents []proto.MetaDelExtentKey) {

	i.Lock()
	oldFileSize := i.Extents.Size()
	for _, ek := range eks {
		delItems := i.Extents.Append(ctx, ek, i.Inode)
		size := i.Extents.Size()
		if i.Size < size {
			i.Size = size
		}
		delExtents = append(delExtents, delItems...)
	}
	i.ModifyTime = ct
	currentFileSize := i.Extents.Size()
	if !(oldFileSize == currentFileSize && len(delExtents) == 0) {
		i.Generation++
	}
	i.Unlock()
	return
}

func (i *Inode) InsertExtents(ctx context.Context, eks []proto.ExtentKey, ct int64) (delExtents []proto.MetaDelExtentKey) {
	if len(eks) == 0 {
		return
	}

	i.Lock()
	defer i.Unlock()

	for _, ek := range eks {
		delExtents = append(delExtents, i.Extents.Insert(ctx, ek, i.Inode)...)
	}
	i.Size = uint64(math.Max(float64(i.Size), float64(i.Extents.Size())))
	i.ModifyTime = ct
	i.Generation++

	return
}

func (i *Inode) ExtentsTruncate(length uint64, ct int64) (delExtents []proto.MetaDelExtentKey) {
	i.Lock()
	delExtents = i.Extents.Truncate(length, i.Inode)
	i.Size = length
	i.ModifyTime = ct
	i.Generation++
	i.Unlock()
	return
}

func (i *Inode) FileMigMergeExtents(newEks []proto.ExtentKey, oldEks []proto.ExtentKey) (delExtents []proto.MetaDelExtentKey, merged bool, msg string) {
	i.Lock()
	defer i.Unlock()
	if delExtents, merged, msg = i.Extents.FileMigMerge(newEks, oldEks, i.Inode); merged {
		i.Generation++
	}
	return
}

func (i *Inode) DelNewExtents(newEks []proto.ExtentKey) (delExtents []proto.MetaDelExtentKey) {
	i.Lock()
	defer i.Unlock()
	delExtents = i.Extents.DelNewExtent(newEks, i.Inode)
	return
}

// IncNLink increases the nLink value by one.
func (i *Inode) IncNLink() {
	i.Lock()
	i.NLink++
	i.Unlock()
}

// DecNLink decreases the nLink value by one.
func (i *Inode) DecNLink() {
	i.Lock()
	if proto.IsDir(i.Type) && i.NLink == 2 {
		i.NLink--
	}
	if i.NLink > 0 {
		i.NLink--
	}
	i.Unlock()
}

func (i *Inode) DecNlinkNum(unlinkCount uint32) {
	i.Lock()
	if i.NLink > unlinkCount {
		i.NLink = i.NLink - unlinkCount
	} else {
		i.NLink = 0
	}

	i.Unlock()
}

// GetNLink returns the nLink value.
func (i *Inode) GetNLink() uint32 {
	i.RLock()
	defer i.RUnlock()
	return i.NLink
}

func (i *Inode) IsDeleting() bool {
	i.RLock()
	ok := i.NLink == 0
	i.RUnlock()
	return ok
}

func (i *Inode) IsEmptyDir() bool {
	i.RLock()
	ok := (proto.IsDir(i.Type) && i.NLink <= 2)
	i.RUnlock()
	return ok
}

func (i *Inode) IsNeedCompact(minEkLen int, minInodeSize uint64, maxEkAvgSize uint64) bool {
	i.RLock()
	defer i.RUnlock()
	//if minEkLen < MinEkLen {
	//	minEkLen = MinEkLen
	//}
	if i.Extents.Len() <= minEkLen || i.Size <= minInodeSize {
		return false
	}
	ekAvgSize := i.Size / uint64(i.Extents.Len())
	if ekAvgSize < maxEkAvgSize {
		return true
	}

	return false
}

// SetDeleteMark set the deleteMark flag. TODO markDelete or deleteMark? markDelete has been used in datanode.
func (i *Inode) SetDeleteMark() {
	i.Lock()
	i.Flag |= DeleteMarkFlag
	i.Unlock()
}

func (i *Inode) CancelDeleteMark() {
	i.Lock()
	i.Flag &= ^DeleteMarkFlag
	i.Unlock()
}

// ShouldDelete returns if the inode has been marked as deleted.
func (i *Inode) ShouldDelete() bool {
	i.RLock()
	defer i.RUnlock()
	return i.Flag&DeleteMarkFlag == DeleteMarkFlag
}

// SetAttr sets the attributes of the inode.
func (i *Inode) SetAttr(req *SetattrRequest) {
	i.Lock()
	if req.Valid&proto.AttrMode != 0 {
		i.Type = req.Mode
	}
	if req.Valid&proto.AttrUid != 0 {
		i.Uid = req.Uid
	}
	if req.Valid&proto.AttrGid != 0 {
		i.Gid = req.Gid
	}
	if req.Valid&proto.AttrAccessTime != 0 {
		i.AccessTime = req.AccessTime
	}
	if req.Valid&proto.AttrModifyTime != 0 {
		i.ModifyTime = req.ModifyTime
	}
	i.Unlock()
}

func (i *Inode) DoWriteFunc(fn func()) {
	i.Lock()
	fn()
	i.Unlock()
}

// DoFunc executes the given function.
func (i *Inode) DoReadFunc(fn func()) {
	i.RLock()
	fn()
	i.RUnlock()
}

func (i *Inode) Reset() {
	i.Lock()
	defer i.Unlock()
	i.Inode = 0
	i.Size = 0
	i.Generation = 0
	i.Type = 0
	i.NLink = 0
	i.CreateTime = 0
	i.ModifyTime = 0
	i.AccessTime = 0
	i.Uid = 0
	i.Gid = 0
	i.Flag = 0
	i.Reserved = 0
	i.LinkTarget = nil
	i.Extents = se.NewSortedExtents()
}

