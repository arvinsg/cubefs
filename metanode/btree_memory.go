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
	"encoding/binary"
	"github.com/cubefs/cubefs/util/btree"
	"github.com/cubefs/cubefs/util/unit"
	"hash/crc32"
	"sync"
	"sync/atomic"
)

const defaultBTreeDegree = 32

type (
	// BtreeItem type alias google btree Item
	BtreeItem = btree.Item
)

var _ Snapshot = &MemSnapShot{}

type MemSnapShot struct {
	applyID   uint64
	inode     *InodeBTree
	dentry    *DentryBTree
	extend    *ExtendBTree
	multipart *MultipartBTree
	delDentry *DeletedDentryBTree
	delInode  *DeletedInodeBTree
}

func (b *MemSnapShot) Range(tp TreeType, cb func(item interface{}) (bool, error)) error {
	switch tp {
	case InodeType:
		callBackFunc := func(inode *Inode) (bool, error) {
			return cb(inode)
		}
		return b.inode.Range(&Inode{}, nil, callBackFunc)
	case DentryType:
		callBackFunc := func(dentry *Dentry) (bool, error) {
			return cb(dentry)
		}
		return b.dentry.Range(&Dentry{}, nil, callBackFunc)
	case ExtendType:
		callBackFunc := func(extend *Extend) (bool, error) {
			return cb(extend)
		}
		return b.extend.Range(&Extend{}, nil, callBackFunc)
	case MultipartType:
		callBackFunc := func(multipart *Multipart) (bool, error) {
			return cb(multipart)
		}
		return b.multipart.Range(&Multipart{}, nil, callBackFunc)
	case DelDentryType:
		callBackFunc := func(delDentry *DeletedDentry) (bool, error) {
			return cb(delDentry)
		}
		return b.delDentry.Range(&DeletedDentry{}, nil, callBackFunc)
	case DelInodeType:
		callBackFunc := func(delInode *DeletedINode) (bool, error) {
			return cb(delInode)
		}
		return b.delInode.Range(&DeletedINode{}, nil, callBackFunc)
	default:
	}
	panic("out of type")
}

func (b *MemSnapShot) RangeDentryTreeWithPrefix(prefix, start, end *Dentry, cb func(item *Dentry) (bool, error)) error {
	return b.dentry.RangeWithPrefix(prefix, start, end, cb)
}

func (b *MemSnapShot) RangeDelDentryTreeWithPrefix(prefix, start, end *DeletedDentry, cb func(item *DeletedDentry) (bool, error)) error {
	return b.delDentry.RangeWithPrefix(prefix, start, end, cb)
}

func (b *MemSnapShot) Close() {}

func (b *MemSnapShot) Count(tp TreeType) uint64 {
	switch tp {
	case InodeType:
		return uint64(b.inode.Len())
	case DentryType:
		return uint64(b.dentry.Len())
	case ExtendType:
		return uint64(b.extend.Len())
	case MultipartType:
		return uint64(b.multipart.Len())
	case DelDentryType:
		return uint64(b.delDentry.Len())
	case DelInodeType:
		return uint64(b.delInode.Len())
	default:
	}
	panic("out of type")
}

func (b *MemSnapShot) CrcSum(tp TreeType) (crcSum uint32, err error) {
	var (
		crc           = crc32.NewIEEE()
		data          = make([]byte, 4*unit.MB)
		maxRetryCount = 2
		dataLen       int
	)
	switch tp {
	case InodeType:
		cb := func(i *Inode) (bool, error) {
			for retryCnt := 0; retryCnt < maxRetryCount; retryCnt++ {
				if dataLen, err = i.EncodeBinary(data); err == nil {
					break
				}
				data = make([]byte, dataLen*2)
			}
			if err != nil {
				return false, err
			}
			binary.BigEndian.PutUint64(data[AccessTimeOffset:AccessTimeOffset+8], 0)
			if _, err = crc.Write(data[:dataLen]); err != nil {
				return false, err
			}
			return true, nil
		}
		err = b.inode.Range(&Inode{}, nil, cb)
	case DentryType:
		cb := func(d *Dentry) (bool, error) {
			for retryCnt := 0; retryCnt < maxRetryCount; retryCnt++ {
				if dataLen, err = d.EncodeBinary(data); err == nil {
					break
				}
				data = make([]byte, dataLen*2)
			}
			if err != nil {
				return false, err
			}
			if _, err = crc.Write(data[:dataLen]); err != nil {
				return false, err
			}
			return true, nil
		}
		err = b.dentry.Range(&Dentry{}, nil, cb)
	case ExtendType:
		cb := func(extend *Extend) (bool, error) {
			if data, err = extend.Bytes(); err != nil {
				return false, err
			}
			if _, err = crc.Write(data); err != nil {
				return false, err
			}
			return true, nil
		}
		err = b.extend.Range(&Extend{}, nil, cb)
	case MultipartType:
		cb := func(multipart *Multipart) (bool, error) {
			if data, err = multipart.Bytes(); err != nil {
				return false, err
			}
			if _, err = crc.Write(data); err != nil {
				return false, err
			}
			return true, nil
		}
		err = b.multipart.Range(&Multipart{}, nil, cb)
	case DelDentryType:
		cb := func(delDentry *DeletedDentry) (bool, error) {
			for retryCnt := 0; retryCnt < maxRetryCount; retryCnt++ {
				if dataLen, err = delDentry.EncodeBinary(data); err == nil {
					break
				}
				data = make([]byte, dataLen*2)
			}
			if err != nil {
				return false, err
			}
			if _, err = crc.Write(data[:dataLen]); err != nil {
				return false, err
			}
			return true, nil
		}
		err = b.delDentry.Range(&DeletedDentry{}, nil, cb)
	case DelInodeType:
		cb := func(delInode *DeletedINode) (bool, error) {
			for retryCnt := 0; retryCnt < maxRetryCount; retryCnt++ {
				if dataLen, err = delInode.EncodeBinary(data); err == nil {
					break
				}
				data = make([]byte, dataLen*2)
			}
			if err != nil {
				return false, err
			}
			binary.BigEndian.PutUint64(data[AccessTimeOffset:AccessTimeOffset+8], 0)
			if _, err = crc.Write(data[:dataLen]); err != nil {
				return false, err
			}
			return true, nil
		}
		err = b.delInode.Range(&DeletedINode{}, nil, cb)
	default:
		panic("out of type")
	}
	if err != nil {
		return
	}
	crcSum = crc.Sum32()
	return
}

func (b *MemSnapShot) ApplyID() uint64 {
	return b.applyID
}

func (b *MemSnapShot) GetInode(inoID uint64) (ino *Inode, err error) {
	return b.inode.Get(inoID)
}

func (b *MemSnapShot) FirstDentry() (den *Dentry, err error) {
	err = b.dentry.Range(nil, nil, func(d *Dentry) (bool, error) {
		den = d
		return false, nil
	})
	return
}

var _ InodeTree = &InodeBTree{}
var _ DentryTree = &DentryBTree{}
var _ ExtendTree = &ExtendBTree{}
var _ MultipartTree = &MultipartBTree{}
var _ DeletedDentryTree = &DeletedDentryBTree{}
var _ DeletedInodeTree = &DeletedInodeBTree{}

type InodeBTree struct {
	*BTree
	inodesTotalSize uint64
}

type DentryBTree struct {
	*BTree
}

type ExtendBTree struct {
	*BTree
}

type MultipartBTree struct {
	*BTree
}

type DeletedDentryBTree struct {
	*BTree
}

type DeletedInodeBTree struct {
	*BTree
	delInodesTotalSize uint64
}

func (i *InodeBTree) GetMaxInode() (uint64, error) {
	i.Lock()
	item := i.tree.Max()
	i.Unlock()
	if item == nil {
		return 0, nil
	}
	return item.(*Inode).Inode, nil
}

//get
func (i *InodeBTree) RefGet(ino uint64) (*Inode, error) {
	item := i.BTree.Get(&Inode{Inode: ino})
	if item != nil {
		return item.(*Inode), nil
	}
	return nil, nil
}

func (i *InodeBTree) Get(ino uint64) (*Inode, error) {
	item := i.BTree.CopyGet(&Inode{Inode: ino})
	if item != nil {
		return item.(*Inode), nil
	}
	return nil, nil
}

func (i *DentryBTree) RefGet(pid uint64, name string) (*Dentry, error) {
	item := i.BTree.Get(&Dentry{ParentId: pid, Name: name})
	if item != nil {
		return item.(*Dentry), nil
	}
	return nil, nil
}

func (i *DentryBTree) Get(pid uint64, name string) (*Dentry, error) {
	item := i.BTree.CopyGet(&Dentry{ParentId: pid, Name: name})
	if item != nil {
		return item.(*Dentry), nil
	}
	return nil, nil
}

func (i *ExtendBTree) RefGet(ino uint64) (*Extend, error) {
	item := i.BTree.Get(&Extend{inode: ino})
	if item != nil {
		return item.(*Extend), nil
	}
	return nil, nil
}

func (i *ExtendBTree) Get(ino uint64) (*Extend, error) {
	item := i.BTree.CopyGet(&Extend{inode: ino})
	if item != nil {
		return item.(*Extend), nil
	}
	return nil, nil
}

func (i *MultipartBTree) RefGet(key, id string) (*Multipart, error) {
	item := i.BTree.Get(&Multipart{key: key, id: id})
	if item != nil {
		return item.(*Multipart), nil
	}
	return nil, nil
}

func (i *MultipartBTree) Get(key, id string) (*Multipart, error) {
	item := i.BTree.CopyGet(&Multipart{key: key, id: id})
	if item != nil {
		return item.(*Multipart), nil
	}
	return nil, nil
}

func (i *DeletedInodeBTree) RefGet(ino uint64) (*DeletedINode, error) {
	item := i.BTree.Get(NewDeletedInodeByID(ino))
	if item != nil {
		return item.(*DeletedINode), nil
	}
	return nil, nil
}

func (i *DeletedInodeBTree) Get(ino uint64) (*DeletedINode, error) {
	item := i.BTree.CopyGet(NewDeletedInodeByID(ino))
	if item != nil {
		return item.(*DeletedINode), nil
	}
	return nil, nil
}

func (i *DeletedDentryBTree) RefGet(pino uint64, name string, timeStamp int64) (*DeletedDentry, error) {
	item := i.BTree.Get(newPrimaryDeletedDentry(pino, name, timeStamp, 0))
	if item != nil {
		return item.(*DeletedDentry), nil
	}
	return nil, nil
}

func (i *DeletedDentryBTree) Get(pino uint64, name string, timeStamp int64) (*DeletedDentry, error) {
	item := i.BTree.CopyGet(newPrimaryDeletedDentry(pino, name, timeStamp, 0))
	if item != nil {
		return item.(*DeletedDentry), nil
	}
	return nil, nil
}

func (i *InodeBTree) Update(dbHandle interface{}, inode *Inode) error {
	return nil
}

//put
func (i *InodeBTree) Put(dbHandle interface{}, inode *Inode) error {
	i.BTree.ReplaceOrInsert(inode, true)
	return nil
}
func (i *DentryBTree) Update(dbHandle interface{}, dentry *Dentry) error {
	return nil
}
func (i *DentryBTree) Put(dbHandle interface{}, dentry *Dentry) error {
	i.BTree.ReplaceOrInsert(dentry, true)
	return nil
}
func (i *ExtendBTree) Update(dbHandle interface{}, extend *Extend) error {
	return nil
}
func (i *ExtendBTree) Put(dbHandle interface{}, extend *Extend) error {
	i.BTree.ReplaceOrInsert(extend, true)
	return nil
}
func (i *MultipartBTree) Update(dbHandle interface{}, multipart *Multipart) error {
	return nil
}
func (i *MultipartBTree) Put(dbHandle interface{}, multipart *Multipart) error {
	i.BTree.ReplaceOrInsert(multipart, true)
	return nil
}
func (i *DeletedInodeBTree) Update(dbHandle interface{}, delIno *DeletedINode) error {
	return nil
}

//create
func (i *InodeBTree) Create(dbHandle interface{}, inode *Inode, replace bool) (*Inode, bool, error) {
	item, ok := i.BTree.ReplaceOrInsert(inode, replace)
	if !ok {
		return item.(*Inode), ok, nil
	}
	return inode, ok, nil
}
func (i *DentryBTree) Create(dbHandle interface{}, dentry *Dentry, replace bool) (*Dentry, bool, error) {
	item, ok := i.BTree.ReplaceOrInsert(dentry, replace)
	if !ok {
		return item.(*Dentry), ok, nil
	}
	return dentry, ok, nil
}
func (i *ExtendBTree) Create(dbHandle interface{}, extend *Extend, replace bool) (*Extend, bool, error) {
	item, ok := i.BTree.ReplaceOrInsert(extend, replace)
	if !ok {
		return item.(*Extend), ok, nil
	}
	return extend, ok, nil
}
func (i *MultipartBTree) Create(dbHandle interface{}, mul *Multipart, replace bool) (*Multipart, bool, error) {
	item, ok := i.BTree.ReplaceOrInsert(mul, replace)
	if !ok {
		return item.(*Multipart), ok, nil
	}
	return mul, ok, nil
}
func (i *DeletedDentryBTree) Create(dbHandle interface{}, delDentry *DeletedDentry, replace bool) (*DeletedDentry, bool, error) {
	item, ok := i.BTree.ReplaceOrInsert(delDentry, replace)
	if !ok {
		return item.(*DeletedDentry), ok, nil
	}
	return delDentry, ok, nil
}
func (i *DeletedInodeBTree) Create(dbHandle interface{}, delInode *DeletedINode, replace bool) (*DeletedINode, bool, error) {
	item, ok := i.BTree.ReplaceOrInsert(delInode, replace)
	if !ok {
		return item.(*DeletedINode), ok, nil
	}
	return delInode, ok, nil
}

func (i *InodeBTree) Delete(dbHandle interface{}, ino uint64) (bool, error) {
	if v := i.BTree.Delete(&Inode{Inode: ino}); v == nil {
		return false, nil
	}
	return true, nil
}
func (i *DentryBTree) Delete(dbHandle interface{}, pid uint64, name string) (bool, error) {
	if v := i.BTree.Delete(&Dentry{ParentId: pid, Name: name}); v == nil {
		return false, nil
	}
	return true, nil
}
func (i *ExtendBTree) Delete(dbHandle interface{}, ino uint64) (bool, error) {
	if v := i.BTree.Delete(&Extend{inode: ino}); v == nil {
		return false, nil
	}
	return true, nil
}
func (i *MultipartBTree) Delete(dbHandle interface{}, key, id string) (bool, error) {
	if mul := i.BTree.Delete(&Multipart{key: key, id: id}); mul == nil {
		return false, nil
	}
	return true, nil
}
func (i *DeletedDentryBTree) Delete(dbHandle interface{}, pid uint64, name string, timeStamp int64) (bool, error) {
	if dd := i.BTree.Delete(&DeletedDentry{Dentry: Dentry{ParentId: pid, Name: name}, Timestamp: timeStamp}); dd == nil {
		return false, nil
	}
	return true, nil
}
func (i *DeletedInodeBTree) Delete(dbHandle interface{}, ino uint64) (bool, error) {
	if di := i.BTree.Delete(&DeletedINode{Inode: Inode{Inode: ino}}); di == nil {
		return false, nil
	}
	return true, nil
}

//range
func (i *InodeBTree) Range(start, end *Inode, cb func(i *Inode) (bool, error)) error {
	var (
		err  error
		next bool
	)
	if start == nil {
		start = NewInode(0, 0)
	}

	callback := func(i BtreeItem) bool {
		next, err = cb(i.(*Inode))
		if err != nil {
			return false
		}
		return next
	}

	if end == nil {
		i.BTree.AscendGreaterOrEqual(start, callback)
	} else {
		i.BTree.AscendRange(start, end, callback)
	}
	return err
}

func (i *DentryBTree) Range(start, end *Dentry, cb func(d *Dentry) (bool, error)) error {
	var (
		err  error
		next bool
	)
	if start == nil {
		start = &Dentry{0, "", 0, 0}
	}

	callback := func(i BtreeItem) bool {
		next, err = cb(i.(*Dentry))
		if err != nil {
			return false
		}
		return next
	}

	if end == nil {
		i.BTree.AscendGreaterOrEqual(start, callback)
	} else {
		i.BTree.AscendRange(start, end, callback)
	}
	return err
}

func (i *DentryBTree) RangeWithPrefix(prefix, start, end *Dentry, cb func(d *Dentry) (bool, error)) error {
	return i.Range(start, end, cb)
}

func (i *ExtendBTree) Range(start, end *Extend, cb func(e *Extend) (bool, error)) error {
	var (
		err  error
		next bool
	)
	if start == nil {
		start = &Extend{inode: 0}
	}

	callback := func(i BtreeItem) bool {
		next, err = cb(i.(*Extend))
		if err != nil {
			return false
		}
		return next
	}

	if end == nil {
		i.BTree.AscendGreaterOrEqual(start, callback)
	} else {
		i.BTree.AscendRange(start, end, callback)
	}

	return err
}

func (i *MultipartBTree) Range(start, end *Multipart, cb func(m *Multipart) (bool, error)) error {
	var (
		err  error
		next bool
	)
	callback := func(i BtreeItem) bool {
		next, err = cb(i.(*Multipart))
		if err != nil {
			return false
		}
		return next
	}

	if start == nil {
		start = &Multipart{key: "", id: ""}
	}

	if end == nil {
		i.BTree.AscendGreaterOrEqual(start, callback)
	} else {
		i.BTree.AscendRange(start, end, callback)
	}
	return err
}

func (i *MultipartBTree) RangeWithPrefix(prefix, start, end *Multipart, cb func(m *Multipart) (bool, error)) error {
	return i.Range(start, end, cb)
}

func (i *DeletedDentryBTree) Range(start, end *DeletedDentry, cb func(deletedDentry *DeletedDentry) (bool, error)) error {
	var (
		err  error
		next bool
	)
	if start == nil {
		start = newPrimaryDeletedDentry(0, "", 0, 0)
	}
	callback := func(i BtreeItem) bool {
		next, err = cb(i.(*DeletedDentry))
		if err != nil {
			return false
		}
		return next
	}
	if end == nil {
		i.BTree.AscendGreaterOrEqual(start, callback)
	} else {
		i.BTree.AscendRange(start, end, callback)
	}
	return err
}

func (i *DeletedDentryBTree) RangeWithPrefix(prefix, start, end *DeletedDentry, cb func(deletedDentry *DeletedDentry) (bool, error)) error {
	return i.Range(start, end, cb)
}

func (i *DeletedInodeBTree) Range(start, end *DeletedINode, cb func(deletedInode *DeletedINode) (bool, error)) error {
	var (
		err  error
		next bool
	)
	if start == nil {
		start = NewDeletedInodeByID(0)
	}
	callback := func(i BtreeItem) bool {
		next, err = cb(i.(*DeletedINode))
		if err != nil {
			return false
		}
		return next
	}
	if end == nil {
		i.BTree.AscendGreaterOrEqual(start, callback)
	} else {
		i.BTree.AscendRange(start, end, callback)
	}
	return err
}

// MaxItem returns the largest item in the btree.
func (i *InodeBTree) MaxItem() *Inode {
	i.RLock()
	item := i.tree.Max()
	i.RUnlock()
	if item == nil {
		return nil
	}
	return item.(*Inode)
}

func (i *InodeBTree) UpdateInodeTotalSize(addSize, subSize uint64) {
	if addSize == subSize {
		return
	}

	if addSize != 0 {
		atomic.AddUint64(&i.inodesTotalSize, addSize)
	}

	if subSize != 0 {
		current := atomic.LoadUint64(&i.inodesTotalSize)
		newValue := uint64(0)
		if current > subSize {
			newValue = current - subSize
		}
		atomic.StoreUint64(&i.inodesTotalSize, newValue)
	}
	return
}

func (i *DeletedInodeBTree) UpdateDelInodeTotalSize(addSize, subSize uint64) {
	if addSize == subSize {
		return
	}

	if addSize != 0 {
		atomic.AddUint64(&i.delInodesTotalSize, addSize)
	}

	if subSize != 0 {
		current := atomic.LoadUint64(&i.delInodesTotalSize)
		newValue := uint64(0)
		if current > subSize {
			newValue = current - subSize
		}
		atomic.StoreUint64(&i.delInodesTotalSize, newValue)
	}
}

func (i *InodeBTree) GetInodesTotalSize() uint64 {
	return atomic.LoadUint64(&i.inodesTotalSize)
}

func (i *DeletedInodeBTree) GetDelInodesTotalSize() uint64 {
	return atomic.LoadUint64(&i.delInodesTotalSize)
}

// BTree is the wrapper of Google's btree.
type BTree struct {
	sync.RWMutex
	tree *btree.BTree
}

// NewBtree creates a new btree.
func NewBtree() *BTree {
	return &BTree{
		tree: btree.New(defaultBTreeDegree),
	}
}

// Get returns the object of the given key in the btree.
func (b *BTree) Get(key BtreeItem) (item BtreeItem) {
	b.RLock()
	item = b.tree.Get(key)
	b.RUnlock()
	return
}

func (b *BTree) CopyGet(key BtreeItem) (item BtreeItem) {
	b.Lock()
	item = b.tree.CopyGet(key)
	b.Unlock()
	return
}

// Find searches for the given key in the btree.
func (b *BTree) Find(key BtreeItem, fn func(i BtreeItem)) {
	b.RLock()
	item := b.tree.Get(key)
	b.RUnlock()
	if item == nil {
		return
	}
	fn(item)
}

func (b *BTree) CopyFind(key BtreeItem, fn func(i BtreeItem)) {
	b.Lock()
	item := b.tree.CopyGet(key)
	fn(item)
	b.Unlock()
}

// Has checks if the key exists in the btree.
func (b *BTree) Has(key BtreeItem) (ok bool) {
	b.RLock()
	ok = b.tree.Has(key)
	b.RUnlock()
	return
}

// Delete deletes the object by the given key.
func (b *BTree) Delete(key BtreeItem) (item BtreeItem) {
	b.Lock()
	item = b.tree.Delete(key)
	b.Unlock()
	return
}

func (b *BTree) Execute(fn func(tree interface{}) interface{}) interface{} {
	b.Lock()
	defer b.Unlock()
	return fn(b)
}

// ReplaceOrInsert is the wrapper of google's btree ReplaceOrInsert.
func (b *BTree) ReplaceOrInsert(key BtreeItem, replace bool) (item BtreeItem, ok bool) {
	b.Lock()
	if replace {
		item = b.tree.ReplaceOrInsert(key)
		b.Unlock()
		ok = true
		return
	}

	item = b.tree.Get(key)
	if item == nil {
		item = b.tree.ReplaceOrInsert(key)
		b.Unlock()
		ok = true
		return
	}
	ok = false
	b.Unlock()
	return
}

// Ascend is the wrapper of the google's btree Ascend.
// This function scans the entire btree. When the data is huge, it is not recommended to use this function online.
// Instead, it is recommended to call GetTree to obtain the snapshot of the current btree, and then do the scan on the snapshot.
func (b *BTree) Ascend(fn func(i BtreeItem) bool) {
	b.RLock()
	b.tree.Ascend(fn)
	b.RUnlock()
}

// AscendRange is the wrapper of the google's btree AscendRange.
func (b *BTree) AscendRange(greaterOrEqual, lessThan BtreeItem, iterator func(i BtreeItem) bool) {
	b.RLock()
	b.tree.AscendRange(greaterOrEqual, lessThan, iterator)
	b.RUnlock()
}

// AscendGreaterOrEqual is the wrapper of the google's btree AscendGreaterOrEqual
func (b *BTree) AscendGreaterOrEqual(pivot BtreeItem, iterator func(i BtreeItem) bool) {
	b.RLock()
	b.tree.AscendGreaterOrEqual(pivot, iterator)
	b.RUnlock()
}

// GetTree returns the snapshot of a btree.
func (b *BTree) GetTree() *BTree {
	b.Lock()
	t := b.tree.Clone()
	b.Unlock()
	nb := NewBtree()
	nb.tree = t
	return nb
}

// Reset resets the current btree.
func (b *BTree) Reset() {
	b.Lock()
	b.tree.Clear(true)
	b.Unlock()
}

func (i *BTree) Release() {
	i.Reset()
}

func (i *BTree) SetApplyID(index uint64) {
}

func (i *BTree) GetApplyID() uint64 {
	return 0
}

func (i *BTree) GetPersistentApplyID() uint64 {
	return 0
}

func (i *BTree) PersistBaseInfo() error {
	return nil
}

func (i *BTree) CreateBatchWriteHandle() (interface{}, error) {
	return i, nil
}

func (i *BTree) CommitBatchWrite(handle interface{}, needCommitApplyID bool) error {
	return nil
}

func (i *BTree) ReleaseBatchWriteHandle(handle interface{}) error {
	return nil
}

func (i *BTree) BatchWriteCount(handle interface{}) (int, error) {
	return 0, nil
}

func (i *BTree) CommitAndReleaseBatchWriteHandle(handle interface{}, needCommitApplyID bool) error {
	return nil
}

func (i *BTree) ClearBatchWriteHandle(handle interface{}) error {
	return nil
}

func (i *BTree) SetCursor(cursor uint64) {
	return
}

func (i *BTree) GetCursor() uint64 {
	return 0
}

func (i *BTree) Flush() error {
	return nil
}

func (i *BTree) Count() uint64 {
	return uint64(i.Len())
}

// real count by type
func (i *BTree) RealCount() uint64 {
	return uint64(i.Len())
}

// Len returns the total number of items in the btree.
func (b *BTree) Len() (size int) {
	b.RLock()
	size = b.tree.Len()
	b.RUnlock()
	return
}
