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
	"encoding/binary"
	"fmt"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"io"
)

type FSMDeletedINode struct {
	inode uint64
}

func NewFSMDeletedINode(ino uint64) *FSMDeletedINode {
	fi := new(FSMDeletedINode)
	fi.inode = ino
	return fi
}

func (i *FSMDeletedINode) Marshal() (res []byte, err error) {
	res = make([]byte, 8)
	binary.BigEndian.PutUint64(res, i.inode)
	return
}

func (i *FSMDeletedINode) Unmarshal(data []byte) (err error) {
	i.inode = binary.BigEndian.Uint64(data)
	return
}

type FSMDeletedINodeBatch []*FSMDeletedINode

func (db FSMDeletedINodeBatch) Marshal() (data []byte, err error) {
	buff := bytes.NewBuffer(make([]byte, 0))
	err = binary.Write(buff, binary.BigEndian, uint32(len(db)))
	if err != nil {
		return
	}

	for _, di := range db {
		var bs []byte
		bs, err = di.Marshal()
		if err != nil {
			return
		}
		err = binary.Write(buff, binary.BigEndian, uint32(len(bs)))
		if err != nil {
			return
		}
		_, err = buff.Write(bs)
		if err != nil {
			return
		}
	}
	data = buff.Bytes()
	return
}

func FSMDeletedINodeBatchUnmarshal(raw []byte) (FSMDeletedINodeBatch, error) {
	buff := bytes.NewBuffer(raw)
	var batchLen uint32
	if err := binary.Read(buff, binary.BigEndian, &batchLen); err != nil {
		return nil, err
	}

	result := make(FSMDeletedINodeBatch, 0, int(batchLen))

	var dataLen uint32
	for j := 0; j < int(batchLen); j++ {
		if err := binary.Read(buff, binary.BigEndian, &dataLen); err != nil {
			return nil, err
		}
		data := make([]byte, int(dataLen))
		if _, err := buff.Read(data); err != nil {
			return nil, err
		}
		ino := new(FSMDeletedINode)
		if err := ino.Unmarshal(data); err != nil {
			return nil, err
		}
		result = append(result, ino)
	}

	return result, nil
}

type fsmOpDeletedInodeResponse struct {
	Status uint8  `json:"st"`
	Inode  uint64 `json:"ino"`
}

func (mp *metaPartition) mvToDeletedInodeTree(dbHandle interface{}, inode *Inode, timestamp int64) (status uint8, err error) {
	status = proto.OpOk
	dino := NewDeletedInode(inode, timestamp)

	var resp *fsmOpDeletedInodeResponse
	resp, err = mp.fsmCreateDeletedInode(dbHandle, dino)
	if err != nil {
		log.LogErrorf("[mvToDeletedInodeTree], inode: %v, status: %v, err: %v", inode, status, err)
		return
	}
	status = resp.Status
	if status != proto.OpOk && status != proto.OpExistErr {
		log.LogErrorf("[mvToDeletedInodeTree], inode: %v, status: %v", inode, status)
		return
	}

	if _, err = mp.inodeTree.Delete(dbHandle, inode.Inode); err != nil {
		log.LogErrorf("[mvToDeletedInodeTree], inode(%v) deleted failed(%v)", inode, err)
	}
	mp.updateInodesTotalSize(0, inode.Size)
	return
}

func (mp *metaPartition) fsmCreateDeletedInode(dbHandle interface{}, dino *DeletedINode) (rsp *fsmOpDeletedInodeResponse, err error) {
	rsp = new(fsmOpDeletedInodeResponse)
	rsp.Inode = dino.Inode.Inode
	rsp.Status = proto.OpOk
	mp.setAllocatorIno(dino.Inode.Inode)
	var existDelIno *DeletedINode
	if existDelIno, err = mp.inodeDeletedTree.RefGet(dino.Inode.Inode); err != nil {
		rsp.Status = proto.OpErr
		return
	}
	if existDelIno != nil {
		mp.updateDelInodesTotalSize(0, existDelIno.Size)
		log.LogErrorf("[fsmCreateDeletedInode], partitionID(%v), delInode(%v) already exist, exist delInode(%v)",
			mp.config.PartitionId, dino, existDelIno)
	}

	//if exist, replace
	if _, _, err = mp.inodeDeletedTree.Create(dbHandle, dino, true); err != nil {
		rsp.Status = proto.OpErr
		return
	}
	mp.updateDelInodesTotalSize(dino.Size, 0)
	return
}

func (mp *metaPartition) fsmBatchRecoverDeletedInode(dbHandle interface{}, inos FSMDeletedINodeBatch) (rsp []*fsmOpDeletedInodeResponse, err error) {
	defer func() {
		if err != nil {
			for index := 0; index < len(inos); index++ {
				rsp = append(rsp, &fsmOpDeletedInodeResponse{Status: proto.OpErr, Inode: inos[index].inode})
			}
		}
	}()
	for _, ino := range inos {
		var resp *fsmOpDeletedInodeResponse
		resp, err = mp.recoverDeletedInode(dbHandle, ino.inode)
		if err != nil {
			rsp = rsp[:0]
			return
		}
		if resp.Status != proto.OpOk {
			rsp = append(rsp, resp)
		}
	}
	return
}

func (mp *metaPartition) fsmRecoverDeletedInode(dbHandle interface{}, ino *FSMDeletedINode) (
	resp *fsmOpDeletedInodeResponse, err error) {
	return mp.recoverDeletedInode(dbHandle, ino.inode)
}

func (mp *metaPartition) recoverDeletedInode(dbHandle interface{}, inodeID uint64) (
	resp *fsmOpDeletedInodeResponse, err error) {
	resp = new(fsmOpDeletedInodeResponse)
	resp.Inode = inodeID
	resp.Status = proto.OpOk

	var (
		currInode    *Inode
		deletedInode *DeletedINode
		ok           bool
	)

	defer func() {
		if resp.Status != proto.OpOk {
			log.LogDebugf("[recoverDeletedInode], partitionID(%v), inode(%v), status: %v",
				mp.config.PartitionId, inodeID, resp.Status)
		}
	}()

	dino := NewDeletedInodeByID(inodeID)
	currInode, err = mp.inodeTree.Get(inodeID)
	if err != nil {
		resp.Status = proto.OpErr
		return
	}
	deletedInode, err = mp.inodeDeletedTree.Get(inodeID)
	if err != nil {
		resp.Status = proto.OpErr
		return
	}
	if currInode != nil {
		if deletedInode != nil {
			log.LogCriticalf("[recoverDeletedInode], partitionID(%v), curInode(%v), delInode(%v)",
				mp.config.PartitionId, currInode, deletedInode)
			if _, err = mp.inodeDeletedTree.Delete(dbHandle, inodeID); err != nil {
				resp.Status = proto.OpErr
				return
			}
			mp.updateDelInodesTotalSize(0, deletedInode.Size)
			return
		}

		if currInode.ShouldDelete() {
			log.LogDebugf("[recoverDeletedInode], the inode[%v] 's deleted flag is invalid", inodeID)
			currInode.CancelDeleteMark()
		}
		if !proto.IsDir(currInode.Type) {
			currInode.IncNLink() // TODO: How to handle idempotent?
		}
		if err = mp.inodeTree.Update(dbHandle, currInode); err != nil {
			resp.Status = proto.OpErr
			return
		}
		log.LogDebugf("[recoverDeletedInode], success to increase the link of inode[%v]", inodeID)
		return
	}

	if deletedInode == nil {
		log.LogErrorf("[recoverDeletedInode], not found the inode[%v] from deletedTree", dino)
		resp.Status = proto.OpNotExistErr
		return
	}

	if deletedInode.IsExpired {
		log.LogWarnf("[recoverDeletedInode], inode: [%v] is expired", deletedInode)
		resp.Status = proto.OpNotExistErr
		return
	}

	inoPtr := deletedInode.buildInode()
	inoPtr.CancelDeleteMark()
	if inoPtr.IsEmptyDir() {
		inoPtr.NLink = 2
	} else {
		inoPtr.IncNLink()
	}
	var existIno *Inode
	existIno, ok, err = mp.inodeTree.Create(dbHandle, inoPtr, false)
	if err != nil {
		log.LogErrorf("[recoverDeletedInode], failed to add inode to inodeTree, inode: (%v), error: (%v)", inoPtr, err)
		resp.Status = proto.OpErr
		return
	}
	if !ok {
		log.LogErrorf("[recoverDeletedInode], failed to add inode to inodeTree, inode exist, inode(%v) existIno(%v)", inoPtr, existIno)
		resp.Status = proto.OpExistErr
		return
	}
	mp.updateInodesTotalSize(inoPtr.Size, 0)
	if _, err = mp.inodeDeletedTree.Delete(dbHandle, dino.Inode.Inode); err != nil {
		log.LogErrorf("[recoverDeletedInode], failed to delete deletedInode, delInode: (%v), error: (%v)", dino, err)
		resp.Status = proto.OpErr
	}
	mp.updateDelInodesTotalSize(0, deletedInode.Size)
	return
}

func (mp *metaPartition) fsmBatchCleanDeletedInode(dbHandle interface{}, inos FSMDeletedINodeBatch) (rsp []*fsmOpDeletedInodeResponse, err error) {
	rsp = make([]*fsmOpDeletedInodeResponse, 0)
	defer func() {
		if err != nil {
			for index := 0; index < len(inos); index++ {
				rsp = append(rsp, &fsmOpDeletedInodeResponse{Status: proto.OpErr, Inode: inos[index].inode})
			}
		}
	}()

	for _, ino := range inos {
		var resp *fsmOpDeletedInodeResponse
		resp, err = mp.cleanDeletedInode(dbHandle, ino.inode)
		if err == rocksDBError {
			rsp = rsp[:0]
			return
		}
		if resp.Status != proto.OpOk {
			rsp = append(rsp, resp)
		}
	}
	return
}

func (mp *metaPartition) fsmCleanDeletedInode(dbHandle interface{}, ino *FSMDeletedINode) (
	resp *fsmOpDeletedInodeResponse, err error) {
	return mp.cleanDeletedInode(dbHandle, ino.inode)
}

func (mp *metaPartition) cleanDeletedInode(dbHandle interface{}, inode uint64) (
	resp *fsmOpDeletedInodeResponse, err error) {
	resp = new(fsmOpDeletedInodeResponse)
	resp.Inode = inode
	resp.Status = proto.OpOk
	defer func() {
		log.LogDebugf("[cleanDeletedInode], inode: (%v), status:[%v]", inode, resp.Status)
	}()

	var dino *DeletedINode
	dino, err = mp.inodeDeletedTree.Get(inode)
	if err != nil {
		resp.Status = proto.OpErr
		return
	}

	if dino == nil {
		resp.Status = proto.OpNotExistErr
		return
	}

	begDen := newPrimaryDeletedDentry(dino.Inode.Inode, "", 0, 0)
	endDen := newPrimaryDeletedDentry(dino.Inode.Inode+1, "", 0, 0)
	var children int
	err = mp.dentryDeletedTree.RangeWithPrefix(begDen, begDen, endDen, func(d *DeletedDentry) (bool, error) {
		children++
		if children > 0 {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		resp.Status = proto.OpErr
		return
	}

	if children > 0 {
		resp.Status = proto.OpExistErr
		return
	}

	if dino.IsEmptyDir() || dino.IsDeleting() {
		dino.setExpired()
		mp.freeList.Push(dino.Inode.Inode)
		if err = mp.inodeDeletedTree.Update(dbHandle, dino); err != nil {
			resp.Status = proto.OpErr
		}
		return
	}
	resp.Status = proto.OpErr
	return
}

func (mp *metaPartition) fsmCleanExpiredInode(dbHandle interface{}, inos FSMDeletedINodeBatch) (rsp []*fsmOpDeletedInodeResponse, err error) {
	defer func() {
		if err != nil {
			for index := 0; index < len(inos); index++ {
				rsp = append(rsp, &fsmOpDeletedInodeResponse{Status: proto.OpErr, Inode: inos[index].inode})
			}
		}
	}()
	for _, ino := range inos {
		var resp *fsmOpDeletedInodeResponse
		resp, err = mp.cleanExpiredInode(dbHandle, ino.inode)
		if err == rocksDBError {
			rsp = rsp[:0]
			return
		}
		if resp.Status != proto.OpOk {
			rsp = append(rsp, resp)
		}
	}
	return
}

func (mp *metaPartition) cleanExpiredInode(dbHandle interface{}, ino uint64) (
	resp *fsmOpDeletedInodeResponse, err error) {
	resp = new(fsmOpDeletedInodeResponse)
	resp.Inode = ino
	resp.Status = proto.OpOk
	defer func() {
		log.LogDebugf("[cleanExpiredInode], inode: %v, status: %v", ino, resp.Status)
	}()

	var di *DeletedINode
	di, err = mp.inodeDeletedTree.Get(ino)
	if err != nil {
		resp.Status = proto.OpErr
		return
	}
	if di == nil {
		return
	}

	if di.IsEmptyDir() || di.IsDeleting() {
		di.setExpired()
		mp.freeList.Push(di.Inode.Inode)
		if err = mp.inodeDeletedTree.Update(dbHandle, di); err != nil {
			resp.Status = proto.OpErr
		}
		return
	}
	resp.Status = proto.OpErr
	return
}

func (mp *metaPartition) internalClean(dbHandle interface{}, val []byte) (err error) {
	log.LogInfof("[internalClean] clean inode start")
	defer func() {
		log.LogInfof("[internalClean] clean inode finished, result:%v", err)
	}()
	if len(val) == 0 {
		return
	}
	buf := bytes.NewBuffer(val)
	var inodeID uint64
	for {
		err = binary.Read(buf, binary.BigEndian, &inodeID)
		if err != nil {
			if err == io.EOF {
				err = nil
				return
			}
			return
		}
		log.LogDebugf("[internalClean] received internal delete: partitionID(%v) inode(%v)",
			mp.config.PartitionId, inodeID)
		if err = mp.internalCleanDeletedInode(dbHandle, inodeID); err != nil {
			log.LogErrorf("[internalClean] clean deleted inode failed, partitionID(%v) inode(%v)",
				mp.config.PartitionId, inodeID)
			return
		}
	}
}

func (mp *metaPartition) internalCleanDeletedInode(dbHandle interface{}, inodeID uint64) (err error) {
	mp.freeList.Remove(inodeID)
	var ok bool
	var dino *DeletedINode
	if dino, err = mp.inodeDeletedTree.RefGet(inodeID); err != nil {
		log.LogErrorf("[internalCleanDeletedInode] partitionID(%v) get dino(%v) from deleted inode tree error:%v",
			mp.config.PartitionId, inodeID, err)
		return
	}

	if ok, err = mp.inodeDeletedTree.Delete(dbHandle, inodeID); err != nil {
		log.LogErrorf("[internalCleanDeletedInode] partitionID(%v) delete dino(%v) from deleted inode tree error:%v",
			mp.config.PartitionId, inodeID, err)
		return
	}

	if !ok {
		log.LogDebugf("[internalCleanDeletedInode], partitionID(%v) dino(%v) not exist", mp.config.PartitionId, inodeID)
		//check inode tree, if exist in inode tree, do not clear inode id in bitmap
		var inode *Inode
		if inode, err = mp.inodeTree.RefGet(inodeID); err != nil {
			log.LogErrorf("[internalCleanDeletedInode] partitionID(%v) get ino(%v) from inode tree error:%v",
				mp.config.PartitionId, inodeID, err)
			return
		}

		if inode != nil {
			if enableBitMapAllocator, _ := mp.topoManager.GetBitMapAllocatorEnableFlag(mp.config.VolName); enableBitMapAllocator {
				exporter.WarningRocksdbError(fmt.Sprintf("action[internalCleanDeletedInode] clusterID[%s] " +
					"volumeName[%s] partitionID[%v] confict inode[%v]", mp.manager.metaNode.clusterId, mp.config.VolName,
					mp.config.PartitionId, inode.Inode))
			}
			//exist in inode tree, skip clear bitmap
			log.LogDebugf("[internalCleanDeletedInode] partitionID(%v) ino(%v) exist in inode tree",
				mp.config.PartitionId, inodeID)
			return
		}
	} else {
		if dino != nil {
			mp.updateDelInodesTotalSize(0, dino.Size)
		}
		log.LogDebugf("[internalCleanDeletedInode] partitionID(%v) dino(%v) delete success", mp.config.PartitionId, inodeID)
	}

	if _, err = mp.extendTree.Delete(dbHandle, inodeID); err != nil { // Also delete extend attribute.
		log.LogErrorf("[internalCleanDeletedInode] partitionID(%v) deleted extend failed, ino:%v, error:%v",
			mp.config.PartitionId, inodeID, err)
		return
	}
	log.LogDebugf("[internalCleanDeletedInode], partitionID(%v) clean deleted ino: %v result: %v",
		mp.config.PartitionId, inodeID, err)
	mp.clearAllocatorIno(inodeID)
	return
}
