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
	"context"
	"fmt"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/multirate"
	"github.com/cubefs/cubefs/util/topology"
	"net"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

const (
	AsyncDeleteInterval           = 10 * time.Second
	UpdateVolTicket               = 2 * time.Minute
	BatchCounts                   = 128
	OpenRWAppendOpt               = os.O_CREATE | os.O_RDWR | os.O_APPEND
	TempFileValidTime             = 86400 //units: sec
	DeleteInodeFileExtension      = "INODE_DEL"
	DeleteWorkerCnt               = 10
	InodeNLink0DelayDeleteSeconds = 24 * 3600
	maxDeleteInodeFileSize        = MB
)

func (mp *metaPartition) startFreeList() (err error) {
	if mp.delInodeFp, err = os.OpenFile(path.Join(mp.config.RootDir,
		DeleteInodeFileExtension), OpenRWAppendOpt, 0644); err != nil {
		return
	}
	mp.renameDeleteEKRecordFile(delExtentKeyList, prefixDelExtentKeyListBackup)
	mp.renameDeleteEKRecordFile(InodeDelExtentKeyList, PrefixInodeDelExtentKeyListBackup)
	delExtentListDir := path.Join(mp.config.RootDir, delExtentKeyList)
	if mp.delEKFd, err = os.OpenFile(delExtentListDir, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err != nil {
		log.LogErrorf("[startFreeList] mp[%v] create delEKListFile(%s) failed:%v",
			mp.config.PartitionId, delExtentListDir, err)
		return
	}

	inodeDelEKListDir := path.Join(mp.config.RootDir, InodeDelExtentKeyList)
	if mp.inodeDelEkFd, err = os.OpenFile(inodeDelEKListDir, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err != nil {
		log.LogErrorf("[startFreeList] mp[%v] create inodeDelEKListFile(%s) failed:%v",
			mp.config.PartitionId, inodeDelEKListDir, err)
		return
	}

	// start vol update ticket
	go mp.deleteWorker()
	mp.startToDeleteExtents()
	return
}

const (
	MinDeleteBatchCounts = 100
	MaxSleepCnt          = 10
)

func (mp *metaPartition) GetDelInodeInterval() uint64{
	interval := mp.topoManager.GetDelInodeIntervalConf(mp.config.VolName)
	clusterWaitValue := atomic.LoadUint64(&deleteWorkerSleepMs)
	if interval == 0 {
		interval = clusterWaitValue
	}
	return interval
}

func (mp *metaPartition) GetBatchDelInodeCnt() uint64{
	clusterDelCnt := DeleteBatchCount()   // default 128
	batchDelCnt := mp.topoManager.GetBatchDelInodeCntConf(mp.config.VolName)
	if batchDelCnt == 0 {
		batchDelCnt = clusterDelCnt
	}

	return batchDelCnt
}

func (mp *metaPartition) GetTruncateEKCountEveryTime() (count int) {
	count = DefaultTruncateEKCount
	truncateEKCount := mp.topoManager.GetTruncateEKCountConf(mp.config.VolName)
	if truncateEKCount > 0 {
		count = truncateEKCount
	}
	return
}

func (mp *metaPartition) deleteWorker() {
	freeLaterInodesCleanTimer := time.NewTicker(time.Minute*10)
	var (
		//idx      uint64
		isLeader   bool
		sleepCnt   uint64
		leaderTerm uint64
	)
	for {
		select {
		case <-mp.stopC:
			if mp.delInodeFp != nil {
				mp.delInodeFp.Sync()
				mp.delInodeFp.Close()
			}

			if mp.inodeDelEkFd != nil {
				mp.inodeDelEkFd.Sync()
				mp.inodeDelEkFd.Close()
			}
			freeLaterInodesCleanTimer.Stop()
			return
		case <- freeLaterInodesCleanTimer.C:
			//clean all free later inodes
			mp.freeLaterInodes = make(map[uint64]byte, 0)
		default:
		}

		if _, isLeader = mp.IsLeader(); !isLeader {
			mp.freeLaterInodes = make(map[uint64]byte, 0)
			time.Sleep(AsyncDeleteInterval)
			continue
		}

		//DeleteWorkerSleepMs()
		interval := mp.GetDelInodeInterval()
		time.Sleep(time.Duration(interval) * time.Millisecond)

		//TODO: add sleep time value
		isForceDeleted := sleepCnt%MaxSleepCnt == 0
		if !isForceDeleted && mp.freeList.Len() < MinDeleteBatchCounts {
			time.Sleep(AsyncDeleteInterval)
			sleepCnt++
			continue
		}

		batchCount := mp.GetBatchDelInodeCnt()
		buffSlice := mp.freeList.Get(int(batchCount))
		//mp.persistDeletedInodes(buffSlice)
		leaderTerm, isLeader = mp.LeaderTerm()
		if !isLeader {
			time.Sleep(AsyncDeleteInterval)
			continue
		}
		mp.deleteMarkedInodes(context.Background(), buffSlice, leaderTerm)
		sleepCnt++
	}
}

// delete Extents by Partition,and find all successDelete inode
func (mp *metaPartition) batchDeleteExtentsByPartition(ctx context.Context, partitionDeleteExtents map[uint64][]*proto.MetaDelExtentKey,
	allInodes []*Inode, truncateCount int) (shouldCommit []*Inode) {
	occurErrors := make(map[uint64]error)
	shouldCommit = make([]*Inode, 0, DeleteBatchCount())
	var (
		wg   sync.WaitGroup
		lock sync.Mutex
	)

	//wait all Partition do BatchDeleteExtents fininsh
	for partitionID, extents := range partitionDeleteExtents {
		wg.Add(1)
		go func(partitionID uint64, extents []*proto.MetaDelExtentKey) {
			start := 0
			for {
				end := start + DefaultDelEKBatchCount//每次删除的ek个数不超过1000
				if end > len(extents) {
					end = len(extents)
				}
				mp.deleteEKWithRateLimit(end-start)
				perr := mp.doBatchDeleteExtentsByPartition(ctx, partitionID, extents[start:end])
				lock.Lock()
				occurErrors[partitionID] = perr
				lock.Unlock()
				if perr != nil {
					break
				}
				if end == len(extents) {
					break
				}
				start = end
			}
			wg.Done()
		}(partitionID, extents)
	}
	wg.Wait()

	for dpID, perr := range occurErrors {
		if perr == nil {
			continue
		}

		errorEKs, _ := partitionDeleteExtents[dpID]
		for _, ek := range errorEKs {
			mp.freeLaterInodes[ek.InodeIDInner] = 0
			log.LogWarnf("deleteInode metaPartition(%v) Inode(%v) ek(%v) delete error(%v)",mp.config.PartitionId,
				ek.InodeIDInner, ek, perr)
		}
	}

	for _, inode := range allInodes {
		if _, ok := mp.freeLaterInodes[inode.Inode]; ok {
			continue
		}

		if mp.HasRocksDBStore() || inode.Extents == nil || inode.Extents.Len() == 0 {
			shouldCommit = append(shouldCommit, inode)
			continue
		}

		ekCount := inode.Extents.TruncateByCountFromEnd(truncateCount)
		if ekCount == 0 {
			shouldCommit = append(shouldCommit, inode)
		}
	}
	return
}

func (mp *metaPartition) deleteEKWithRateLimit(delEKCount int) {
	ctx := context.Background()
	if atomic.LoadUint64(&delExtentRateLimitLocal) > 0 {
		delExtentRateLimiterLocal.WaitN(ctx, delEKCount)
		return
	}

	ps := multirate.Properties{
		{multirate.PropertyTypeVol, mp.config.VolName},
		{multirate.PropertyTypeOp,strconv.Itoa(int(proto.OpMarkDelete))},
		{multirate.PropertyTypePartition, strconv.Itoa(int(mp.config.PartitionId))},
	}
	stat := multirate.Stat{
		Count: delEKCount,
	}
	multirate.WaitN(context.Background(), ps, stat)
	return
}

func (mp *metaPartition) recordInodeDeleteEkInfo(info *Inode, truncateIndexOffset int) {
	if info == nil || info.Extents == nil ||info.Extents.Len() == 0 {
		return
	}

	log.LogDebugf("[recordInodeDeleteEkInfo] mp[%v] delEk[ino:%v] record %d eks", mp.config.PartitionId, info.Inode, info.Extents.Len())
	var (
		data []byte
		err  error
	)
	inoDelExtentListDir := path.Join(mp.config.RootDir, InodeDelExtentKeyList)
	if mp.inodeDelEkRecordCount >= defMaxDelEKRecord {
		_ = mp.inodeDelEkFd.Close()
		mp.inodeDelEkFd = nil
		mp.renameDeleteEKRecordFile(InodeDelExtentKeyList, PrefixInodeDelExtentKeyListBackup)
		mp.inodeDelEkRecordCount = 0
	}
	timeStamp := time.Now().Unix()

	info.Extents.RangeWithIndexOffset(truncateIndexOffset, func(ek proto.ExtentKey) bool {

		delEk := ek.ConvertToMetaDelEk(info.Inode, uint64(proto.DelEkSrcTypeFromDelInode), timeStamp)
		ekBuff := make([]byte, proto.ExtentDbKeyLengthWithIno)
		delEk.MarshalDeleteEKRecord(ekBuff)
		data = append(data, ekBuff...)
		mp.inodeDelEkRecordCount++
		return true
	})

	if mp.inodeDelEkFd == nil {
		if mp.inodeDelEkFd, err = os.OpenFile(inoDelExtentListDir, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err != nil {
			log.LogErrorf("[recordInodeDeleteEkInfo] mp[%v] delEk[ino:%v, ek:%v] create delEKListFile(%s) failed:%v",
				mp.config.PartitionId, info.Inode, info.Extents.Len(), InodeDelExtentKeyList, err)
			return
		}
	}

	defer func() {
		if err != nil {
			_ = mp.inodeDelEkFd.Close()
			mp.inodeDelEkFd = nil
		}
	}()

	if _, err = mp.inodeDelEkFd.Write(data); err != nil {
		log.LogErrorf("[recordInodeDeleteEkInfo] mp[%v] delEk[ino:%v, ek:%v] write file(%s) failed:%v",
			mp.config.PartitionId, info.Inode, info.Extents.Len(), inoDelExtentListDir, err)
		return
	}
	log.LogDebugf("[recordInodeDeleteEkInfo] mp[%v] delEk[ino:%v, ek:%v] record success", mp.config.PartitionId, info.Inode, info.Extents.Len())
	return
}

// Delete the marked inodes.
func (mp *metaPartition) deleteMarkedInodes(ctx context.Context, inoSlice []uint64, term uint64) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf(fmt.Sprintf("metaPartition(%v) deleteMarkedInodes panic (%v), stack:%v", mp.config.PartitionId, r, debug.Stack()))
		}
	}()
	truncateEKCount := mp.GetTruncateEKCountEveryTime()
	shouldCommit := make([]*Inode, 0, DeleteBatchCount())
	//shouldRePushToFreeList := make([]*Inode, 0)
	allDeleteExtents := make(map[string]uint64)
	deleteExtentsByPartition := make(map[uint64][]*proto.MetaDelExtentKey)
	allInodes := make([]*Inode, 0)
	for _, ino := range inoSlice {
		if _, ok := mp.freeLaterInodes[ino]; ok {
			log.LogDebugf("[deleteMarkedInodes], inode(%v) need free later", ino)
			time.Sleep(5*time.Microsecond)
			continue
		}
		var inodeVal *Inode
		dio, err := mp.inodeDeletedTree.RefGet(ino)
		if err != nil {
			log.LogWarnf("[deleteMarkedInodes], not found the deleted inode: %v", ino)
			continue
		}
		if dio == nil || !dio.IsExpired {
			//unexpected, just for avoid mistake delete
			mp.freeList.Remove(ino)
			log.LogWarnf("[deleteMarkedInodes], unexpired deleted inode: %v", ino)
			continue
		}
		inodeVal = dio.buildInode()

		if inodeVal.Extents == nil || inodeVal.Extents.Len() == 0 {
			allInodes = append(allInodes, inodeVal)
			continue
		}

		inodeID := inodeVal.Inode
		if exist, _ := mp.hasXAttr(inodeID, proto.XAttrKeyOSSMultipart); exist {
			inodeID = 0
		}

		truncateIndexOffset := inodeVal.Extents.Len() - truncateEKCount
		if truncateIndexOffset < 0 || mp.HasRocksDBStore() {
			truncateIndexOffset = 0
		}

		mp.recordInodeDeleteEkInfo(inodeVal, truncateIndexOffset)
		inodeVal.Extents.RangeWithIndexOffset(truncateIndexOffset, func(ek proto.ExtentKey) bool {
			ext := &ek
			_, ok := allDeleteExtents[ext.GetExtentKey()]
			if !ok {
				allDeleteExtents[ek.GetExtentKey()] = ino
			}
			exts, ok := deleteExtentsByPartition[ek.PartitionId]
			if !ok {
				exts = make([]*proto.MetaDelExtentKey, 0)
			}
			exts = append(exts, &proto.MetaDelExtentKey{
				ExtentKey:    ek,
				InodeId:      inodeID,
				InodeIDInner: inodeVal.Inode,
			})
			deleteExtentsByPartition[ext.PartitionId] = exts
			return true
		})
		allInodes = append(allInodes, inodeVal)
	}
	shouldCommit = mp.batchDeleteExtentsByPartition(ctx, deleteExtentsByPartition, allInodes, truncateEKCount)
	bufSlice := make([]byte, 0, 8*len(shouldCommit))
	for _, inode := range shouldCommit {
		bufSlice = append(bufSlice, inode.MarshalKey()...)
	}

	leaderTerm, isLeader := mp.LeaderTerm()
	if !isLeader || leaderTerm != term {
		log.LogErrorf("[deleteMarkedInodes] partitionID(%v) leader change", mp.config.PartitionId)
		return
	}

	err := mp.syncToRaftFollowersFreeInode(ctx, bufSlice)
	if err != nil {
		log.LogWarnf("[deleteInodeTreeOnRaftPeers] raft commit inode list: %v, "+
			"response %s", shouldCommit, err.Error())
	}
	log.LogInfof("metaPartition(%v) deleteInodeCnt(%v) inodeCnt(%v)", mp.config.PartitionId, len(shouldCommit), mp.inodeTree.Count())
}

func (mp *metaPartition) syncToRaftFollowersFreeInode(ctx context.Context, hasDeleteInodes []byte) (err error) {
	if len(hasDeleteInodes) == 0 {
		return
	}
	//_, err = mp.submit(opFSMInternalDeleteInode, hasDeleteInodes)
	_, err = mp.submit(ctx, opFSMInternalCleanDeletedInode, "", hasDeleteInodes, nil)

	return
}

const (
	notifyRaftFollowerToFreeInodesTimeOut = 60 * 2
)

func (mp *metaPartition) notifyRaftFollowerToFreeInodes(ctx context.Context, wg *sync.WaitGroup, target string, hasDeleteInodes []byte) (err error) {
	var conn *net.TCPConn
	conn, err = mp.config.ConnPool.GetConnect(target)
	defer func() {
		wg.Done()
		if err != nil {
			log.LogWarnf(err.Error())
			mp.config.ConnPool.PutConnect(conn, ForceClosedConnect)
		} else {
			mp.config.ConnPool.PutConnect(conn, NoClosedConnect)
		}
	}()
	if err != nil {
		return
	}
	request := NewPacketToFreeInodeOnRaftFollower(ctx, mp.config.PartitionId, hasDeleteInodes)
	if err = request.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		return
	}

	if err = request.ReadFromConn(conn, notifyRaftFollowerToFreeInodesTimeOut); err != nil {
		return
	}

	if request.ResultCode != proto.OpOk {
		err = fmt.Errorf("request(%v) error(%v)", request.GetUniqueLogId(), string(request.Data[:request.Size]))
	}

	return
}

func (mp *metaPartition) doDeleteMarkedInodes(ctx context.Context, ext *proto.MetaDelExtentKey) (err error) {
	// get the data node view
	tpObj := exporter.NewNodeAndVolTP(MetaPartitionDeleteEKUmpKey, mp.config.VolName)
	defer tpObj.SetWithCount(1, err)
	log.LogDebugf("doDeleteMarkedInodes mp(%v) ext(%v)", mp.config.PartitionId, ext)
	dp := mp.topoManager.GetPartitionFromCache(mp.config.VolName, ext.PartitionId)
	if dp == nil {
		mp.topoManager.FetchDataPartitionView(mp.config.VolName, ext.PartitionId)
		log.LogDebugf("doDeleteMarkedInodes find dataPartition(%v) in vol(%s) failed," +
			" force fetch data partition view", ext.PartitionId, mp.config.VolName)
		err = errors.NewErrorf("unknown dataPartitionID=%d in vol",
			ext.PartitionId)
		return
	}

	//delete the ec node
	if proto.IsEcFinished(dp.EcMigrateStatus) {
		err = mp.doDeleteEcMarkedInodes(ctx, dp, ext)
		return
	}else if dp.EcMigrateStatus == proto.Migrating {
		err = errors.NewErrorf("dp(%v) is migrate Ec, wait done", dp.PartitionID)
		return
	}

	// delete the data node
	if ext.SrcType != uint64(proto.DelEkSrcTypeFromFileMigMerge) {
		err = mp.doMarkDelete(ctx, dp, ext)
	} else {
		var isMismatchOp = false
		isMismatchOp, err = mp.doBatchTrashExtents(ctx, dp, ext)
		if isMismatchOp {
			err = mp.doMarkDelete(ctx, dp, ext)
		}
	}
	return
}

func (mp *metaPartition) doMarkDelete(ctx context.Context, dp *topology.DataPartition, ext *proto.MetaDelExtentKey) (err error) {
	var conn *net.TCPConn
	conn, err = mp.config.ConnPool.GetConnect(dp.Hosts[0])

	defer func() {
		if err != nil {
			mp.config.ConnPool.PutConnect(conn, ForceClosedConnect)
		} else {
			mp.config.ConnPool.PutConnect(conn, NoClosedConnect)
		}
	}()

	if err != nil {
		err = errors.NewErrorf("get conn from pool %s, "+
			"extents partitionId=%d, extentId=%d",
			err.Error(), ext.PartitionId, ext.ExtentId)
		return
	}

	p := NewPacketToDeleteExtent(ctx, dp, ext)
	if err = p.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		err = errors.NewErrorf("write to dataNode %s, %s", p.GetUniqueLogId(),
			err.Error())
		return
	}
	if err = p.ReadFromConn(conn, proto.ReadDeadlineTime); err != nil {
		err = errors.NewErrorf("read response from dataNode %s, %s",
			p.GetUniqueLogId(), err.Error())
		return
	}
	if p.ResultCode != proto.OpOk {
		if p.ResultCode == proto.OpTryOtherAddr {
			mp.topoManager.FetchDataPartitionView(mp.config.VolName, ext.PartitionId)
			log.LogDebugf("doDeleteMarkedInodes vol(%s) dataPartition(%v) not exist in host(%v)," +
				" force fetch data partition view", mp.config.VolName, ext.PartitionId, dp.Hosts[0])
		}
		err = errors.NewErrorf("[deleteMarkedInodes] %s response: %s", p.GetUniqueLogId(),
			p.GetResultMsg())
	}
	return
}

func (mp *metaPartition) doBatchTrashExtents(ctx context.Context, dp *topology.DataPartition, ext *proto.MetaDelExtentKey) (isMismatchOp bool, err error){
	var conn *net.TCPConn
	conn, err = mp.config.ConnPool.GetConnect(dp.Hosts[0])

	defer func() {
		if err != nil {
			mp.config.ConnPool.PutConnect(conn, ForceClosedConnect)
		} else {
			mp.config.ConnPool.PutConnect(conn, NoClosedConnect)
		}
	}()

	if err != nil {
		err = errors.NewErrorf("get conn from pool %s, "+
			"extents partitionId=%d, extentId=%d",
			err.Error(), ext.PartitionId, ext.ExtentId)
		return
	}

	p := NewPacketToBatchTrashExtent(ctx, dp, []*proto.MetaDelExtentKey{ext})
	if err = p.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		err = errors.NewErrorf("write to dataNode %s, %s", p.GetUniqueLogId(),
			err.Error())
		return
	}
	if err = p.ReadFromConn(conn, proto.ReadDeadlineTime); err != nil {
		err = errors.NewErrorf("read response from dataNode %s, %s",
			p.GetUniqueLogId(), err.Error())
		return
	}
	if p.ResultCode != proto.OpOk {
		if p.ResultCode == proto.OpTryOtherAddr {
			mp.topoManager.FetchDataPartitionView(mp.config.VolName, ext.PartitionId)
			log.LogDebugf("doDeleteMarkedInodes vol(%s) dataPartition(%v) not exist in host(%v)," +
				" force fetch data partition view", mp.config.VolName, ext.PartitionId, dp.Hosts[0])
		}
		if p.ResultCode == proto.OpArgMismatchErr && strings.Contains(string(p.Data), "unknown opcode"){
			isMismatchOp = true
			log.LogDebugf("doDeleteMarkedInodes vol(%s) dataPartition(%v) need use old op, host(%v),",
				mp.config.VolName, ext.PartitionId, dp.Hosts[0])
		}
		err = errors.NewErrorf("[deleteMarkedInodes] %s response: %s", p.GetUniqueLogId(),
			p.GetResultMsg())
	}
	return
}

func (mp *metaPartition) doBatchDeleteExtentsByPartition(ctx context.Context, partitionID uint64, exts []*proto.MetaDelExtentKey) (err error) {
	tpObj := exporter.NewNodeAndVolTP(MetaPartitionDeleteEKUmpKey, mp.config.VolName)
	defer tpObj.SetWithCount(int64(len(exts)), err)
	log.LogDebugf("doBatchDeleteExtentsByPartition mp(%v) dp(%v), extentCnt(%v)", mp.config.PartitionId, partitionID, len(exts))
	// get the data node view
	dp := mp.topoManager.GetPartitionFromCache(mp.config.VolName, partitionID)
	if dp == nil {
		mp.topoManager.FetchDataPartitionView(mp.config.VolName, partitionID)
		log.LogDebugf("doBatchDeleteExtentsByPartition find dataPartition(%v) in vol(%s) failed," +
			" force fetch data partition view", partitionID, mp.config.VolName)
		err = errors.NewErrorf("unknown dataPartitionID=%d in vol",
			partitionID)
		return
	}
	for _, ext := range exts {
		if ext.PartitionId != partitionID {
			err = errors.NewErrorf("BatchDeleteExtent do batchDelete on PartitionID(%v) but unexpect Extent(%v)", partitionID, ext)
			return
		}
	}

	// delete the ec node
	//delete the ec node
	if proto.IsEcFinished(dp.EcMigrateStatus) {
		err = mp.doBatchDeleteEcExtentsByPartition(ctx, dp, exts)
		return
	}else if dp.EcMigrateStatus == proto.Migrating {
		err = errors.NewErrorf("dp(%v) is migrate Ec, wait done", dp.PartitionID)
		return
	}

	// delete the data node
	conn, err := mp.config.ConnPool.GetConnect(dp.Hosts[0])

	defer func() {
		if err != nil {
			mp.config.ConnPool.PutConnect(conn, ForceClosedConnect)
		} else {
			mp.config.ConnPool.PutConnect(conn, NoClosedConnect)
		}
	}()

	if err != nil {
		err = errors.NewErrorf("get conn from pool %s, "+
			"extents partitionId=%d",
			err.Error(), partitionID)
		return
	}
	p := NewPacketToBatchDeleteExtent(ctx, dp, exts)
	if err = p.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		err = errors.NewErrorf("write to dataNode %s, %s", p.GetUniqueLogId(),
			err.Error())
		return
	}
	if err = p.ReadFromConn(conn, proto.ReadDeadlineTime*10); err != nil {
		err = errors.NewErrorf("read response from dataNode %s, %s",
			p.GetUniqueLogId(), err.Error())
		return
	}
	if p.ResultCode != proto.OpOk {
		if p.ResultCode == proto.OpTryOtherAddr {
			mp.topoManager.FetchDataPartitionView(mp.config.VolName, partitionID)
			log.LogDebugf("doBatchDeleteExtentsByPartition vol(%s) dataPartition(%v) not exist in host(%v)," +
				" force fetch data partition view", mp.config.VolName, partitionID, dp.Hosts[0])
		}
		err = errors.NewErrorf("[deleteMarkedInodes] %s response: %s", p.GetUniqueLogId(),
			p.GetResultMsg())
	}
	return
}

func (mp *metaPartition) persistDeletedInodes(inos []uint64) {
	if fileInfo, err := mp.delInodeFp.Stat(); err == nil && fileInfo.Size() >= maxDeleteInodeFileSize {
		mp.delInodeFp.Truncate(0)
	}
	for _, ino := range inos {
		if _, err := mp.delInodeFp.WriteString(fmt.Sprintf("%v\n", ino)); err != nil {
			log.LogWarnf("[persistDeletedInodes] failed store ino=%v", ino)
		}
	}
}

func (mp *metaPartition) doDeleteEcMarkedInodes(ctx context.Context, dp *topology.DataPartition, ext *proto.MetaDelExtentKey) (err error) {
	// delete the data node
	conn, err := mp.config.ConnPool.GetConnect(dp.EcHosts[0])

	defer func() {
		if err != nil {
			log.LogError(err)
			mp.config.ConnPool.PutConnect(conn, ForceClosedConnect)
		} else {
			mp.config.ConnPool.PutConnect(conn, NoClosedConnect)
		}
	}()

	if err != nil {
		err = errors.NewErrorf("get conn from pool %s, "+
			"extents partitionId=%d, extentId=%d",
			err.Error(), ext.PartitionId, ext.ExtentId)
		return
	}
	p := NewPacketToDeleteEcExtent(ctx, dp, ext)
	if err = p.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		err = errors.NewErrorf("write to ecNode %s, %s", p.GetUniqueLogId(),
			err.Error())
		return
	}
	if err = p.ReadFromConn(conn, proto.ReadDeadlineTime); err != nil {
		err = errors.NewErrorf("read response from ecNode %s, %s",
			p.GetUniqueLogId(), err.Error())
		return
	}
	if p.ResultCode != proto.OpOk {
		if p.ResultCode == proto.OpTryOtherAddr {
			mp.topoManager.FetchDataPartitionView(mp.config.VolName, ext.PartitionId)
			log.LogDebugf("doDeleteEcMarkedInodes vol(%s) ecDataPartition(%v) not exist in host(%v)," +
				" force fetch data partition view", mp.config.VolName, ext.PartitionId, dp.EcHosts[0])
		}
		err = errors.NewErrorf("[deleteEcMarkedInodes] %s response: %s", p.GetUniqueLogId(),
			p.GetResultMsg())
	}
	return
}

func (mp *metaPartition) doBatchDeleteEcExtentsByPartition(ctx context.Context, dp *topology.DataPartition, exts []*proto.MetaDelExtentKey) (err error) {
	var (
		conn *net.TCPConn
	)
	conn, err = mp.config.ConnPool.GetConnect(dp.EcHosts[0])
	defer func() {
		if err != nil {
			mp.config.ConnPool.PutConnect(conn, ForceClosedConnect)
		} else {
			mp.config.ConnPool.PutConnect(conn, NoClosedConnect)
		}
	}()

	if err != nil {
		err = errors.NewErrorf("get conn from pool %s, "+
			"extents partitionId=%d",
			err.Error(), dp.PartitionID)
		return
	}
	p := NewPacketToBatchDeleteEcExtent(ctx, dp, exts)
	if err = p.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		err = errors.NewErrorf("write to ecNode %s, %s", p.GetUniqueLogId(),
			err.Error())
		return
	}
	if err = p.ReadFromConn(conn, proto.ReadDeadlineTime*10); err != nil {
		err = errors.NewErrorf("read response from ecNode %s, %s",
			p.GetUniqueLogId(), err.Error())
		return
	}
	if p.ResultCode != proto.OpOk {
		err = errors.NewErrorf("[deleteEcMarkedInodes] %s response: %s", p.GetUniqueLogId(),
			p.GetResultMsg())
	}
	return
}
