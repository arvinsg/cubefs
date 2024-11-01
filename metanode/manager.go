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
	"encoding/json"
	"fmt"
	"github.com/cubefs/cubefs/util/multirate"
	"github.com/cubefs/cubefs/util/tokenmanager"
	"github.com/cubefs/cubefs/util/unit"
	"github.com/tiglabs/raft"
	"io/fs"
	"io/ioutil"
	"net"
	_ "net/http/pprof"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/util/exporter"

	"github.com/cubefs/cubefs/cmd/common"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/util/connpool"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/statistics"
)

const partitionPrefix = "partition_"
const ExpiredPartitionPrefix = "expired_"

// MetadataManager manages all the meta partitions.
type MetadataManager interface {
	Start() error
	FailOverLeaderMp()
	Stop()
	//CreatePartition(id string, start, end uint64, peers []proto.Peer) error
	HandleMetadataOperation(conn net.Conn, p *Packet, remoteAddr string) error
	GetPartition(id uint64) (MetaPartition, error)
	GetRecorder(id uint64) (*metaRecorder, error)
	Range(f func(i uint64, p MetaPartition) bool)
	RangeMonitorData(deal func(data *statistics.MonitorData, volName, diskPath string, pid uint64))
	StartPartition(id uint64) error
	StopPartition(id uint64) error
	ReloadPartition(id uint64) error
	ResetDumpSnapShotConfCount(confCount uint64)
	GetDumpSnapRunningCount() uint64
	GetDumpSnapMPID() []uint64
	GetStartFailedPartitions() []uint64
	MetaPartitionCount() int
}

// MetadataManagerConfig defines the configures in the metadata manager.
type MetadataManagerConfig struct {
	NodeID    uint64
	RootDir   string
	ZoneName  string
	RaftStore raftstore.RaftStore
}

type metadataManager struct {
	nodeId                uint64
	zoneName              string
	rootDir               string
	rocksDBDirs           []string
	raftStore             raftstore.RaftStore
	connPool              *connpool.ConnectPool
	state                 uint32
	mu                    sync.RWMutex
	createMu              sync.RWMutex
	partitions            map[uint64]MetaPartition 	// Key: metaRangeId, Val: metaPartition
	recorders             sync.Map 					// Key: metaRangeId, Val: metaRecorder
	startFailedPartitions sync.Map
	metaNode              *MetaNode
	flDeleteBatchCount    atomic.Value
	stopC                 chan bool
	tokenM                *tokenmanager.TokenManager
	noLeaderMpsMap        map[uint64]int64
}

type VolumeConfig struct {
	Name                              string
	PartitionCount                    int
	TrashDay                          int32
	ChildFileMaxCnt                   uint32
	TrashCleanInterval                uint64
	BatchDelInodeCnt                  uint32
	DelInodeInterval                  uint32
	EnableBitMapAllocator             bool
	CleanTrashItemMaxDurationEachTime int32
	CleanTrashItemMaxCountEachTime    int32
	CursorSkipStep                    uint64
	EnableRemoveDupReq                bool
	TruncateEKCount                   int
}

type MetaNodeVersion struct {
	Major int64
	Minor int64
	Patch int64
}


func (m *metadataManager) getPacketLabelVals(p *Packet) (labels []string) {
	labels = make([]string, 3)
	mp, err := m.getPartition(p.PartitionID)
	if err != nil {
		log.LogErrorf("[metaManager] getPacketLabels metric packet: %v, partitions: %v", p, m.partitions)
		return
	}

	labels[0] = mp.GetBaseConfig().VolName
	labels[1] = fmt.Sprintf("%d", p.PartitionID)
	labels[2] = p.GetOpMsg()

	return
}

func (m *metadataManager) statisticsOpTimeDelay(p *Packet, startTime time.Time, cost int)  {
	partition, err := m.GetPartition(p.PartitionID)
	if err != nil {
		return
	}
	mp, ok := partition.(*metaPartition)
	if !ok {
		return
	}

	var statisticsAction int
	switch p.Opcode {
	case proto.OpMetaCreateInode:
		statisticsAction = proto.ActionMetaOpCreateInode
	case proto.OpMetaEvictInode:
		statisticsAction = proto.ActionMetaOpEvictInode
	case proto.OpMetaCreateDentry:
		statisticsAction = proto.ActionMetaOpCreateDentry
	case proto.OpMetaDeleteDentry:
		statisticsAction = proto.ActionMetaOpDeleteDentry
	case proto.OpMetaLookup:
		statisticsAction = proto.ActionMetaOpLookup
	case proto.OpMetaReadDir:
		statisticsAction = proto.ActionMetaOpReadDir
	case proto.OpMetaInodeGet:
		statisticsAction = proto.ActionMetaOpInodeGet
	case proto.OpMetaBatchInodeGet:
		statisticsAction = proto.ActionMetaOpBatchInodeGet
	case proto.OpMetaExtentsAdd:
		statisticsAction = proto.ActionMetaOpExtentsAdd
	case proto.OpMetaExtentsList:
		statisticsAction = proto.ActionMetaOpExtentsList
	case proto.OpMetaTruncate:
		statisticsAction = proto.ActionMetaOpTruncate
	case proto.OpMetaExtentsInsert:
		statisticsAction = proto.ActionMetaOpExtentsInsert
	default:
		return
	}

	mp.monitorData[statisticsAction].SetCost(0, cost, startTime)
}

// HandleMetadataOperation handles the metadata operations.
func (m *metadataManager) HandleMetadataOperation(conn net.Conn, p *Packet, remoteAddr string) (err error) {
	start := time.Now()
	metric := exporter.NewModuleTPWithStart(p.GetOpMsg(), start)
	defer func() {
		cost := time.Since(start)
		metric.SetWithCost(int64(cost/time.Millisecond), err)
		m.statisticsOpTimeDelay(p, start, int(cost/time.Microsecond))
	}()

	if err = m.rateLimit(conn, p, remoteAddr); err != nil{
		err = fmt.Errorf("limit time out")
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		m.respondToClient(conn, p)
		return
	}

	switch p.Opcode {
	case proto.OpMetaCreateInode:
		err = m.opCreateInode(conn, p, remoteAddr)
	case proto.OpMetaLinkInode:
		err = m.opMetaLinkInode(conn, p, remoteAddr)
	case proto.OpMetaFreeInodesOnRaftFollower:
		err = m.opFreeInodeOnRaftFollower(conn, p, remoteAddr)
	case proto.OpMetaUnlinkInode:
		err = m.opMetaUnlinkInode(conn, p, remoteAddr)
	case proto.OpMetaBatchUnlinkInode:
		err = m.opMetaBatchUnlinkInode(conn, p, remoteAddr)
	case proto.OpMetaInodeGet:
		err = m.opMetaInodeGet(conn, p, remoteAddr, proto.OpInodeGetVersion1)
	case proto.OpMetaInodeGetV2:
		err = m.opMetaInodeGet(conn, p, remoteAddr, proto.OpInodeGetVersion2)
	case proto.OpMetaEvictInode:
		err = m.opMetaEvictInode(conn, p, remoteAddr)
	case proto.OpMetaBatchEvictInode:
		err = m.opBatchMetaEvictInode(conn, p, remoteAddr)
	case proto.OpMetaSetattr:
		err = m.opSetAttr(conn, p, remoteAddr)
	case proto.OpMetaCreateDentry:
		err = m.opCreateDentry(conn, p, remoteAddr)
	case proto.OpMetaDeleteDentry:
		err = m.opDeleteDentry(conn, p, remoteAddr)
	case proto.OpMetaBatchDeleteDentry:
		err = m.opBatchDeleteDentry(conn, p, remoteAddr)
	case proto.OpMetaUpdateDentry:
		err = m.opUpdateDentry(conn, p, remoteAddr)
	case proto.OpMetaReadDir:
		err = m.opReadDir(conn, p, remoteAddr)
	case proto.OpCreateMetaPartition:
		err = m.opCreateMetaPartition(conn, p, remoteAddr)
	case proto.OpMetaNodeHeartbeat:
		err = m.opMasterHeartbeat(conn, p, remoteAddr)
	case proto.OpMetaExtentsAdd:
		err = m.opMetaExtentsAdd(conn, p, remoteAddr)
	case proto.OpMetaExtentsInsert:
		err = m.opMetaExtentsInsert(conn, p, remoteAddr)
	case proto.OpMetaExtentsList:
		err = m.opMetaExtentsList(conn, p, remoteAddr)
	case proto.OpMetaExtentsDel:
		err = m.opMetaExtentsDel(conn, p, remoteAddr)
	case proto.OpMetaTruncate:
		err = m.opMetaExtentsTruncate(conn, p, remoteAddr)
	case proto.OpMetaLookup:
		err = m.opMetaLookup(conn, p, remoteAddr)
	case proto.OpDeleteMetaPartition:
		err = m.opExpiredMetaPartition(conn, p, remoteAddr)
	case proto.OpUpdateMetaPartition:
		err = m.opUpdateMetaPartition(conn, p, remoteAddr)
	case proto.OpLoadMetaPartition:
		err = m.opLoadMetaPartition(conn, p, remoteAddr)
	case proto.OpDecommissionMetaPartition:
		err = m.opDecommissionMetaPartition(conn, p, remoteAddr)
	case proto.OpAddMetaPartitionRaftMember:
		err = m.opAddMetaPartitionRaftMember(conn, p, remoteAddr)
	case proto.OpRemoveMetaPartitionRaftMember:
		err = m.opRemoveMetaPartitionRaftMember(conn, p, remoteAddr)
	case proto.OpAddMetaPartitionRaftLearner:
		err = m.opAddMetaPartitionRaftLearner(conn, p, remoteAddr)
	case proto.OpPromoteMetaPartitionRaftLearner:
		err = m.opPromoteMetaPartitionRaftLearner(conn, p, remoteAddr)
	case proto.OpResetMetaPartitionRaftMember:
		err = m.opResetMetaPartitionMember(conn, p, remoteAddr)
	case proto.OpCreateMetaRecorder:
		err = m.opCreateMetaRecorder(conn, p, remoteAddr)
	case proto.OpDeleteMetaRecorder:
		err = m.opExpiredMetaRecorder(conn, p, remoteAddr)
	case proto.OpAddMetaPartitionRaftRecorder:
		err = m.opAddMetaPartitionRaftRecorder(conn, p, remoteAddr)
	case proto.OpRemoveMetaPartitionRaftRecorder:
		err = m.opRemoveMetaPartitionRaftRecorder(conn, p, remoteAddr)
	case proto.OpResetMetaRecorderRaftMember:
		err = m.opResetMetaRecorderRaftMember(conn, p, remoteAddr)
	case proto.OpMetaPartitionTryToLeader:
		err = m.opMetaPartitionTryToLeader(conn, p, remoteAddr)
	case proto.OpMetaBatchInodeGet:
		err = m.opMetaBatchInodeGet(conn, p, remoteAddr)
	case proto.OpMetaDeleteInode:
		err = m.opMetaDeleteInode(conn, p, remoteAddr)
	case proto.OpMetaCursorReset:
		//err = m.opMetaCursorReset(conn, p, remoteAddr)
	case proto.OpMetaBatchDeleteInode:
		err = m.opMetaBatchDeleteInode(conn, p, remoteAddr)
	case proto.OpMetaBatchExtentsAdd:
		err = m.opMetaBatchExtentsAdd(conn, p, remoteAddr)
	// operations for extend attributes
	case proto.OpMetaSetXAttr:
		err = m.opMetaSetXAttr(conn, p, remoteAddr)
	case proto.OpMetaGetXAttr:
		err = m.opMetaGetXAttr(conn, p, remoteAddr)
	case proto.OpMetaBatchGetXAttr:
		err = m.opMetaBatchGetXAttr(conn, p, remoteAddr)
	case proto.OpMetaRemoveXAttr:
		err = m.opMetaRemoveXAttr(conn, p, remoteAddr)
	case proto.OpMetaListXAttr:
		err = m.opMetaListXAttr(conn, p, remoteAddr)
	// operations for multipart session
	case proto.OpCreateMultipart:
		err = m.opCreateMultipart(conn, p, remoteAddr)
	case proto.OpListMultiparts:
		err = m.opListMultipart(conn, p, remoteAddr)
	case proto.OpRemoveMultipart:
		err = m.opRemoveMultipart(conn, p, remoteAddr)
	case proto.OpAddMultipartPart:
		err = m.opAppendMultipart(conn, p, remoteAddr)
	case proto.OpGetMultipart:
		err = m.opGetMultipart(conn, p, remoteAddr)
	case proto.OpMetaGetAppliedID:
		err = m.opGetAppliedID(conn, p, remoteAddr)
	case proto.OpMetaGetTruncateIndex:
		err = m.opGetTruncateIndex(conn, p, remoteAddr)
	case proto.OpGetMetaNodeVersionInfo:
		err = m.opGetMetaNodeVersionInfo(conn, p, remoteAddr)
	case proto.OpMetaGetCmpInode:
		err = m.opGetCompactInodesInfo(conn, p, remoteAddr)
	case proto.OpMetaInodeMergeEks:
		err = m.opInodeMergeExtents(conn, p, remoteAddr)
	case proto.OpMetaFileMigMergeEks:
		err = m.opFileMigMergeExtents(conn, p, remoteAddr)
	case proto.OpMetaReadDeletedDir:
		err = m.opReadDeletedDir(conn, p, remoteAddr)
	case proto.OpMetaLookupForDeleted:
		err = m.opMetaLookupDeleted(conn, p, remoteAddr)
	case proto.OpMetaGetDeletedInode:
		err = m.opMetaGetDeletedInode(conn, p, remoteAddr)
	case proto.OpMetaBatchGetDeletedInode:
		err = m.opMetaBatchGetDeletedInode(conn, p, remoteAddr)
	case proto.OpMetaRecoverDeletedDentry:
		err = m.opRecoverDeletedDentry(conn, p, remoteAddr)
	case proto.OpMetaBatchRecoverDeletedDentry:
		err = m.opBatchRecoverDeletedDentry(conn, p, remoteAddr)
	case proto.OpMetaRecoverDeletedInode:
		err = m.opRecoverDeletedINode(conn, p, remoteAddr)
	case proto.OpMetaBatchRecoverDeletedInode:
		err = m.opBatchRecoverDeletedINode(conn, p, remoteAddr)
	case proto.OpMetaCleanDeletedDentry:
		err = m.opCleanDeletedDentry(conn, p, remoteAddr)
	case proto.OpMetaBatchCleanDeletedDentry:
		err = m.opBatchCleanDeletedDentry(conn, p, remoteAddr)
	case proto.OpMetaCleanDeletedInode:
		err = m.opCleanDeletedINode(conn, p, remoteAddr)
	case proto.OpMetaBatchCleanDeletedInode:
		err = m.opBatchCleanDeletedINode(conn, p, remoteAddr)
	case proto.OpMetaStatDeletedFileInfo:
		err = m.opStatDeletedFileInfo(conn, p, remoteAddr)
	case proto.OpMetaGetExtentsNoModifyAccessTime:
		err = m.opGetExtentsNoModifyAccessTime(conn, p, remoteAddr)
	default:
		err = fmt.Errorf("%s unknown Opcode: %d, reqId: %d", remoteAddr,
			p.Opcode, p.GetReqID())
		p.PacketErrorWithBody(proto.OpNotPerm, []byte(err.Error()))
		m.respondToClient(conn, p)
	}
	if err != nil {
		err = errors.NewErrorf("%s [%s] req: %d - %s", remoteAddr, p.GetOpMsg(),
			p.GetReqID(), err.Error())
	}
	return
}

// Start starts the metadata manager.
func (m *metadataManager) Start() (err error) {
	if atomic.CompareAndSwapUint32(&m.state, common.StateStandby, common.StateStart) {
		defer func() {
			var newState uint32
			if err != nil {
				newState = common.StateStandby
			} else {
				newState = common.StateRunning
			}
			atomic.StoreUint32(&m.state, newState)
		}()
		err = m.onStart()
	}
	return
}

func (m *metadataManager) dealFailOverLeaderMp(mpIdArr []uint64) (leaderCnt uint64) {
	FailOverCh := make(chan uint64, defParallelFailOverCnt)
	var wg sync.WaitGroup
	leaderCnt = 0
	log.LogErrorf("fail over need check mp count:%d", len(mpIdArr))
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, pid := range mpIdArr {
			partition, err := m.getPartition(pid)
			if err != nil {
				continue
			}
			if _, ok := partition.IsLeader(); ok {
				FailOverCh <- pid
				//partition.(*metaPartition).tryToGiveUpLeader()
				atomic.AddUint64(&leaderCnt, 1)
			}
		}
		close(FailOverCh)
		log.LogErrorf("fail over lear mp count:%d", leaderCnt)
	}()

	for i := 0; i < defParallelFailOverCnt; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				pid, ok := <-FailOverCh
				if !ok {
					return
				}
				partition, err := m.getPartition(pid)
				if err != nil {
					continue
				}
				if _, ok := partition.IsLeader(); ok {
					log.LogErrorf("fail over lear mp [%d]", pid)
					partition.(*metaPartition).tryToGiveUpLeader()
				}
			}

		}()
	}
	wg.Wait()
	return
}

func (m *metadataManager) FailOverLeaderMp() {
	mpIdArray := make([]uint64, 0)
	m.Range(func(pid uint64, p MetaPartition) bool {
		mpIdArray = append(mpIdArray, pid)
		return true
	})

	for i := 0; i < defTryFailOverCnt; i++ {
		leaderCnt := m.dealFailOverLeaderMp(mpIdArray)
		log.LogErrorf("fail over %d partitions", leaderCnt)
		if leaderCnt == 0 {
			break
		}
		time.Sleep(intervalFailOverLeader)
	}
}

// Stop stops the metadata manager.
func (m *metadataManager) Stop() {
	if atomic.CompareAndSwapUint32(&m.state, common.StateRunning, common.StateShutdown) {
		defer atomic.StoreUint32(&m.state, common.StateStopped)
		m.onStop()
	}
}

// onStart creates the connection pool and loads the partitions.
func (m *metadataManager) onStart() (err error) {
	go m.syncMetaPartitionsRocksDBWalLogScheduler()
	go m.cleanOldDeleteEKRecordFileSchedule()
	err = m.startPartitions()
	if err != nil {
		log.LogError(err.Error())
		return
	}
	err = m.startRecorders()
	if err != nil {
		log.LogError(err.Error())
		return
	}
	go m.doCheckNoLeaderPartitionsSchedule()
	return
}

func (m *metadataManager) MetaPartitionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.partitions)
}

func (m *metadataManager) syncMetaPartitionsRocksDBWalLog() {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("sync rocks db wal log panic, %v", r)
			exporter.WarningAppendKey(PanicBackGroundKey, "sync rocks db wal log panic")
		}
	}()
	pidArray := make([]uint64, 0)
	//get all pid
	m.Range(func(i uint64, p MetaPartition) bool {
		pidArray = append(pidArray, i)
		return true
	})

	//get mp
	for _, pid := range pidArray {
		//check exit?
		select {
		case <-m.stopC:
			return
		default:
		}
		//sync one mp
		mp, err := m.getPartition(pid)
		if err != nil || mp == nil {
			continue
		}
		confInfo := getGlobalConfNodeInfo()
		mp.(*metaPartition).db.CommitEmptyRecordToSyncWal(confInfo.rocksFlushWal)
		// default 2 second
		time.Sleep(intervalSyncRocksDbWalLog)
	}
}

func (m *metadataManager) syncMetaPartitionsRocksDBWalLogScheduler() {
	//default 30 min
	syncTimer := time.NewTimer(defSyncRocksDbWalLog)
	for {
		select {
		case <-syncTimer.C:
			confInfo := getGlobalConfNodeInfo()
			interval := time.Duration(confInfo.rocksFlushWalInterval) * time.Minute
			if interval < defSyncRocksDbWalLog {
				interval = defSyncRocksDbWalLog
			}
			startSync := time.Now()
			log.LogWarnf("start sync wal log")
			m.syncMetaPartitionsRocksDBWalLog()
			syncCost := time.Since(startSync)
			log.LogWarnf("stop sync wal log:%v", syncCost/time.Second)
			syncTimer.Reset(interval)
		case <-m.stopC:
			syncTimer.Stop()
			return
		}
	}
}

func (m *metadataManager) doCheckNoLeaderPartitionsSchedule() {
	ticker := time.NewTicker(time.Second*NoLeaderCheckIntervalSecond)
	for {
		select{
		case <- m.stopC:
			ticker.Stop()
			return
		case <- ticker.C:
			m.checkNoLeaderPartitions()
		}
	}
}

func (m *metadataManager) checkNoLeaderPartitions() {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("check no leader partitions panic, %v", r)
			exporter.WarningAppendKey(PanicBackGroundKey, "check no leader partitions panic")
		}
	}()
	noLeaderMPsID := make([]uint64, 0)
	ids := make([]uint64, 0, len(m.partitions))
	m.Range(func(i uint64, p MetaPartition) bool {
		ids = append(ids, i)
		return true
	})
	m.walkRecorders(func(recorder *metaRecorder) bool {
		ids = append(ids, recorder.partitionID)
		return true
	})
	for _, id := range ids {
		leader, _ := m.raftStore.RaftServer().LeaderTerm(id)
		if leader != raft.NoLeader {
			delete(m.noLeaderMpsMap, id)
			continue
		}

		lastNoLeaderTimestamp, ok := m.noLeaderMpsMap[id]
		if !ok {
			m.noLeaderMpsMap[id] = time.Now().Unix()
		} else if time.Now().Unix() - lastNoLeaderTimestamp >= 6*NoLeaderCheckIntervalSecond {
			noLeaderMPsID = append(noLeaderMPsID, id)
		}
	}
	if len(noLeaderMPsID) == 0 {
		return
	}
	warningMsg := fmt.Sprintf("some meta partition no leader more than 1min, no leader meta partitions: %v",
		noLeaderMPsID)
	exporter.WarningAppendKey(PartitionNoLeader, warningMsg)
}

func (m *metadataManager) cleanOldDeleteEKRecordFileSchedule() {
	timer := time.NewTicker(cleanDelEKRecordFileTimerInterval)
	for {
		select {
		case <- m.stopC:
			timer.Stop()
			return
		case <- timer.C:
			m.doCleanOldDeleteEKRecordFile()
		}
	}
}

func (m *metadataManager) doCleanOldDeleteEKRecordFile() {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("clean del ek record file panic, %v", r)
			exporter.WarningAppendKey(PanicBackGroundKey, "clean del ek record file panic")
		}
	}()
	mpIDs := make([]uint64, 0)
	m.Range(func(i uint64, p MetaPartition) bool {
		mpIDs = append(mpIDs, p.GetBaseConfig().PartitionId)
		return true
	})

	expiredTime := time.Now().Add(-delEKRecordFileRetentionTime)
	for _, mpID := range mpIDs {
		partition, err := m.getPartition(mpID)
		if err != nil{
			continue
		}

		mp, ok := partition.(*metaPartition)
		if !ok {
			continue
		}

		mp.removeOldDeleteEKRecordFileByTime(delExtentKeyList, prefixDelExtentKeyListBackup, expiredTime)
		mp.removeOldDeleteEKRecordFileByTime(InodeDelExtentKeyList, PrefixInodeDelExtentKeyListBackup, expiredTime)
	}

	var metaDataDiskUsedRatio float64
	if metaDataDisk, ok := m.metaNode.disks[m.metaNode.metadataDir]; ok {
		metaDataDiskUsedRatio = metaDataDisk.Used/metaDataDisk.Total
	}
	if metaDataDiskUsedRatio < ForceCleanDelEKRecordFileMaxMetaDataDiskUsedFactor {
		log.LogDebugf("[removeOldDeleteEKRecordFile] meta data disk used ratio:%v", metaDataDiskUsedRatio)
		return
	}

	recordFileMaxTotalSize := uint64(defForceDeleteEKRecordFileMaxTotalSize)
	for _, mpID := range mpIDs {
		partition, err := m.getPartition(mpID)
		if err != nil{
			continue
		}

		mp, ok := partition.(*metaPartition)
		if !ok {
			continue
		}

		mp.removeOldDeleteEKRecordFile(delExtentKeyList, prefixDelExtentKeyListBackup, recordFileMaxTotalSize, true)
		mp.removeOldDeleteEKRecordFile(InodeDelExtentKeyList, PrefixInodeDelExtentKeyListBackup, recordFileMaxTotalSize, true)
	}
}

// onStop stops each meta partitions.
func (m *metadataManager) onStop() {
	var wg sync.WaitGroup
	start := time.Now()
	if m.partitions != nil {
		m.Range(func(i uint64, p MetaPartition) bool {
			wg.Add(1)
			go func(partition MetaPartition) {
				defer wg.Done()
				partition.Stop()
			}(p)
			return true
		})
	}
	m.walkRecorders(func(mr *metaRecorder) bool {
		wg.Add(1)
		go func(recorder *metaRecorder) {
			defer wg.Done()
			recorder.Stop()
		}(mr)
		return true
	})
	wg.Wait()
	log.LogErrorf("stop partitions and recorders cost:%v", time.Since(start))

	close(m.stopC)
	return
}

// LoadMetaPartition returns the meta partition with the specified volName.
func (m *metadataManager) getPartition(id uint64) (mp MetaPartition, err error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mp, ok := m.partitions[id]
	if ok {
		return
	}
	err = errors.New(fmt.Sprintf("%s: %d", proto.ErrUnknownMetaPartition.Error(), id))
	return
}

func (m *metadataManager) loadPartitions(metaNodeInfo *proto.MetaNodeInfo, fileInfoList []fs.FileInfo) {
	if len(metaNodeInfo.PersistenceMetaPartitions) == 0 {
		log.LogWarnf("loadPartitions: length of PersistenceMetaPartitions is 0, ExpiredPartition check without effect")
	}

	var wg sync.WaitGroup
	for _, fileInfo := range fileInfoList {
		if fileInfo.IsDir() && strings.HasPrefix(fileInfo.Name(), partitionPrefix) {

			if isExpiredPartition(fileInfo.Name(), metaNodeInfo.PersistenceMetaPartitions, partitionPrefix) {
				log.LogErrorf("loadPartitions: find expired partition[%s], rename it and you can delete him manually",
					fileInfo.Name())
				oldName := path.Join(m.rootDir, fileInfo.Name())
				newName := path.Join(m.rootDir, ExpiredPartitionPrefix+fileInfo.Name()+"_"+strconv.FormatInt(time.Now().Unix(), 10))
				if tempErr := os.Rename(oldName, newName); tempErr != nil {
					log.LogErrorf("rename file has err:[%s]", tempErr.Error())
				}

				if len(fileInfo.Name()) > 10 && strings.HasPrefix(fileInfo.Name(), partitionPrefix) {
					log.LogErrorf("loadPartitions: find expired partition[%s], rename raft file",
						fileInfo.Name())
					partitionId := fileInfo.Name()[len(partitionPrefix):]
					oldRaftName := path.Join(m.metaNode.raftDir, partitionId)
					newRaftName := path.Join(m.metaNode.raftDir, ExpiredPartitionPrefix+partitionId+"_"+strconv.FormatInt(time.Now().Unix(), 10))
					log.LogErrorf("loadPartitions: find expired try rename raft file [%s] -> [%s]", oldRaftName, newRaftName)
					if _, tempErr := os.Stat(oldRaftName); tempErr != nil {
						log.LogWarnf("stat file [%s] has err:[%s]", oldRaftName, tempErr.Error())
					} else {
						if tempErr := os.Rename(oldRaftName, newRaftName); tempErr != nil {
							log.LogErrorf("rename file has err:[%s]", tempErr.Error())
						}
					}
				}

				continue
			}

			wg.Add(1)
			go func(fileName string) {
				var loadErr error
				defer func() {
					if r := recover(); r != nil {
						log.LogErrorf("loadPartitions partition: %s, "+
							"error: %s, failed: %v", fileName, loadErr, r)
						log.LogFlush()
						panic(r)
					}
					if loadErr != nil {
						log.LogErrorf("loadPartitions partition: %s, "+
							"error: %s", fileName, loadErr)
						log.LogFlush()
						panic(loadErr)
					}
				}()
				defer wg.Done()
				if len(fileName) < 10 {
					log.LogWarnf("ignore unknown partition dir: %s", fileName)
					return
				}
				var id uint64
				partitionId := fileName[len(partitionPrefix):]
				id, loadErr = strconv.ParseUint(partitionId, 10, 64)
				if loadErr != nil {
					log.LogWarnf("ignore path: %s,not partition", partitionId)
					return
				}

				partitionConfig := &MetaPartitionConfig{
					NodeId:             m.nodeId,
					RaftStore:          m.raftStore,
					RootDir:            path.Join(m.rootDir, fileName),
					ConnPool:           m.connPool,
					TrashRemainingDays: -1,
				}
				partitionConfig.AfterStop = func() {
					m.detachPartition(id)
				}
				// check snapshot dir or backup
				snapshotDir := path.Join(partitionConfig.RootDir, snapshotDir)
				if _, loadErr = os.Stat(snapshotDir); loadErr != nil {
					backupDir := path.Join(partitionConfig.RootDir, snapshotBackup)
					if _, loadErr = os.Stat(backupDir); loadErr == nil {
						if loadErr = os.Rename(backupDir, snapshotDir); loadErr != nil {
							loadErr = errors.Trace(loadErr,
								fmt.Sprintf(": fail recover backup snapshot %s",
									snapshotDir))
							return
						}
					}
					loadErr = nil
				}
				var partition MetaPartition
				if partition, loadErr = LoadMetaPartition(partitionConfig, m); loadErr != nil {
					log.LogErrorf("load partition id=%d failed: %s.",
						id, loadErr.Error())
					return
				}
				m.attachPartition(id, partition)
			}(fileInfo.Name())
		}
	}
	wg.Wait()
	return
}

func (m *metadataManager) loadRecorders(metaNodeInfo *proto.MetaNodeInfo, fileInfoList []fs.FileInfo) {
	if len(metaNodeInfo.PersistenceMetaRecorders) == 0 {
		log.LogWarnf("loadRecorders: length of PersistenceMetaRecorders is 0, ExpiredRecorder check without effect")
	}

	for _, fileInfo := range fileInfoList {
		if fileInfo.IsDir() && strings.HasPrefix(fileInfo.Name(), RecorderPrefix) {
			if m.isExpiredRecorder(fileInfo.Name(), metaNodeInfo.PersistenceMetaRecorders, RecorderPrefix) {
				log.LogErrorf("loadRecorders: find expired recorder[%s], rename it and you can delete him manually",
					fileInfo.Name())
				m.expireRecorder(fileInfo.Name())
				continue
			}
			mr, loadErr := m.loadRecorder(fileInfo.Name())
			if loadErr != nil {
				log.LogErrorf("loadRecorders partition: %s, error: %s", fileInfo.Name(), loadErr)
				log.LogFlush()
				panic(loadErr)
			}
			m.attachRecorder(mr.partitionID, mr)
		}
	}
	return
}

func (m *metadataManager) addPartition(id uint64, partition MetaPartition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.partitions[id] = partition
}

func (m *metadataManager) attachPartition(id uint64, partition MetaPartition) {
	m.addPartition(id, partition)
	m.metaNode.addVolToFetchTopologyManager(partition.GetBaseConfig().VolName)
	return
}

func (m *metadataManager) startPartitions() (err error) {
	var wg sync.WaitGroup
	start := time.Now()
	failCnt := uint64(0)

	mpIds := m.partitionIDs()
	partitionIdC := make(chan uint64, defParallelismStartMPCount)
	wg.Add(1)
	go func(c chan<- uint64) {
		defer wg.Done()
		for _, mpId := range mpIds {
			c <- mpId
		}
		close(c)
	}(partitionIdC)

	for i := 0; i < defParallelismStartMPCount; i++ {
		wg.Add(1)
		go func(c <-chan uint64) {
			defer wg.Done()
			var partition MetaPartition
			for {
				mpId, ok := <-c
				if !ok || mpId == 0 {
					return
				}

				partition, err = m.getPartition(mpId)
				if err != nil {
					continue
				}
				if partition.GetBaseConfig().PartitionId != mpId {
					continue
				}
				if pErr := partition.Start(); pErr != nil {
					log.LogErrorf("partition[%v] start failed: %v", mpId, pErr)
					if strings.Contains(pErr.Error(), raft.ErrLackOfRaftLog.Error()) {
						m.startFailedPartitions.Store(mpId, true)
					} else {
						atomic.AddUint64(&failCnt, 1)
					}
					continue
				}
				log.LogInfof("partition[%v] start success", mpId)
			}
		}(partitionIdC)
	}
	wg.Wait()

	if failCnt != 0 {
		log.LogErrorf("start %d partitions failed", failCnt)
		return errors.NewErrorf("start %d partitions failed", failCnt)
	}

	ids := m.GetStartFailedPartitions()
	if len(ids) != 0 {
		exporter.WarningAppendKey(PartitionStartFailed, fmt.Sprintf("start failed meta partitions: %v", ids))
		log.LogErrorf("start failed partitions: %v", ids)
	}

	log.LogInfof("start %d partitions cost :%v", len(m.partitions), time.Since(start))
	return
}

func (m *metadataManager) delPartition(id uint64) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, has := m.partitions[id]
	if !has {
		err = fmt.Errorf("unknown partition: %d", id)
		return
	}
	delete(m.partitions, id)
	return
}

func (m *metadataManager) detachPartition(id uint64) (err error) {
	var partition MetaPartition
	partition, err = m.getPartition(id)
	if err != nil {
		err = fmt.Errorf("unknown partition: %d", id)
		return
	}
	if err = m.delPartition(id); err != nil {
		return
	}
	m.metaNode.delVolFromFetchTopologyManager(partition.GetBaseConfig().VolName)
	return
}

func (m *metadataManager) partitionIDs() (pids []uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pids = make([]uint64, 0, len(m.partitions))
	for id, _ := range m.partitions {
		pids = append(pids, id)
	}
	return
}

func (m *metadataManager) createPartition(request *proto.CreateMetaPartitionRequest) (err error) {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	if _, ok := m.startFailedPartitions.Load(request.PartitionID); ok {
		err = errors.NewErrorf("[createPartition]->partition %v exist in startFailedPartitions", request.PartitionID)
		return
	}

	partitionId := fmt.Sprintf("%d", request.PartitionID)

	mpc := &MetaPartitionConfig{
		PartitionId:        request.PartitionID,
		VolName:            request.VolName,
		Start:              request.Start,
		End:                request.End,
		Cursor:             request.Start,
		Peers:              request.Members,
		Learners:           request.Learners,
		Recorders: 			request.Recorders,
		RaftStore:          m.raftStore,
		NodeId:             m.nodeId,
		RootDir:            path.Join(m.rootDir, partitionPrefix+partitionId),
		ConnPool:           m.connPool,
		TrashRemainingDays: int32(request.TrashDays),
		StoreMode:          request.StoreMode,
		CreationType:       request.CreationType,
	}
	mpc.AfterStop = func() {
		m.detachPartition(request.PartitionID)
	}

	if mpc.StoreMode < proto.StoreModeMem || mpc.StoreMode > proto.StoreModeRocksDb {
		mpc.StoreMode = proto.StoreModeMem
	}

	var oldMp MetaPartition
	if oldMp, err = m.GetPartition(request.PartitionID); err == nil {
		err = oldMp.IsEquareCreateMetaPartitionRequst(request)
		return
	}
	if _, err = m.GetRecorder(request.PartitionID); err == nil {
		err = fmt.Errorf("meta recorder exists")
		return
	}

	var partition MetaPartition
	if partition, err = CreateMetaPartition(mpc, m); err != nil {
		err = errors.NewErrorf("[createPartition]->%s", err.Error())
		return
	}

	if err = partition.Start(); err != nil {
		os.RemoveAll(mpc.RootDir)
		log.LogErrorf("load meta partition %v fail: %v", request.PartitionID, err)
		err = errors.NewErrorf("[createPartition]->%s", err.Error())
		return
	}

	m.attachPartition(request.PartitionID, partition)
	log.LogInfof("load meta partition %v success", request.PartitionID)

	return
}

func (m *metadataManager) deletePartition(id uint64) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, has := m.partitions[id]
	if !has {
		return
	}
	mp.Reset()
	delete(m.partitions, id)
	return
}

func (m *metadataManager) expiredPartition(id uint64) (err error) {
	var mp MetaPartition
	if mp, err = m.getPartition(id); err != nil {
		return
	}

	if err = mp.Expired(); err != nil {
		return
	}
	_ = m.detachPartition(id)
	return
}

// Range scans all the meta partitions.
func (m *metadataManager) Range(f func(i uint64, p MetaPartition) bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for k, v := range m.partitions {
		if !f(k, v) {
			return
		}
	}
}

// GetPartition returns the meta partition with the given ID.
func (m *metadataManager) GetPartition(id uint64) (mp MetaPartition, err error) {
	mp, err = m.getPartition(id)
	return
}

// MarshalJSON only marshals the base information of every partition.
func (m *metadataManager) MarshalJSON() (data []byte, err error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return json.Marshal(m.partitions)
}

func (m *metadataManager) rateLimit(conn net.Conn, p *Packet, remoteAddr string) (error){
	// ignore rate limit if request is from cluster master nodes
	if p.isMasterRequest() {
		return nil
	}

	var ps = multirate.Properties{}
	ps = append(ps, multirate.Property{Type: multirate.PropertyTypeOp, Value: strconv.Itoa(int(p.Opcode))})

	pid := p.PartitionID
	mp, err := m.GetPartition(pid)
	if err == nil && mp != nil {
		vol := mp.GetBaseConfig().VolName
		ps = append(ps, multirate.Property{Type: multirate.PropertyTypeVol, Value: vol})
		ps = append(ps, multirate.Property{Type: multirate.PropertyTypePartition, Value: strconv.Itoa(int(pid))})
	}
	stat := multirate.Stat{
		Count: 1,
		InBytes: int(unit.PacketHeaderSize + p.ArgLen + p.Size),
	}
	return multirate.WaitNUseDefaultTimeout(context.Background(), ps, stat)
}

func (m *metadataManager) RangeMonitorData(deal func(data *statistics.MonitorData, volName, diskPath string, pid uint64)) {
	pids := m.partitionIDs()
	for _, pid := range pids {
		mp, err := m.getPartition(pid)
		if err != nil {
			continue
		}
		mp.RangeMonitorData(deal)
	}
	return
}

func (m *metadataManager) StartPartition(id uint64) (err error) {
	if _, err = m.getPartition(id); err == nil {
		err = fmt.Errorf("MP[%d] already start", id)
		return
	}

	err = nil
	fileName := fmt.Sprintf("%s%d", partitionPrefix, id)
	var mpDirInfo os.FileInfo
	partitionConfig := &MetaPartitionConfig{
		NodeId:             m.nodeId,
		RaftStore:          m.raftStore,
		RootDir:            path.Join(m.rootDir, fileName),
		ConnPool:           m.connPool,
		TrashRemainingDays: -1,
	}
	partitionConfig.AfterStop = func() {
		m.detachPartition(id)
	}

	if mpDirInfo, err = os.Stat(partitionConfig.RootDir); err != nil {
		err = fmt.Errorf("MP[%d] not exist", id)
		return
	}

	if !mpDirInfo.IsDir() {
		err = fmt.Errorf("MP[%d] root path is not dir", id)
		return
	}

	// check snapshot dir or backup
	snapDir := path.Join(partitionConfig.RootDir, snapshotDir)
	if _, err = os.Stat(snapDir); err != nil {
		backupDir := path.Join(partitionConfig.RootDir, snapshotBackup)
		if _, err = os.Stat(backupDir); err == nil {
			if err = os.Rename(backupDir, snapDir); err != nil {
				err = errors.Trace(err,
					fmt.Sprintf(": fail recover backup snapshot %s",
						snapshotDir))
				return
			}
		}
		err = nil
	}
	var partition MetaPartition
	if partition, err = LoadMetaPartition(partitionConfig, m); err != nil {
		log.LogErrorf("load partition id=%d failed: %s.",
			id, err.Error())
		return
	}
	partition.(*metaPartition).CreationType = proto.DecommissionedCreateMetaPartition
	m.attachPartition(id, partition)
	err = partition.Start()
	if err != nil {
		m.detachPartition(id)
		return
	}
	m.startFailedPartitions.Delete(id)
	return
}

func (m *metadataManager) StopPartition(id uint64) (err error) {
	if _, ok := m.startFailedPartitions.Load(id); ok {
		return
	}

	var partition MetaPartition
	if partition, err = m.getPartition(id); err != nil {
		err = fmt.Errorf("MP[%d] already stopped", id)
		return
	}

	partition.Stop()
	// after func will detach partition
	return
}

func (m *metadataManager) ReloadPartition(id uint64) (err error) {
	if err = m.StopPartition(id); err != nil {
		return
	}
	if err = m.StartPartition(id); err != nil {
		return
	}
	return
}

func (m *metadataManager) ResetDumpSnapShotConfCount(confCount uint64) {
	if m.tokenM == nil {
		return
	}

	if m.tokenM.GetConfCnt() != confCount {
		m.tokenM.ResetRunCnt(confCount)
	}

	return
}

func (m *metadataManager) GetDumpSnapRunningCount() uint64 {
	if m.tokenM == nil {
		return 0
	}

	return m.tokenM.GetRunningCnt()
}

func (m *metadataManager) GetDumpSnapMPID() []uint64 {
	if m.tokenM == nil {
		return nil
	}

	_, mpIds := m.tokenM.GetRunningIds()
	return mpIds
}

func (m *metadataManager) GetStartFailedPartitions() (ids []uint64) {
	ids = make([]uint64, 0)
	m.startFailedPartitions.Range(func(key, value interface{}) bool {
		ids = append(ids, key.(uint64))
		return true
	})
	return
}

func (m *metadataManager) expiredStartFailedPartition(mpID uint64) (err error) {
	//expired raft path
	var currentPath = path.Clean(path.Join(m.raftStore.RaftPath(), strconv.FormatUint(mpID, 10)))
	var newPath = path.Join(path.Dir(currentPath),
		ExpiredPartitionPrefix+path.Base(currentPath)+"_"+strconv.FormatInt(time.Now().Unix(), 10))
	if err = os.Rename(currentPath, newPath); err != nil {
		log.LogErrorf("Expired: mark expired raft path fail: partitionID(%v) path(%v) newPath(%v) err(%v)",
			mpID, currentPath, newPath, err)
		return
	}
	log.LogInfof("ExpiredStartFailedPartition: mark expired raft path: partitionID(%v) path(%v) newPath(%v)",
		mpID, currentPath, newPath)

	//expired partition path
	currentPath = path.Clean(path.Join(m.rootDir, partitionPrefix+strconv.FormatUint(mpID, 10)))
	newPath = path.Join(path.Dir(currentPath),
		ExpiredPartitionPrefix+path.Base(currentPath)+"_"+strconv.FormatInt(time.Now().Unix(), 10))

	if err = os.Rename(currentPath, newPath); err != nil {
		log.LogErrorf("ExpiredPartition: mark expired partition fail: partitionID(%v) path(%v) newPath(%v) err(%v)",
			mpID, currentPath, newPath, err)
		return err
	}
	log.LogInfof("ExpiredPartition: mark expired partition: partitionID(%v) path(%v) newPath(%v)",
		mpID, currentPath, newPath)

	m.startFailedPartitions.Delete(mpID)
	return
}

func (m *metadataManager) loadMetaInfo() (metaNodeInfo *proto.MetaNodeInfo, fileInfoList []fs.FileInfo, err error) {
	for i := 0; i < 3; i++ {
		if metaNodeInfo, err = masterClient.NodeAPI().GetMetaNode(fmt.Sprintf("%s:%s", m.metaNode.localAddr,
			m.metaNode.listen)); err != nil {
			log.LogErrorf("loadPartitions: get MetaNode info fail: err(%v)", err)
			continue
		}
		break
	}

	// Check metadataDir directory
	var fileInfo os.FileInfo
	fileInfo, err = os.Stat(m.rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			err = os.MkdirAll(m.rootDir, 0755)
		} else {
			return
		}
	}
	if !fileInfo.IsDir() {
		err = errors.New("metadataDir must be directory")
		return
	}
	// scan the data directory
	fileInfoList, err = ioutil.ReadDir(m.rootDir)
	if err != nil {
		return
	}
	return
}

func (m *metadataManager) loadRecorder(fileName string) (mr *metaRecorder, err error) {
	var	id	uint64
	recorderIdStr := fileName[len(RecorderPrefix):]
	if id, err = strconv.ParseUint(recorderIdStr, 10, 64); err != nil {
		return
	}
	var cfg *raftstore.RecorderConfig
	if cfg, err = raftstore.LoadRecorderConfig(path.Join(m.rootDir, fileName), m.nodeId, m.raftStore); err != nil {
		return
	}
	if id != cfg.PartitionID {
		err = fmt.Errorf("mismatch recorder ID: dirName(%v) config(%v)", fileName, cfg)
		return
	}
	if mr, err = NewMetaRecorder(cfg, m); err != nil {
		return
	}
	return
}

func (m *metadataManager) startRecorders() error {
	// todo 并行？测试一下性能
	failCnt, successCnt := 0, 0
	start := time.Now()
	m.walkRecorders(func(mr *metaRecorder) bool {
		if err := mr.start(); err != nil {
			log.LogErrorf("recorder[%v] start failed: %v", mr.partitionID, err)
			failCnt++
		} else {
			successCnt++
		}
		return true
	})
	if failCnt != 0 {
		log.LogErrorf("start %d recorders failed", failCnt)
		return fmt.Errorf("start %d recorders failed", failCnt)
	}
	log.LogInfof("start %d recorders cost :%v", successCnt, time.Since(start))
	return nil
}

func (m *metadataManager) createRecorder(request *proto.CreateMetaRecorderRequest) (err error) {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	if _, ok := m.startFailedPartitions.Load(request.PartitionID); ok {
		err = errors.NewErrorf("[createRecorder]->partition %v exist in startFailedPartitions", request.PartitionID)
		return
	}

	if _, err = m.GetPartition(request.PartitionID); err == nil {
		err = fmt.Errorf("meta partition exists")
		return
	}
	if oldMr, getErr := m.GetRecorder(request.PartitionID); getErr == nil {
		err = oldMr.IsEqualCreateMetaRecorderRequest(request)
		return
	}

	var mr *metaRecorder
	cfg := &raftstore.RecorderConfig{
		VolName:        request.VolName,
		PartitionID:	request.PartitionID,
		Peers:          request.Members,
		Learners:       request.Learners,
		Recorders: 		request.Recorders,
		NodeID:         m.nodeId,
		RaftStore:      m.raftStore,
	}
	if mr, err = NewMetaRecorder(cfg, m); err != nil {
		err = errors.NewErrorf("[newRecorder]->%s", err.Error())
		return
	}
	if err = mr.persist(); err != nil {
		err = errors.NewErrorf("[persistMetadata]->%s", err.Error())
		return
	}
	if err = mr.start(); err != nil {
		os.RemoveAll(mr.Recorder().MetaPath())
		log.LogErrorf("start recorder %v fail: %v", request.PartitionID, err)
		err = errors.NewErrorf("[startRecorder]->%s", err.Error())
		return
	}

	m.attachRecorder(request.PartitionID, mr)
	log.LogInfof("attach recorder %v success", request.PartitionID)

	return
}

func (m *metadataManager) expiredRecorder(id uint64) (err error) {
	var mr *metaRecorder
	if mr, err = m.GetRecorder(id); err != nil {
		return
	}

	if err = mr.Expired(); err != nil {
		return
	}
	_ = m.detachRecorder(id)
	return
}

func (m *metadataManager) GetRecorder(id uint64) (mr *metaRecorder, err error) {
	value, ok := m.recorders.Load(id)
	if ok {
		mr = value.(*metaRecorder)
		return
	}
	err = errors.New(fmt.Sprintf("unknown meta recorder: %d", id))
	return
}

func (m *metadataManager) attachRecorder(id uint64, recorder *metaRecorder) {
	m.recorders.Store(id, recorder)
	return
}

func (m *metadataManager) detachRecorder(id uint64) (err error) {
	_, loaded := m.recorders.LoadAndDelete(id)
	if !loaded {
		err = fmt.Errorf("unknown recorder: %d", id)
		return
	}
	return
}

func (m *metadataManager) walkRecorders(visitor func(recorder *metaRecorder) bool) {
	if visitor == nil {
		return
	}
	m.recorders.Range(func(key, value interface{}) bool {
		mr, ok := value.(*metaRecorder)
		if !ok {
			return true
		}
		if !visitor(mr) {
			return false
		}
		return true
	})
}

func (m *metadataManager) isExpiredRecorder(fileName string, partitions []uint64, recorderPrefix string) (expiredPartition bool) {
	return isExpiredPartition(fileName, partitions, recorderPrefix)
}

func (m *metadataManager) expireRecorder(fileName string)  {
	oldName := path.Join(m.rootDir, fileName)
	newName := path.Join(m.rootDir, ExpiredRecorderPrefix+fileName+"_"+strconv.FormatInt(time.Now().Unix(), 10))
	if tempErr := os.Rename(oldName, newName); tempErr != nil {
		log.LogErrorf("rename file has err:[%s]", tempErr.Error())
	}
	log.LogErrorf("find expired recorder[%s], rename raft file", fileName)
	partitionId := fileName[len(RecorderPrefix):]
	oldRaftName := path.Join(m.metaNode.raftDir, partitionId)
	newRaftName := path.Join(m.metaNode.raftDir, ExpiredRecorderPrefix+partitionId+"_"+strconv.FormatInt(time.Now().Unix(), 10))
	log.LogErrorf("loadRecorders: find expired try rename raft file [%s] -> [%s]", oldRaftName, newRaftName)
	if _, tempErr := os.Stat(oldRaftName); tempErr != nil {
		log.LogWarnf("stat file [%s] has err:[%s]", oldRaftName, tempErr.Error())
	} else {
		if tempErr := os.Rename(oldRaftName, newRaftName); tempErr != nil {
			log.LogErrorf("rename file has err:[%s]", tempErr.Error())
		}
	}
}

// NewMetadataManager returns a new metadata manager.
func NewMetadataManager(conf MetadataManagerConfig, metaNode *MetaNode) (MetadataManager, error) {
	mm := &metadataManager{
		nodeId:         conf.NodeID,
		zoneName:       conf.ZoneName,
		rootDir:        conf.RootDir,
		raftStore:      conf.RaftStore,
		partitions:     make(map[uint64]MetaPartition),
		metaNode:       metaNode,
		connPool:       connpool.NewConnectPool(),
		stopC:          make(chan bool, 0),
		rocksDBDirs:    metaNode.rocksDirs,
		tokenM:         tokenmanager.NewTokenManager(10),
		noLeaderMpsMap: make(map[uint64]int64),
	}

	var (
		metaNodeInfo	*proto.MetaNodeInfo
		fileInfoList	[]fs.FileInfo
		err				error
	)
	if metaNodeInfo, fileInfoList, err = mm.loadMetaInfo(); err != nil {
		return nil, err
	}
	mm.loadPartitions(metaNodeInfo, fileInfoList)
	mm.loadRecorders(metaNodeInfo, fileInfoList)
	return mm, nil
}

// isExpiredPartition return whether one partition is expired
// if one partition does not exist in master, we decided that it is one expired partition
func isExpiredPartition(fileName string, partitions []uint64, partitionPrefix string) (expiredPartition bool) {
	if len(partitions) == 0 {
		return true
	}

	partitionId := fileName[len(partitionPrefix):]
	id, err := strconv.ParseUint(partitionId, 10, 64)
	if err != nil {
		log.LogWarnf("isExpiredPartition: %s, check error [%v], skip this check", partitionId, err)
		return true
	}

	for _, existId := range partitions {
		if existId == id {
			return false
		}
	}
	return true
}

func NewMetaNodeVersion(version string) *MetaNodeVersion {
	ver := &MetaNodeVersion{2, 5, 0}
	dotParts := strings.SplitN(version, ".", 3)
	if len(dotParts) != 3 {
		log.LogErrorf("[version: %s]'s length is  not right! ", version)
	}
	parsed := make([]int64, 3, 3)
	if len(dotParts) < 3 {
		return ver
	}
	for i, v := range dotParts[:3] {
		val, err := strconv.ParseInt(v, 10, 64)
		parsed[i] = val
		if err != nil {
			return ver
		}
	}
	ver.Major = parsed[0]
	ver.Minor = parsed[1]
	ver.Patch = parsed[2]
	return ver
}

func (v MetaNodeVersion) Compare(versionB *MetaNodeVersion) int {
	verA := []int64{v.Major, v.Minor, v.Patch}
	verB := []int64{versionB.Major, versionB.Minor, v.Patch}
	return recursiveCompare(verA, verB)
}

func recursiveCompare(versionA []int64, versionB []int64) int {
	if len(versionA) == 0 {
		return 0
	}
	a := versionA[0]
	b := versionB[0]
	if a > b {
		return 1
	} else if a < b {
		return -1
	}
	return recursiveCompare(versionA[1:], versionB[1:])
}

// LessThan: compare metaNodeVersion, return true if A < B.
func (v MetaNodeVersion) LessThan(versionB *MetaNodeVersion) bool {
	return v.Compare(versionB) < 0
}

func (v MetaNodeVersion) GreaterThan(versionB *MetaNodeVersion) bool {
	return v.Compare(versionB) > 0
}

func (v MetaNodeVersion) GreaterOrEqual(versionB *MetaNodeVersion) bool {
	return v.Compare(versionB) >= 0
}

func (v MetaNodeVersion) VersionStr() string {
	return fmt.Sprintf("%v.%v.%v", v.Major, v.Minor, v.Patch)
}
