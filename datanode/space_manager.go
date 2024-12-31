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

package datanode

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubefs/cubefs/util/topology"
	atomic2 "go.uber.org/atomic"

	"github.com/cubefs/cubefs/util/async"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
	"github.com/tiglabs/raft/util"
)

// SpaceManager manages the disk space.
type SpaceManager struct {
	clusterID            string
	disks                map[string]*Disk
	partitions           map[uint64]*DataPartition
	raftStore            raftstore.RaftStore
	nodeID               uint64
	diskMutex            sync.RWMutex
	partitionMutex       sync.RWMutex
	stats                *Stats
	stopC                chan bool
	selectedIndex        int // TODO what is selected index
	diskList             []string
	dataNode             *DataNode
	createPartitionMutex sync.RWMutex // 该锁用于控制Partition创建的并发，保证同一时间只处理一个Partition的创建操作

	// Parallel task limits on disk
	fixTinyDeleteRecordLimitOnDisk uint64
	normalExtentDeleteExpireTime   uint64
	flushFDIntervalSec             uint32
	flushFDParallelismOnDisk       uint64
	topoManager                    *topology.TopologyManager
	diskReservedRatio              *atomic2.Float64
}

// NewSpaceManager creates a new space manager.
func NewSpaceManager(dataNode *DataNode) *SpaceManager {
	var space = &SpaceManager{
		disks:                          make(map[string]*Disk),
		diskList:                       make([]string, 0),
		partitions:                     make(map[uint64]*DataPartition),
		stats:                          NewStats(dataNode.zoneName),
		stopC:                          make(chan bool, 0),
		dataNode:                       dataNode,
		fixTinyDeleteRecordLimitOnDisk: DefaultFixTinyDeleteRecordLimitOnDisk,
		normalExtentDeleteExpireTime:   DefaultNormalExtentDeleteExpireTime,
		topoManager:                    dataNode.topoManager,
		diskReservedRatio:              atomic2.NewFloat64(DefaultDiskReservedRatio),
	}
	async.RunWorker(space.statUpdateScheduler, func(i interface{}) {
		log.LogCriticalf("SPCMGR: stat update scheduler occurred panic: %v\nCallstack:\n%v",
			i, string(debug.Stack()))
	})
	return space
}

func (manager *SpaceManager) AsyncLoadExtent() {

	var disks = manager.GetDisks()
	var wg = new(sync.WaitGroup)
	if log.IsInfoEnabled() {
		log.LogInfof("SPCMGR: lazy load start")
	}
	var start = time.Now()
	for _, disk := range disks {
		wg.Add(1)
		go func(disk *Disk) {
			defer wg.Done()
			disk.AsyncLoadExtent(DefaultLazyLoadParallelismPerDisk)
		}(disk)
	}
	wg.Wait()
	if log.IsInfoEnabled() {
		log.LogInfof("SPCMGR: lazy load complete, elapsed %v", time.Now().Sub(start))
	}
	gHasLoadDataPartition = true
}

func (manager *SpaceManager) Stop() {
	defer func() {
		recover()
	}()
	close(manager.stopC)
	// 并行关闭所有Partition并释放空间, 并行度为64
	const maxParallelism = 128
	var parallelism = int(math.Min(float64(maxParallelism), float64(len(manager.partitions))))
	wg := sync.WaitGroup{}
	partitionC := make(chan *DataPartition, parallelism)
	wg.Add(1)
	go func(c chan<- *DataPartition) {
		defer wg.Done()
		manager.WalkPartitions(func(partition *DataPartition) bool {
			c <- partition
			return true
		}) // WalkPartitions 方法内部采用局部读锁结构，既不会产生map线程安全问题由不会长时间占用锁.
		close(c)
	}(partitionC)

	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func(c <-chan *DataPartition) {
			defer wg.Done()
			var partition *DataPartition
			for {
				if partition = <-c; partition == nil {
					return
				}
				partition.Stop()
			}
		}(partitionC)
	}
	wg.Wait()
}

func (manager *SpaceManager) SetNodeID(nodeID uint64) {
	manager.nodeID = nodeID
}

func (manager *SpaceManager) GetNodeID() (nodeID uint64) {
	return manager.nodeID
}

func (manager *SpaceManager) SetClusterID(clusterID string) {
	manager.clusterID = clusterID
}

func (manager *SpaceManager) GetClusterID() (clusterID string) {
	return manager.clusterID
}

func (manager *SpaceManager) SetRaftStore(raftStore raftstore.RaftStore) {
	manager.raftStore = raftStore
}
func (manager *SpaceManager) GetRaftStore() (raftStore raftstore.RaftStore) {
	return manager.raftStore
}

func (manager *SpaceManager) GetPartitions() (partitions []*DataPartition) {
	manager.partitionMutex.RLock()
	partitions = make([]*DataPartition, 0, len(manager.partitions))
	for _, dp := range manager.partitions {
		partitions = append(partitions, dp)
	}
	manager.partitionMutex.RUnlock()
	return
}

func (manager *SpaceManager) WalkPartitions(visitor func(partition *DataPartition) bool) {
	if visitor == nil {
		return
	}
	for _, partition := range manager.GetPartitions() {
		if !visitor(partition) {
			break
		}
	}
}

func (manager *SpaceManager) GetDisks() (disks []*Disk) {
	manager.diskMutex.RLock()
	defer manager.diskMutex.RUnlock()
	disks = make([]*Disk, 0)
	for _, disk := range manager.disks {
		disks = append(disks, disk)
	}
	return
}

func (manager *SpaceManager) WalkDisks(visitor func(*Disk) bool) {
	if visitor == nil {
		return
	}
	for _, disk := range manager.GetDisks() {
		if !visitor(disk) {
			break
		}
	}
}

func (manager *SpaceManager) Stats() *Stats {
	return manager.stats
}

func (manager *SpaceManager) LoadDisk(path string, expired CheckExpired) (err error) {
	var (
		disk *Disk
	)
	if _, exists := manager.GetDisk(path); !exists {
		var config = &DiskConfig{
			GetReservedRatio:  manager.GetDiskReservedRatio,
			MaxErrCnt:         DefaultDiskMaxErr,
			MaxFDLimit:        DiskMaxFDLimit,
			ForceFDEvictRatio: DiskForceEvictFDRatio,

			FixTinyDeleteRecordLimit: manager.fixTinyDeleteRecordLimitOnDisk,
		}
		var startTime = time.Now()
		if disk, err = OpenDisk(path, config, manager, DiskLoadPartitionParallelism, manager.topoManager, expired); err != nil {
			return
		}
		manager.putDisk(disk)
		var count int
		disk.WalkPartitions(func(partition *DataPartition) bool {
			manager.AttachPartition(partition)
			count++
			return true
		})
		log.LogInfof("Disk %v: load compete: partitions=%v, elapsed=%v", path, count, time.Since(startTime))
		err = nil
	}
	return
}

func (manager *SpaceManager) StartPartitions() {
	var err error
	partitions := make([]*DataPartition, 0)
	manager.partitionMutex.RLock()
	for _, partition := range manager.partitions {
		partitions = append(partitions, partition)
	}
	manager.partitionMutex.RUnlock()

	var (
		wg sync.WaitGroup
	)
	for _, dp := range partitions {
		wg.Add(1)
		go func(dp *DataPartition) {
			defer wg.Done()
			if err = dp.Start(); err != nil {
				manager.partitionMutex.Lock()
				delete(manager.partitions, dp.ID())
				manager.partitionMutex.Unlock()
				dp.Disk().DetachDataPartition(dp)
				msg := fmt.Sprintf("partition [id: %v, disk: %v] start failed: %v", dp.ID(), dp.Disk().Path, err)
				log.LogErrorf(msg)
				exporter.Warning(msg)
			}
		}(dp)
	}
	wg.Wait()
}

func (manager *SpaceManager) GetDisk(path string) (d *Disk, exists bool) {
	manager.diskMutex.RLock()
	defer manager.diskMutex.RUnlock()
	disk, has := manager.disks[path]
	if has && disk != nil {
		d = disk
		exists = true
		return
	}
	return
}

func (manager *SpaceManager) putDisk(d *Disk) {
	manager.diskMutex.Lock()
	manager.disks[d.Path] = d
	manager.diskList = append(manager.diskList, d.Path)
	manager.diskMutex.Unlock()

}

func (manager *SpaceManager) updateMetrics() {
	manager.diskMutex.RLock()
	var (
		total, used, available                                 uint64
		totalPartitionSize, remainingCapacityToCreatePartition uint64
		maxCapacityToCreatePartition, partitionCnt             uint64
	)
	maxCapacityToCreatePartition = 0
	for _, d := range manager.disks {
		total += d.Total
		used += d.Used
		available += d.Available
		totalPartitionSize += d.Allocated
		remainingCapacityToCreatePartition += d.Unallocated
		partitionCnt += uint64(d.PartitionCount())
		if maxCapacityToCreatePartition < d.Unallocated {
			maxCapacityToCreatePartition = d.Unallocated
		}
	}
	manager.diskMutex.RUnlock()
	log.LogDebugf("action[updateMetrics] total(%v) used(%v) available(%v) totalPartitionSize(%v)  remainingCapacityToCreatePartition(%v) "+
		"partitionCnt(%v) maxCapacityToCreatePartition(%v) ", total, used, available, totalPartitionSize, remainingCapacityToCreatePartition, partitionCnt, maxCapacityToCreatePartition)
	manager.stats.updateMetrics(total, used, available, totalPartitionSize,
		remainingCapacityToCreatePartition, maxCapacityToCreatePartition, partitionCnt)
}

func (manager *SpaceManager) minPartitionCnt() (d *Disk) {
	manager.diskMutex.Lock()
	defer manager.diskMutex.Unlock()
	var (
		minWeight     float64
		minWeightDisk *Disk
	)
	minWeight = math.MaxFloat64
	for _, disk := range manager.disks {
		if disk.Available <= 5*unit.GB || disk.Status != proto.ReadWrite {
			continue
		}
		diskWeight := disk.getSelectWeight()
		if diskWeight < minWeight {
			minWeight = diskWeight
			minWeightDisk = disk
		}
	}
	if minWeightDisk == nil {
		return
	}
	if minWeightDisk.Available <= 5*unit.GB || minWeightDisk.Status != proto.ReadWrite {
		return
	}
	d = minWeightDisk
	return d
}
func (manager *SpaceManager) statUpdateScheduler() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for {
			select {
			case <-ticker.C:
				manager.updateMetrics()
			case <-manager.stopC:
				ticker.Stop()
				return
			}
		}
	}()
}

func (manager *SpaceManager) Partition(partitionID uint64) (dp *DataPartition) {
	manager.partitionMutex.RLock()
	defer manager.partitionMutex.RUnlock()
	dp = manager.partitions[partitionID]

	return
}

func (manager *SpaceManager) AttachPartition(dp *DataPartition) {
	manager.partitionMutex.Lock()
	defer manager.partitionMutex.Unlock()
	manager.partitions[dp.ID()] = dp
}

// DetachDataPartition removes a data partition from the partition map.
func (manager *SpaceManager) DetachDataPartition(partitionID uint64) {
	manager.partitionMutex.Lock()
	defer manager.partitionMutex.Unlock()
	delete(manager.partitions, partitionID)
}

func (manager *SpaceManager) CreatePartition(request *proto.CreateDataPartitionRequest) (dp *DataPartition, err error) {
	// 保证同一时间只处理一个Partition的创建操作
	manager.createPartitionMutex.Lock()
	defer manager.createPartitionMutex.Unlock()
	dpCfg := &dataPartitionCfg{
		PartitionID:   request.PartitionId,
		VolName:       request.VolumeId,
		Peers:         request.Members,
		Hosts:         request.Hosts,
		Learners:      request.Learners,
		RaftStore:     manager.raftStore,
		NodeID:        manager.nodeID,
		ClusterID:     manager.clusterID,
		PartitionSize: request.PartitionSize,
		ReplicaNum:    request.ReplicaNum,

		VolHAType: request.VolumeHAType,
	}
	dp = manager.Partition(dpCfg.PartitionID)
	if dp != nil {
		if err = dp.IsEquareCreateDataPartitionRequst(request); err != nil {
			return nil, err
		}
		return
	}
	disk := manager.minPartitionCnt()
	if disk == nil {
		return nil, ErrNoSpaceToCreatePartition
	}
	if dp, err = disk.createPartition(dpCfg, request); err != nil {
		return
	}
	if dp.isReplLeader {
		dp.extentStore.MoveAllToAvailTinyExtentC(proto.TinyExtentCount)
	}
	disk.AttachDataPartition(dp)
	manager.AttachPartition(dp)
	if err = dp.Start(); err != nil {
		return
	}
	return
}

// DeletePartition deletes a partition based on the partition id.
func (manager *SpaceManager) DeletePartition(dpID uint64) {
	dp := manager.Partition(dpID)
	if dp == nil {
		return
	}
	manager.partitionMutex.Lock()
	delete(manager.partitions, dpID)
	manager.partitionMutex.Unlock()
	dp.Delete()
}

// ExpiredPartition marks specified partition as expired.
// It renames data path to a new name which add 'expired_' as prefix and operation timestamp as suffix.
// (e.g. '/disk0/datapartition_1_128849018880' to '/disk0/deleted_datapartition_1_128849018880_1600054521')
func (manager *SpaceManager) ExpiredPartition(partitionID uint64) {
	dp := manager.Partition(partitionID)
	if dp == nil {
		return
	}
	manager.partitionMutex.Lock()
	delete(manager.partitions, partitionID)
	manager.partitionMutex.Unlock()
	dp.Expired()
}

func (manager *SpaceManager) LoadPartition(d *Disk, partitionID uint64, partitionPath string, force bool) (err error) {
	var partition *DataPartition
	if err = d.RestoreOnePartition(partitionPath, force); err != nil {
		return
	}

	defer func() {
		if err != nil {
			manager.DetachDataPartition(partitionID)
			partition.Disk().DetachDataPartition(partition)
			msg := fmt.Sprintf("partition [id: %v, disk: %v] start failed: %v", partition.ID(), partition.Disk().Path, err)
			log.LogErrorf(msg)
			exporter.Warning(msg)
		}
	}()

	partition = manager.Partition(partitionID)
	if partition == nil {
		return fmt.Errorf("partition not exist")
	}

	if err = partition.ChangeCreateType(proto.DecommissionedCreateDataPartition); err != nil {
		return
	}

	// start raft
	if err = partition.Start(); err != nil {
		return
	}
	async.RunWorker(partition.ExtentStore().Load, func(i interface{}) {
		log.LogCriticalf("SPCMGR: DP %v: lazy load occurred panic: %v\nCallStack:\n%v",
			partition.ID(), i, string(debug.Stack()))
	})
	return
}

func (manager *SpaceManager) SyncPartitionReplicas(partitionID uint64, hosts []string) {
	dp := manager.Partition(partitionID)
	if dp == nil {
		return
	}
	dp.SyncReplicaHosts(hosts)
	return
}

// DeletePartition deletes a partition from cache based on the partition id.
func (manager *SpaceManager) DeletePartitionFromCache(dpID uint64) {
	dp := manager.Partition(dpID)
	if dp == nil {
		return
	}
	manager.partitionMutex.Lock()
	delete(manager.partitions, dpID)
	manager.partitionMutex.Unlock()
	dp.Stop()
	dp.Disk().DetachDataPartition(dp)
}

func (manager *SpaceManager) SetDiskFixTinyDeleteRecordLimit(newValue uint64) {
	if newValue > 0 && manager.fixTinyDeleteRecordLimitOnDisk != newValue {
		log.LogInfof("action[spaceManager] change DiskFixTinyDeleteRecordLimit from(%v) to(%v)", manager.fixTinyDeleteRecordLimitOnDisk, newValue)
		manager.fixTinyDeleteRecordLimitOnDisk = newValue
		manager.diskMutex.Lock()
		for _, disk := range manager.disks {
			disk.SetFixTinyDeleteRecordLimitOnDisk(newValue)
		}
		manager.diskMutex.Unlock()
	}
	return
}

func (manager *SpaceManager) SetForceFlushFDParallelismOnDisk(newValue uint64) {
	if newValue == 0 {
		newValue = DefaultForceFlushFDParallelismOnDisk
	}
	if newValue > 0 && manager.flushFDParallelismOnDisk != newValue {
		log.LogInfof("change ForceFlushFDParallelismOnDisk from %v  to %v", manager.flushFDParallelismOnDisk, newValue)
		manager.flushFDParallelismOnDisk = newValue
		manager.diskMutex.Lock()
		for _, disk := range manager.disks {
			disk.SetForceFlushFDParallelism(newValue)
		}
		manager.diskMutex.Unlock()
	}
}

func (manager *SpaceManager) SetPartitionConsistencyMode(mode proto.ConsistencyMode) {
	if !mode.Valid() {
		return
	}
	manager.partitionMutex.RLock()
	for _, partition := range manager.partitions {
		partition.SetConsistencyMode(mode)
	}
	manager.partitionMutex.RUnlock()
}

const (
	DefaultForceFlushFDSecond              = 10
	DefaultForceFlushFDParallelismOnDisk   = 5
	DefaultForceFlushDataSizeOnEachHDDDisk = 10 * util.MB
	DefaultForceFlushDataSizeOnEachSSDDisk = 50 * util.MB
	DefaultDeletionConcurrencyOnDisk       = 2
	DefaultIssueFixConcurrencyOnDisk       = 16
)

func (manager *SpaceManager) SetForceFlushFDInterval(newValue uint32) {
	if newValue == 0 {
		newValue = DefaultForceFlushFDSecond
	}
	if newValue > 0 && manager.flushFDIntervalSec != newValue {
		log.LogInfof("action[spaceManager] change ForceFlushFDInterval from(%v) to(%v)", manager.flushFDIntervalSec, newValue)
		manager.flushFDIntervalSec = newValue
	}
}

func (manager *SpaceManager) SetSyncWALOnUnstableEnableState(enableState bool) {
	if enableState == manager.raftStore.IsSyncWALOnUnstable() {
		return
	}
	log.LogInfof("action[spaceManager] change SyncWALOnUnstableEnableState from(%v) to(%v)", manager.raftStore.IsSyncWALOnUnstable(), enableState)
	manager.raftStore.SetSyncWALOnUnstable(enableState)
}
func (manager *SpaceManager) SetNormalExtentDeleteExpireTime(newValue uint64) {
	if newValue == 0 {
		newValue = DefaultNormalExtentDeleteExpireTime
	}
	if newValue > 0 && manager.normalExtentDeleteExpireTime != newValue {
		log.LogInfof("action[spaceManager] change normalExtentDeleteExpireTime from(%v) to(%v)", manager.normalExtentDeleteExpireTime, newValue)
		manager.normalExtentDeleteExpireTime = newValue
	}
}

func (manager *SpaceManager) SetDiskReservedRatio(ratio float64) {
	if ratio < proto.DataNodeDiskReservedMinRatio || ratio > proto.DataNodeDiskReservedMaxRatio {
		return
	}

	if current := manager.diskReservedRatio.Load(); ratio >= 0 && ratio != current {
		log.LogInfof("action[spaceManager] change diskReservedRatio from(%v) to(%v)", current, ratio)
		manager.diskReservedRatio.Store(ratio)
	}
}

func (manager *SpaceManager) GetDiskReservedRatio() float64 {
	return manager.diskReservedRatio.Load()
}

func (manager *SpaceManager) RestoreExpiredPartitions(all bool, ids map[uint64]bool) (failedDisks []string, failedDps, successDps []uint64) {
	var err error
	// Format: expired_datapartition_{ID}_{Capacity}_{Timestamp}
	// Regexp: ^expired_datapartition_(\d)+_(\d)+_(\d)+$
	regexpExpiredPartitionDirNameWithT := regexp.MustCompile("^expired_datapartition_(\\d)+_(\\d)+_(\\d)+$")
	// Format: expired_datapartition_{ID}_{Capacity}
	// Regexp: ^expired_datapartition_(\d)+_(\d)+$
	regexpExpiredPartitionDirName := regexp.MustCompile("^expired_datapartition_(\\d)+_(\\d)+$")
	reloadMap := make(map[uint64]struct {
		disk        *Disk
		delTime     uint64
		walPath     string
		newWalPath  string
		newFileName string
		fileName    string
		modTime     uint64
	}, 0)
	for _, d := range manager.disks {
		var entries []fs.DirEntry
		entries, err = os.ReadDir(d.Path)
		if err != nil {
			failedDisks = append(failedDisks, d.Path)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			filename := entry.Name()
			if !(regexpExpiredPartitionDirNameWithT.MatchString(filename) || regexpExpiredPartitionDirName.MatchString(filename)) {
				continue
			}
			var dpid uint64
			var delTime uint64
			parts := strings.Split(filename, "_")
			dpid, err = strconv.ParseUint(parts[2], 10, 64)
			if err != nil {
				failedDps = append(failedDps, dpid)
				continue
			}
			if !all {
				if _, ok := ids[dpid]; !ok {
					continue
				}
			}
			if len(parts) == 4 {
				delTime = uint64(0)
			} else {
				delTime, err = strconv.ParseUint(parts[4], 10, 64)
				if err != nil {
					failedDps = append(failedDps, dpid)
					continue
				}
			}
			var stat os.FileInfo
			stat, err = os.Stat(path.Join(d.Path, filename))
			if err != nil {
				failedDps = append(failedDps, dpid)
				continue
			}
			modTime := uint64(stat.ModTime().UnixNano())
			var walPath, newWalPath string
			// get expired wal dir
			walPath, newWalPath, err = parseExpiredWalPath(path.Join(d.Path, filename), dpid)
			if err != nil {
				failedDps = append(failedDps, dpid)
				log.LogErrorf("action[RestoreExpiredPartitions] partition[%v] err:%v", dpid, err)
				continue
			}
			newFileName := strings.Join(parts[1:4], "_")
			if dpInfo, ok := reloadMap[dpid]; ok {
				if delTime > dpInfo.delTime || (delTime == dpInfo.delTime && modTime > dpInfo.modTime) {
					reloadMap[dpid] = struct {
						disk        *Disk
						delTime     uint64
						walPath     string
						newWalPath  string
						newFileName string
						fileName    string
						modTime     uint64
					}{disk: d, delTime: delTime, walPath: walPath, newWalPath: newWalPath, fileName: filename, newFileName: newFileName, modTime: modTime}
				}
			} else {
				reloadMap[dpid] = struct {
					disk        *Disk
					delTime     uint64
					walPath     string
					newWalPath  string
					newFileName string
					fileName    string
					modTime     uint64
				}{disk: d, delTime: delTime, walPath: walPath, newWalPath: newWalPath, fileName: filename, newFileName: newFileName, modTime: modTime}
			}
		}
	}
	log.LogWarnf("action[RestoreExpiredPartitions] total need restore partitions:%v", len(reloadMap))
	for dpid, dpInfo := range reloadMap {
		// rename wal dir
		if dpInfo.walPath != dpInfo.newWalPath {
			err = os.Rename(path.Join(dpInfo.disk.Path, dpInfo.fileName, dpInfo.walPath), path.Join(dpInfo.disk.Path, dpInfo.fileName, dpInfo.newWalPath))
			if err != nil {
				failedDps = append(failedDps, dpid)
				continue
			}
		}
		// rename partition dir
		err = os.Rename(path.Join(dpInfo.disk.Path, dpInfo.fileName), path.Join(dpInfo.disk.Path, dpInfo.newFileName))
		if err != nil {
			failedDps = append(failedDps, dpid)
			continue
		}
		err = manager.LoadPartition(dpInfo.disk, dpid, dpInfo.newFileName, true)
		if err != nil {
			failedDps = append(failedDps, dpid)
			continue
		}
		successDps = append(successDps, dpid)
	}
	return
}
func parseExpiredWalPath(path string, dpid uint64) (walPath, newWalPath string, err error) {
	walExpireDirName := regexp.MustCompile("^expired_wal_(\\d)+_(\\d)+$")
	walDirName := fmt.Sprintf("wal_%d", dpid)
	var dpEntries []fs.DirEntry
	dpEntries, err = os.ReadDir(path)
	if err != nil {
		return
	}
	existWal := false
	for _, dpEntry := range dpEntries {
		if !dpEntry.IsDir() {
			continue
		}
		if walDirName == dpEntry.Name() {
			walPath = walDirName
			existWal = true
			break
		}
	}
	if existWal {
		walPath = walDirName
		newWalPath = walDirName
		return
	}
	for _, dpEntry := range dpEntries {
		if !dpEntry.IsDir() {
			continue
		}
		if walExpireDirName.MatchString(dpEntry.Name()) {
			walPath = dpEntry.Name()
			break
		}
	}
	if walPath == "" {
		err = fmt.Errorf("empty wal path")
		return
	}
	walParts := strings.Split(walPath, "_")
	newWalPath = strings.Join(walParts[1:3], "_")
	return
}
