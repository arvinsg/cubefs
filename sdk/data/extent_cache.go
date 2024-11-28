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

package data

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	se "github.com/cubefs/cubefs/util/sortedextent"
)

var (
	insertExtentKeyOption = &se.InsertOption{SkipDeletedExtentKeyProcess: true}
	insertExtentKeyContex = se.ContextWithInsertOption(context.Background(), insertExtentKeyOption)
)

// ExtentRequest defines the struct for the request of read or write an extent.
type ExtentRequest struct {
	FileOffset uint64
	Size       int
	Data       []byte
	ExtentKey  *proto.ExtentKey
	Done       bool
}

// String returns the string format of the extent request.
func (er *ExtentRequest) String() string {
	if er == nil {
		return ""
	}
	return fmt.Sprintf("FileOffset(%v) Size(%v) ExtentKey(%v)", er.FileOffset, er.Size, er.ExtentKey)
}

// NewExtentRequest returns a new extent request.
func NewExtentRequest(offset uint64, size int, data []byte, start, end uint64, ek *proto.ExtentKey) *ExtentRequest {
	req := &ExtentRequest{
		FileOffset: offset,
		Size:       size,
		ExtentKey:  ek,
	}
	if data != nil {
		req.Data = data[start:end]
	}
	return req
}

// ExtentCache defines the struct of the extent cache.
type ExtentCache struct {
	sync.RWMutex
	inode       uint64
	gen         uint64 // generation number
	size        uint64 // size of the cache
	root        *se.SortedExtents
	initialized bool
	refreshTime time.Time        // the last time to update extent cache
}

// NewExtentCache returns a new extent cache.
func NewExtentCache(inode uint64) *ExtentCache {
	return &ExtentCache{
		inode: inode,
		root:  se.NewSortedExtents(),
	}
}

// Refresh refreshes the extent cache.
func (cache *ExtentCache) Refresh(ctx context.Context, inode uint64, getExtents GetExtentsFunc, force bool) error {
	cache.UpdateRefreshTime()

	gen, size, extents, err := getExtents(ctx, inode)
	if err != nil {
		return err
	}
	//log.LogDebugf("Local ExtentCache before update: gen(%v) size(%v) extents(%v)", cache.gen, cache.size, cache.List())
	cache.update(gen, size, extents, force)
	//log.LogDebugf("Local ExtentCache after update: gen(%v) size(%v) extents(%v)", cache.gen, cache.size, cache.List())
	return nil
}

func (cache *ExtentCache) update(gen, size uint64, eks []proto.ExtentKey, force bool) {
	cache.Lock()
	defer cache.Unlock()

	log.LogDebugf("ExtentCache update: ino(%v) cache.gen(%v) cache.size(%v) gen(%v) size(%v) ekLen(%v) force(%v)",
		cache.inode, cache.gen, cache.size, gen, size, len(eks), force)

	cache.initialized = true
	if !force && cache.gen != 0 && cache.gen >= gen {
		log.LogDebugf("ExtentCache update: no need to update, ino(%v) gen(%v) size(%v)", cache.inode, gen, size)
		return
	}

	newRoot := se.NewSortedExtents()
	newRoot.Update(eks)
	oldRoot := cache.root

	cache.root = newRoot
	cache.gen = gen
	cache.size = size
	if force {
		return
	}

	// insert temporary ek to prevent read unconsistency
	oldRoot.Range(func(ek proto.ExtentKey) bool {
		if ek.PartitionId == 0 || ek.ExtentId == 0 {
			cache.insert(&ek, false)
			if log.IsDebugEnabled() {
				log.LogDebugf("ExtentCache update: ino(%v) insert temp ek(%v) ekLen(%v)", cache.inode, ek, cache.root.Len())
			}
		}
		return true
	})
}

func (cache *ExtentCache) UpdateRefreshTime() {
	cache.Lock()
	defer cache.Unlock()

	cache.refreshTime = time.Now()
	return
}

func (cache *ExtentCache) IsExpired(expireSecond int64) bool {
	cache.RLock()
	defer cache.RUnlock()

	return time.Since(cache.refreshTime) > time.Duration(expireSecond)*time.Second
}

func (cache *ExtentCache) Insert(ek *proto.ExtentKey, sync bool) {
	cache.Lock()
	defer cache.Unlock()
	cache.insert(ek, sync)
}

func (cache *ExtentCache) insert(ek *proto.ExtentKey, sync bool) {
	ekEnd := ek.FileOffset + uint64(ek.Size)
	deleteExtents := cache.root.Insert(insertExtentKeyContex, *ek, cache.inode)

	if sync {
		cache.gen++
	}
	if ekEnd > cache.size {
		cache.size = ekEnd
	}

	if log.IsDebugEnabled() {
		log.LogDebugf("ExtentCache Insert: ino(%v) ek(%v) deleteEks(%v) ekLen(%v)", cache.inode, ek, deleteExtents, cache.root.Len())
	}
}

func (cache *ExtentCache) Pre(offset uint64) (pre *proto.ExtentKey) {
	cache.RLock()
	defer cache.RUnlock()

	if ek, found := cache.root.PreviousExtentKey(offset); found {
		pre = &ek
		return
	}
	return
}

// Size returns the size of the cache.
func (cache *ExtentCache) Size() (size uint64, gen uint64) {
	cache.RLock()
	defer cache.RUnlock()
	return cache.size, cache.gen
}

// SetSize set the size of the cache.
func (cache *ExtentCache) SetSize(size uint64, sync bool) {
	cache.Lock()
	defer cache.Unlock()
	cache.size = size
	if sync {
		cache.gen++
	}
}

// List returns a list of the extents in the cache.
func (cache *ExtentCache) List() []proto.ExtentKey {
	cache.RLock()
	extents := cache.root.CopyExtents()
	cache.RUnlock()

	return extents
}

// PrepareRequests classifies the incoming request.
func (cache *ExtentCache) PrepareRequests(offset uint64, size int, data []byte) (requests []*ExtentRequest, fileSize uint64) {
	requests = make([]*ExtentRequest, 0)
	start := offset
	end := offset + uint64(size)

	cache.RLock()
	defer cache.RUnlock()

	fileSize = cache.size
	cache.root.VisitByFileRange(offset, uint32(size), func(ek proto.ExtentKey) bool {
		ekStart := ek.FileOffset
		ekEnd := ek.FileOffset + uint64(ek.Size)

		if log.IsDebugEnabled() {
			log.LogDebugf("PrepareRequests: ino(%v) start(%v) end(%v) ekStart(%v) ekEnd(%v)", cache.inode, start, end, ekStart, ekEnd)
		}

		if end <= ekStart {
			return false
		}

		if start < ekStart {
			if end <= ekStart {
				return false
			} else if end < ekEnd {
				// add hole (start, ekStart)
				req := NewExtentRequest(start, int(ekStart-start), data, start-offset, ekStart-offset, nil)
				requests = append(requests, req)
				// add non-hole (ekStart, end)
				req = NewExtentRequest(ekStart, int(end-ekStart), data, ekStart-offset, end-offset, &ek)
				requests = append(requests, req)
				start = end
				return false
			} else {
				// add hole (start, ekStart)
				req := NewExtentRequest(start, int(ekStart-start), data, start-offset, ekStart-offset, nil)
				requests = append(requests, req)

				// add non-hole (ekStart, ekEnd)
				req = NewExtentRequest(ekStart, int(ekEnd-ekStart), data, ekStart-offset, ekEnd-offset, &ek)
				requests = append(requests, req)

				start = ekEnd
				return true
			}
		} else if start < ekEnd {
			if end <= ekEnd {
				// add non-hole (start, end)
				req := NewExtentRequest(start, int(end-start), data, start-offset, end-offset, &ek)
				requests = append(requests, req)
				start = end
				return false
			} else {
				// add non-hole (start, ekEnd), start = ekEnd
				req := NewExtentRequest(start, int(ekEnd-start), data, start-offset, ekEnd-offset, &ek)
				requests = append(requests, req)
				start = ekEnd
				return true
			}
		} else {
			return true
		}
	})

	if start < end {
		if log.IsDebugEnabled() {
			log.LogDebugf("PrepareRequests: ino(%v) start(%v) end(%v)", cache.inode, start, end)
		}
		// add hole (start, end)
		req := NewExtentRequest(start, int(end-start), data, start-offset, end-offset, nil)
		requests = append(requests, req)
	}

	return
}
