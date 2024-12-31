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
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/util/infra"

	"github.com/cubefs/cubefs/util/topology"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/storage"
	"github.com/cubefs/cubefs/util/async"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/statistics"
	"github.com/cubefs/cubefs/util/unit"
	"github.com/shirou/gopsutil/load"
	"golang.org/x/time/rate"
)

var (
	// RegexpDataPartitionDir validates the directory name of a data partition.
	RegexpDataPartitionDir, _ = regexp.Compile("^datapartition_(\\d)+_(\\d)+$")
)

const (
	ExpiredPartitionPrefix = "expired_"

	LatestFlushTimeFile     = "LATEST_FLUSH"
	TempLatestFlushTimeFile = ".LATEST_FLUSH"

	SfxIssueDiskMarkFile = "SFX_ISSUE_DISK"
)

type GetReservedRatioFunc func() float64

// Disk represents the structure of the disk
type Disk struct {
	sync.RWMutex
	Path        string
	ReadErrCnt  uint64 // number of read errors
	WriteErrCnt uint64 // number of write errors

	Total       uint64
	Used        uint64
	Available   uint64
	Unallocated uint64
	Allocated   uint64

	MaxErrCnt int // maximum number of errors
	Status    int // disk status such as READONLY

	getReservedRatio GetReservedRatioFunc
	ReservedSpace    uint64

	partitionMap map[uint64]*DataPartition
	space        *SpaceManager

	// Parallel limit control
	fixTinyDeleteRecordLimit     uint64 // Limit for parallel fix tiny delete record tasks
	executingFixTinyDeleteRecord uint64 // Count of executing fix tiny delete record tasks
	executingRepairTask          uint64 // Count of executing data repair tasks
	limitLock                    sync.Mutex

	// Runtime statistics
	fdCount           int64
	maxFDLimit        uint64
	forceEvictFDRatio unit.Ratio

	forceFlushFDParallelism uint64 // 控制Flush文件句柄的并发度

	latestFlushTimeOnInit int64 // Disk 实例初始化时加载到的该磁盘最近一次Flush数据的时间

	topoManager  *topology.TopologyManager
	monitorData  []*statistics.MonitorData
	interceptors storage.IOInterceptors

	// sfx compressible ssd attribute
	IsSfx             bool
	devName           string
	PhysicalUsedRatio uint32 //physical space usage ratio
	CompressionRatio  uint32 //full disk compression ratio
}

type CheckExpired func(id uint64) bool

func OpenDisk(path string, config *DiskConfig, space *SpaceManager, parallelism int, topoManager *topology.TopologyManager, expired CheckExpired) (d *Disk, err error) {
	_, err = os.Stat(path)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			d = nil
		}
	}()

	d = &Disk{
		Path:                     path,
		getReservedRatio:         config.GetReservedRatio,
		MaxErrCnt:                config.MaxErrCnt,
		space:                    space,
		partitionMap:             make(map[uint64]*DataPartition),
		fixTinyDeleteRecordLimit: config.FixTinyDeleteRecordLimit,
		fdCount:                  0,
		maxFDLimit:               config.MaxFDLimit,
		forceEvictFDRatio:        config.ForceFDEvictRatio,
		forceFlushFDParallelism:  DefaultForceFlushFDParallelismOnDisk,
		topoManager:              topoManager,
		monitorData:              statistics.InitMonitorData(statistics.ModelDataNode),
	}

	d.initInterceptors()

	d.IsSfx, d.devName = GetDevCheckSfx(d.Path)
	if d.IsSfx {
		if err = d.checkSfxIssueDiskStatus(); err != nil {
			return
		}
		log.LogInfof("Disk %v is on sfx csd\n", d.Path)
	}

	if err = d.computeUsage(); err != nil {
		return
	}
	d.updateSpaceInfo()
	if err = d.loadLatestFlushTime(); err != nil {
		return
	}

	if err = d.loadPartitions(parallelism, expired); err != nil {
		d = nil
		return
	}

	d.startScheduler(d.managementScheduler)
	d.startScheduler(d.flushDeleteScheduler)
	d.startScheduler(d.crcComputationScheduler)
	d.startScheduler(d.flushFPScheduler)

	return
}

func (d *Disk) startScheduler(workerFunc async.WorkerFunc) {
	const (
		panicAlertInterval      = time.Minute * 10
		pendingRelaunchInterval = time.Second * 10
	)
	var collectStack = func() string {
		pc := make([]uintptr, 10)
		n := runtime.Callers(3, pc)
		if n == 0 {
			return ""
		}
		pc = pc[:n]
		frames := runtime.CallersFrames(pc)
		var sb strings.Builder
		for {
			frame, more := frames.Next()
			if !strings.HasPrefix(frame.Function, "runtime.") {
				sb.WriteString(fmt.Sprintf("%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line))
			}
			if !more {
				break
			}
		}
		return sb.String()
	}
	var pervAlertTime time.Time
	// 为WorkerFunc的执行增加panic保护和Alert
	var panicRecoverableWorkerFunc async.WorkerFunc = func() {
		defer func() {
			if r := recover(); r != nil {
				if now := time.Now(); now.Sub(pervAlertTime) > panicAlertInterval {
					// 避免报警过于频繁
					var stack = collectStack()
					log.LogCriticalf("Disk %v: async scheduler occurred panic: %v\nCallstack:\n%v",
						d.Path, r, stack)
					exporter.Warning(fmt.Sprintf(
						"Worker occurred panic!\n"+
							"Message: %v\n"+
							"Callstack:\n%v\n",
						r, stack))
					pervAlertTime = now
				}
			}
		}()
		workerFunc()
	}
	// 为WorkerFunc增加自动重启机制
	var restartableWorkerFunc async.WorkerFunc = func() {
		for {
			panicRecoverableWorkerFunc()
			time.Sleep(pendingRelaunchInterval)
		}
	}
	async.RunWorker(restartableWorkerFunc)
}

func (d *Disk) initInterceptors() {
	const (
		ctxKeyExporterTP byte = 0x00
		ctxKeyMonitorTP  byte = 0x01
	)
	type __pair struct {
		Typ         storage.IOType
		ExporterKey string
		MonitorAct  int
	}
	var unifiedDiskPath = strings.Trim(strings.ReplaceAll(d.Path, "/", "_"), "_")
	var pairs = []__pair{
		{
			Typ:         storage.IOCreate,
			ExporterKey: fmt.Sprintf("diskcreate_%s", unifiedDiskPath),
			MonitorAct:  proto.ActionDiskIOCreate,
		},
		{
			Typ:         storage.IOWrite,
			ExporterKey: fmt.Sprintf("diskwrite_%s", unifiedDiskPath),
			MonitorAct:  proto.ActionDiskIOWrite,
		},
		{
			Typ:         storage.IORead,
			ExporterKey: fmt.Sprintf("diskread_%s", unifiedDiskPath),
			MonitorAct:  proto.ActionDiskIORead,
		},
		{
			Typ:         storage.IORemove,
			ExporterKey: fmt.Sprintf("diskremove_%s", unifiedDiskPath),
			MonitorAct:  proto.ActionDiskIORemove,
		},
		{
			Typ:         storage.IOPunch,
			ExporterKey: fmt.Sprintf("diskpunch_%s", unifiedDiskPath),
			MonitorAct:  proto.ActionDiskIOPunch,
		},
		{
			Typ:         storage.IOSync,
			ExporterKey: fmt.Sprintf("disksync_%s", unifiedDiskPath),
			MonitorAct:  proto.ActionDiskIOSync,
		},
	}
	for _, keys := range pairs {
		var typ, exporterKey, monitorAct = keys.Typ, keys.ExporterKey, keys.MonitorAct
		d.interceptors.Register(typ,
			storage.NewFuncInterceptor(
				func() (ctx context.Context, err error) {
					ctx = context.Background()
					ctx = context.WithValue(ctx, ctxKeyExporterTP, exporter.NewModuleTPUs(exporterKey))
					ctx = context.WithValue(ctx, ctxKeyMonitorTP, d.monitorData[monitorAct].BeforeTp())
					return
				},
				func(ctx context.Context, n int64, err error) {
					ctx.Value(ctxKeyExporterTP).(exporter.TP).Set(nil)
					ctx.Value(ctxKeyMonitorTP).(*statistics.TpObject).AfterTp(uint64(n))
				}))
	}
}

func (d *Disk) SetForceFlushFDParallelism(parallelism uint64) {
	if parallelism <= 0 {
		parallelism = DefaultForceFlushFDParallelismOnDisk
	}
	if parallelism != d.forceFlushFDParallelism {
		if log.IsDebugEnabled() {
			log.LogDebugf("Disk %v: flush FD parallelism changed, prev %v, new %v", d.Path, d.forceFlushFDParallelism, parallelism)
		}
		d.forceFlushFDParallelism = parallelism
	}
}

func (d *Disk) IncreaseFDCount() {
	atomic.AddInt64(&d.fdCount, 1)
}

func (d *Disk) DecreaseFDCount() {
	atomic.AddInt64(&d.fdCount, -1)
}

// PartitionCount returns the number of partitions in the partition map.
func (d *Disk) PartitionCount() int {
	d.RLock()
	defer d.RUnlock()
	return len(d.partitionMap)
}

func (d *Disk) computeUsage() (err error) {
	if d.IsSfx {
		err = d.computeUsageOnSFXDevice()
		return
	}
	err = d.computeUsageOnStdDevice()
	return
}

// computeUsageOnSFXDevice computes the disk usage on SFX device
func (d *Disk) computeUsageOnSFXDevice() (err error) {
	if d.IsSfx {
		// 物理状态
		var dStatus sfxStatus
		dStatus, err = GetSfxStatus(d.devName)
		if err != nil {
			return
		}
		// 逻辑状态
		var fsstat = new(syscall.Statfs_t)
		if err = syscall.Statfs(d.Path, fsstat); err != nil {
			return
		}
		d.RLock()
		defer d.RUnlock()
		// 基于物理容量计算保留空间
		reservedSpace := int64(float64(dStatus.totalPhysicalCapability) * d.getReservedRatio())
		d.ReservedSpace = uint64(reservedSpace)

		// 基于物理容量及物理保留空间计算总容量
		total := int64(dStatus.totalPhysicalCapability) - reservedSpace
		if total < 0 {
			total = 0
		}
		d.Total = uint64(total)

		// 基于物理可用空间和逻辑可用空间计算可用空间, 取两者的最小值
		var physicalAvail = int64(dStatus.freePhysicalCapability) - reservedSpace
		var logicalAvail = int64(fsstat.Bavail)*fsstat.Bsize - reservedSpace
		var available = int64(math.Min(float64(physicalAvail), float64(logicalAvail)))
		if available < 0 {
			available = 0
		}
		d.Available = uint64(available)
		used := int64(dStatus.totalPhysicalCapability) -
			int64(math.Min(float64(dStatus.freePhysicalCapability), float64(int64(fsstat.Bavail)*fsstat.Bsize)))
		if used < 0 {
			used = 0
		}
		d.Used = uint64(used)

		allocatedSize := int64(0)
		for _, dp := range d.partitionMap {
			allocatedSize += int64(dp.Size())
		}
		atomic.StoreUint64(&d.Allocated, uint64(allocatedSize))
		unallocated := total - allocatedSize
		if unallocated < 0 {
			unallocated = 0
		}
		d.Unallocated = uint64(unallocated)

		d.PhysicalUsedRatio = dStatus.physicalUsageRatio
		d.CompressionRatio = dStatus.compRatio
		log.LogDebugf("Disk %v: compute usage: totalPhysicalSpace(%v) freePhysicalSpace(%v) PhysicalUsedRatio(%v) CompressionRatio(%v)",
			d.Path, d.Total, d.Available, d.PhysicalUsedRatio, d.CompressionRatio)
		if int64(fsstat.Bavail)*fsstat.Bsize < unit.GB {
			exporter.WarningBySpecialUMPKey(fmt.Sprintf("%v_%v_%v", d.space.clusterID, ModuleName, "NoSpace"), fmt.Sprintf("path: %v, available space less than 1 GB", d.Path))
		}
	}
	return
}

// computeUsageOnStdDevice computes the disk usage on standard device
func (d *Disk) computeUsageOnStdDevice() (err error) {
	fs := syscall.Statfs_t{}
	if err = syscall.Statfs(d.Path, &fs); err != nil {
		d.incReadErrCnt()
		return
	}
	d.RLock()
	defer d.RUnlock()
	//  total := math.Max(0, int64(fs.Blocks*uint64(fs.Bsize)- d.ReservedSpace))
	capacity := int64(fs.Blocks * uint64(fs.Bsize))
	reservedSpace := int64(float64(capacity) * d.getReservedRatio())
	d.ReservedSpace = uint64(reservedSpace)

	total := capacity - reservedSpace
	if total < 0 {
		total = 0
	}
	d.Total = uint64(total)
	//  available := math.Max(0, int64(fs.Bavail*uint64(fs.Bsize) - d.ReservedSpace))
	available := int64(fs.Bavail*uint64(fs.Bsize)) - reservedSpace
	if available < 0 {
		available = 0
	}
	d.Available = uint64(available)

	used := int64(fs.Blocks*uint64(fs.Bsize) - fs.Bavail*uint64(fs.Bsize))
	if used < 0 {
		used = 0
	}
	d.Used = uint64(used)

	allocatedSize := int64(0)
	for _, dp := range d.partitionMap {
		allocatedSize += int64(dp.Size())
	}
	atomic.StoreUint64(&d.Allocated, uint64(allocatedSize))
	//  unallocated = math.Max(0, total - allocatedSize)
	unallocated := total - allocatedSize
	if unallocated < 0 {
		unallocated = 0
	}
	d.Unallocated = uint64(unallocated)

	if log.IsDebugEnabled() {
		log.LogDebugf("Disk %v: computed usage: Capacity %v, Available %v, Used %v, Allocated %v, Unallocated %v",
			d.Path, d.Total, d.Available, d.Used, allocatedSize, unallocated)
	}

	if fs.Bavail*uint64(fs.Bsize) < unit.GB {
		exporter.WarningBySpecialUMPKey(fmt.Sprintf("%v_%v_%v", d.space.clusterID, ModuleName, "NoSpace"), fmt.Sprintf("path: %v, available space less than 1 GB", d.Path))
	}
	return
}

func (d *Disk) incReadErrCnt() {
	atomic.AddUint64(&d.ReadErrCnt, 1)
}

func (d *Disk) incWriteErrCnt() {
	atomic.AddUint64(&d.WriteErrCnt, 1)
}

func (d *Disk) flushFPScheduler() {
	flushFDSecond := d.space.flushFDIntervalSec
	if flushFDSecond == 0 {
		flushFDSecond = DefaultForceFlushFDSecond
	}

	var (
		flushTicker = time.NewTicker(time.Duration(flushFDSecond) * time.Second)
		flushWindow = time.Duration(flushFDSecond) * time.Second
	)

	defer func() {
		flushTicker.Stop()
	}()
	for {
		select {
		case <-flushTicker.C:
			if !gHasLoadDataPartition {
				continue
			}
			avg, err := load.Avg()
			if err != nil {
				log.LogErrorf("Disk %v: get host load value failed: %v", d.Path, err)
				continue
			}
			if avg.Load1 > 1000.0 {
				if log.IsWarnEnabled() {
					log.LogWarnf("Disk %v: skip flush FD: host load value larger than 1000", d.Path)
				}
				continue
			}
			var parallelism = d.forceFlushFDParallelism
			if parallelism <= 0 {
				parallelism = DefaultForceFlushFDParallelismOnDisk
			}
			d.__flushInWindow(int(parallelism), flushWindow)
		}
		if d.maybeUpdateFlushFDInterval(flushFDSecond) {
			log.LogDebugf("action[startFlushFPScheduler] disk(%v) update ticker from(%v) to (%v)", d.Path, flushFDSecond, d.space.flushFDIntervalSec)
			oldFlushFDSecond := flushFDSecond
			flushFDSecond = d.space.flushFDIntervalSec
			if flushFDSecond > 0 {
				flushTicker.Reset(time.Duration(flushFDSecond) * time.Second)
				flushWindow = time.Duration(flushFDSecond) * time.Second
			} else {
				flushFDSecond = oldFlushFDSecond
			}
		}
	}
}

func (d *Disk) markSfxIssueDiskStatus() {
	d.Status = proto.Unavailable
	mesg := fmt.Sprintf("disk path %v error on %v", d.Path, LocalIP)
	exporter.Warning(mesg)
	log.LogErrorf(mesg)
	go d.ForceExitRaftStore()
	var sfxErrorStatusFilepath = path.Join(d.Path, SfxIssueDiskMarkFile)
	_ = os.WriteFile(sfxErrorStatusFilepath, nil, 0644)
}

func (d *Disk) checkSfxIssueDiskStatus() error {
	var sfxErrorStatusFilepath = path.Join(d.Path, SfxIssueDiskMarkFile)
	if _, err := os.Stat(sfxErrorStatusFilepath); err == nil {
		return errors.New("issue disk status detected")
	}
	return nil
}

func (d *Disk) maybeUpdateFlushFDInterval(oldVal uint32) bool {
	if d.space.flushFDIntervalSec > 0 && oldVal != d.space.flushFDIntervalSec {
		return true
	}
	return false
}

func (d *Disk) managementScheduler() {
	var (
		updateSpaceInfoTicker        = time.NewTicker(5 * time.Second)
		checkStatusTicker            = time.NewTicker(time.Minute * 2)
		evictFDTicker                = time.NewTicker(time.Minute * 5)
		forceEvictFDTicker           = time.NewTicker(time.Second * 10)
		evictExtentDeleteCacheTicker = time.NewTicker(time.Minute * 10)
		freeExtentLockInfoTicker     = time.NewTicker(time.Second * 20)
	)
	defer func() {
		updateSpaceInfoTicker.Stop()
		checkStatusTicker.Stop()
		evictFDTicker.Stop()
		forceEvictFDTicker.Stop()
		evictExtentDeleteCacheTicker.Stop()
		freeExtentLockInfoTicker.Stop()
	}()
	for {
		select {
		case <-updateSpaceInfoTicker.C:
			if err := d.computeUsage(); err != nil {
				log.LogErrorf("Disk %v: compute usage failed: %v", d.Path, err)
			}
			d.updateSpaceInfo()
		case <-checkStatusTicker.C:
			d.checkDiskStatus()
		case <-evictFDTicker.C:
			d.evictExpiredFileDescriptor()
		case <-forceEvictFDTicker.C:
			d.forceEvictFileDescriptor()
		case <-evictExtentDeleteCacheTicker.C:
			d.evictExpiredExtentDeleteCache()
		case <-freeExtentLockInfoTicker.C:
			d.freeExtentLockInfo()
		}
	}
}

func (d *Disk) flushDeleteScheduler() {
	var ticker = time.NewTicker(time.Second * 15)
	for {
		select {
		case <-ticker.C:
			if !gHasLoadDataPartition {
				continue
			}
			avg, err := load.Avg()
			if err != nil {
				log.LogErrorf("Disk %v: get host load value failed: %v", d.Path, err)
				continue
			}
			if math.Max(avg.Load1, avg.Load5) > 1000.00 {
				if log.IsWarnEnabled() {
					log.LogWarnf("Disk %v: skip flush delete: host load value larger than 1000", d.Path)
				}
				continue
			}
			d.flushDelete()
		}
	}
}

func (d *Disk) flushDelete() {

	var __flushOnce = func() (deleted, remain int, goon bool) {

		d.WalkPartitions(func(partition *DataPartition) bool {
			const limit = 128
			var n, r, err = partition.FlushDelete(limit)
			if err != nil {
				log.LogErrorf("DP %v: flush delete failed: %v", partition.ID(), err)
			}
			deleted += n
			remain += r
			if r > 32 {
				goon = true
			}
			if n > 0 {
				runtime.Gosched()
			}
			return true
		})
		return
	}

	var (
		deleted int
		remain  int
		start   = time.Now()
	)
	for {
		var d, r, goon = __flushOnce()
		deleted += d
		remain = r
		if goon {
			runtime.Gosched()
			continue
		}
		break
	}
	if deleted > 0 && log.IsDebugEnabled() {
		log.LogDebugf("Disk %v: flush delete complete: deleted %v, remain %v, elapsed %vms",
			d.Path, deleted, remain, time.Now().Sub(start).Milliseconds())
	}
	return
}

func (d *Disk) crcComputationScheduler() {
	var timer = time.NewTimer(0)
	for {
		<-timer.C
		avg, err := load.Avg()
		if err != nil {
			log.LogErrorf("Disk %v: get host load value failed: %v", d.Path, err)
			timer.Reset(time.Minute)
			continue
		}
		if avg.Load1 > 1000.0 {
			if log.IsWarnEnabled() {
				log.LogWarnf("Disk %v: skip compute CRC: host load value larger than 1000", d.Path)
			}
			timer.Reset(time.Minute)
			continue
		}
		d.WalkPartitions(func(partition *DataPartition) bool {
			partition.ExtentStore().AutoComputeExtentCrc()
			return true
		})
		timer.Reset(time.Minute)
	}
}

func (d *Disk) SetFixTinyDeleteRecordLimitOnDisk(value uint64) {
	d.limitLock.Lock()
	defer d.limitLock.Unlock()
	if d.fixTinyDeleteRecordLimit != value {
		log.LogInfof("action[updateTaskExecutionLimit] disk(%v) change fixTinyDeleteRecordLimit from(%v) to(%v)", d.Path, d.fixTinyDeleteRecordLimit, value)
		d.fixTinyDeleteRecordLimit = value
	}
}

const (
	DiskStatusFile = ".diskStatus"
)

func (d *Disk) checkDiskStatus() {
	path := path.Join(d.Path, DiskStatusFile)
	fp, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0755)
	if err != nil {
		d.triggerDiskError(err)
		return
	}
	defer fp.Close()
	data := []byte(DiskStatusFile)
	_, err = fp.WriteAt(data, 0)
	if err != nil {
		d.triggerDiskError(err)
		return
	}
	if err = fp.Sync(); err != nil {
		d.triggerDiskError(err)
		return
	}
	if _, err = fp.ReadAt(data, 0); err != nil {
		d.triggerDiskError(err)
		return
	}
}

func (d *Disk) triggerDiskError(err error) {
	if err == nil {
		return
	}
	if IsDiskErr(err) {
		d.Status = proto.Unavailable
		mesg := fmt.Sprintf("disk path %v error on %v", d.Path, LocalIP)
		exporter.Warning(mesg)
		log.LogErrorf(mesg)
		d.ForceExitRaftStore()
	}
	return
}

func (d *Disk) updateSpaceInfo() {
	if d.Status == proto.Unavailable {
		mesg := fmt.Sprintf("disk path %v error on %v", d.Path, LocalIP)
		log.LogErrorf(mesg)
		exporter.Warning(mesg)
		d.ForceExitRaftStore()
	} else if d.Available <= 0 {
		d.Status = proto.ReadOnly
	} else {
		d.Status = proto.ReadWrite
	}
	if log.IsDebugEnabled() {
		log.LogDebugf("Disk %v: updated space info: total(%v) available(%v) remain(%v) "+
			"reservedSpace(%v) maxErrs(%v) readErrs(%v) writeErrs(%v) status(%v)", d.Path,
			d.Total, d.Available, d.Unallocated, d.ReservedSpace, d.MaxErrCnt, d.ReadErrCnt, d.WriteErrCnt, d.Status)
	}
	return
}

// AttachDataPartition adds a data partition to the partition map.G
func (d *Disk) AttachDataPartition(dp *DataPartition) {
	d.Lock()
	d.partitionMap[dp.ID()] = dp
	d.Unlock()

	_ = d.computeUsage()
}

// DetachDataPartition removes a data partition from the partition map.
func (d *Disk) DetachDataPartition(dp *DataPartition) {
	d.Lock()
	delete(d.partitionMap, dp.ID())
	d.Unlock()

	_ = d.computeUsage()
}

// GetDataPartition returns the data partition based on the given partition ID.
func (d *Disk) GetDataPartition(partitionID uint64) (partition *DataPartition) {
	d.RLock()
	defer d.RUnlock()
	return d.partitionMap[partitionID]
}

func (d *Disk) ForceExitRaftStore() {
	partitionList := d.DataPartitionList()
	for _, partitionID := range partitionList {
		if partition := d.GetDataPartition(partitionID); partition != nil {
			partition.partitionStatus = proto.Unavailable
			partition.stopRaft()
		}
	}
}

// DataPartitionList returns a list of the data partitions
func (d *Disk) DataPartitionList() (partitionIDs []uint64) {
	d.Lock()
	defer d.Unlock()
	partitionIDs = make([]uint64, 0, len(d.partitionMap))
	for _, dp := range d.partitionMap {
		partitionIDs = append(partitionIDs, dp.ID())
	}
	return
}

func unmarshalPartitionName(name string) (partitionID uint64, partitionSize int, err error) {
	arr := strings.Split(name, "_")
	if len(arr) != 3 {
		err = fmt.Errorf("error DataPartition name(%v)", name)
		return
	}
	if partitionID, err = strconv.ParseUint(arr[1], 10, 64); err != nil {
		return
	}
	if partitionSize, err = strconv.Atoi(arr[2]); err != nil {
		return
	}
	return
}

func (d *Disk) isPartitionDir(filename string) (isPartitionDir bool) {
	isPartitionDir = RegexpDataPartitionDir.MatchString(filename)
	return
}

// RestorePartition reads the files stored on the local disk and restores the data partitions.
func (d *Disk) loadPartitions(parallelism int, expired CheckExpired) (err error) {

	var (
		partitionID uint64
		diskFp      *os.File
	)

	if diskFp, err = os.Open(d.Path); err != nil {
		return
	}

	var filenames []string
	if filenames, err = diskFp.Readdirnames(-1); err != nil {
		return
	}

	if parallelism < 1 {
		parallelism = 1
	}
	var loadWaitGroup = new(sync.WaitGroup)
	var filenameCh = make(chan string, parallelism)
	for i := 0; i < parallelism; i++ {
		loadWaitGroup.Add(1)
		go func() {
			defer loadWaitGroup.Done()
			var (
				filename  string
				partition *DataPartition
				loadErr   error
			)
			for {
				if filename = <-filenameCh; len(filename) == 0 {
					return
				}
				partitionFullPath := path.Join(d.Path, filename)
				startTime := time.Now()
				if partition, loadErr = d.loadPartition(partitionFullPath); loadErr != nil {
					msg := fmt.Sprintf("load partition(%v) failed: %v",
						partitionFullPath, loadErr)
					log.LogError(msg)
					exporter.Warning(msg)
					continue
				}
				log.LogInfof("DP(%v) load complete, elapsed %v",
					partitionFullPath, time.Since(startTime))
				d.AttachDataPartition(partition)
			}
		}()
	}
	go func() {
		for _, filename := range filenames {
			if !d.isPartitionDir(filename) {
				continue
			}

			if partitionID, _, err = unmarshalPartitionName(filename); err != nil {
				log.LogErrorf("action[RestorePartition] unmarshal partitionName(%v) from disk(%v) err(%v) ",
					filename, d.Path, err.Error())
				continue
			}
			if expired != nil && expired(partitionID) {
				if log.IsWarnEnabled() {
					log.LogWarnf("Disk %v: found expired DP %v (%v): rename it and you can delete it "+
						"manually", d.Path, partitionID, filename)
				}
				oldName := path.Join(d.Path, filename)
				newName := path.Join(d.Path, ExpiredPartitionPrefix+filename)
				_ = os.Rename(oldName, newName)
				continue
			}
			filenameCh <- filename
		}
		close(filenameCh)
	}()

	loadWaitGroup.Wait()
	return
}

// RestoreOnePartition restores the data partition.
func (d *Disk) RestoreOnePartition(partitionPath string, force bool) (err error) {
	var (
		partitionID uint64
		partition   *DataPartition
		dInfo       *DataNodeInfo
	)
	if len(partitionPath) == 0 {
		err = fmt.Errorf("action[RestoreOnePartition] partition path is empty")
		return
	}
	partitionFullPath := path.Join(d.Path, partitionPath)
	_, err = os.Stat(partitionFullPath)
	if err != nil {
		err = fmt.Errorf("action[RestoreOnePartition] read dir(%v) err(%v)", partitionFullPath, err)
		return
	}
	if !d.isPartitionDir(partitionPath) {
		err = fmt.Errorf("action[RestoreOnePartition] invalid partition path")
		return
	}

	if partitionID, _, err = unmarshalPartitionName(partitionPath); err != nil {
		err = fmt.Errorf("action[RestoreOnePartition] unmarshal partitionName(%v) from disk(%v) err(%v) ",
			partitionPath, d.Path, err.Error())
		return
	}

	dInfo, err = d.getPersistPartitionsFromMaster()
	if err != nil {
		return
	}
	if len(dInfo.PersistenceDataPartitions) == 0 {
		log.LogWarnf("action[RestoreOnePartition]: length of PersistenceDataPartitions is 0, ExpiredPartition check without effect")
	}

	if !force && isExpiredPartition(partitionID, dInfo.PersistenceDataPartitions) {
		err = fmt.Errorf("find expired partition[%s], rename it and you can delete it manually", partitionPath)
		log.LogErrorf("action[RestoreOnePartition] err: %v", err)
		newName := path.Join(d.Path, ExpiredPartitionPrefix+partitionPath)
		_ = os.Rename(partitionFullPath, newName)
		return
	}

	startTime := time.Now()
	if partition, err = d.loadPartition(partitionFullPath); err != nil {
		msg := fmt.Sprintf("load partition(%v) failed: %v",
			partitionFullPath, err)
		log.LogError(msg)
		exporter.Warning(msg)
		return
	}
	log.LogInfof("partition(%v) load complete cost(%v)",
		partitionFullPath, time.Since(startTime))
	d.AttachDataPartition(partition)
	d.space.AttachPartition(partition)
	return
}

func (d *Disk) WalkPartitions(visitor func(*DataPartition) bool) {
	if visitor == nil {
		return
	}
	for _, partition := range d.__partitions() {
		if !visitor(partition) {
			break
		}
	}
}

func (d *Disk) __partitions() []*DataPartition {
	d.Lock()
	var partitions = make([]*DataPartition, 0, len(d.partitionMap))
	for _, partition := range d.partitionMap {
		partitions = append(partitions, partition)
	}
	d.Unlock()
	return partitions
}

func (d *Disk) AsyncLoadExtent(parallelism int) {

	if log.IsInfoEnabled() {
		log.LogInfof("Disk %v: storage lazy load start", d.Path)
	}
	var start = time.Now()
	var wg = new(sync.WaitGroup)
	var partitionCh = make(chan *DataPartition, parallelism)
	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		async.RunWorker(func() {
			defer wg.Done()
			var dp *DataPartition
			for {
				if dp = <-partitionCh; dp == nil {
					return
				}
				dp.ExtentStore().Load()
			}
		})
	}
	wg.Add(1)
	async.RunWorker(func() {
		defer wg.Done()
		d.WalkPartitions(func(partition *DataPartition) bool {
			partitionCh <- partition
			return true
		})
		close(partitionCh)
	})
	wg.Wait()
	if log.IsInfoEnabled() {
		log.LogInfof("Disk %v: storage lazy load complete, elapsed %v", d.Path, time.Now().Sub(start))
	}
}

func (d *Disk) getPersistPartitionsFromMaster() (dInfo *DataNodeInfo, err error) {
	var dataNode *proto.DataNodeInfo
	var convert = func(node *proto.DataNodeInfo) *DataNodeInfo {
		result := &DataNodeInfo{}
		result.Addr = node.Addr
		result.PersistenceDataPartitions = node.PersistenceDataPartitions
		return result
	}
	for i := 0; i < 3; i++ {
		dataNode, err = MasterClient.NodeAPI().GetDataNode(d.space.dataNode.localServerAddr)
		if err != nil {
			log.LogErrorf("action[RestorePartition]: getDataNode error %v", err)
			continue
		}
		break
	}
	if err != nil {
		return
	}
	dInfo = convert(dataNode)
	return
}

func (d *Disk) AddSize(size uint64) {
	atomic.AddUint64(&d.Allocated, size)
}

func (d *Disk) getSelectWeight() float64 {
	return float64(atomic.LoadUint64(&d.Allocated)) / float64(d.Total)
}

// isExpiredPartition return whether one partition is expired
// if one partition does not exist in master, we decided that it is one expired partition
func isExpiredPartition(id uint64, partitions []uint64) bool {
	if len(partitions) == 0 {
		return true
	}

	for _, existId := range partitions {
		if existId == id {
			return false
		}
	}
	return true
}

func (d *Disk) canFinTinyDeleteRecord() bool {
	d.limitLock.Lock()
	defer d.limitLock.Unlock()
	if d.executingFixTinyDeleteRecord >= d.fixTinyDeleteRecordLimit {
		return false
	}
	d.executingFixTinyDeleteRecord++
	return true
}

func (d *Disk) finishFixTinyDeleteRecord() {
	d.limitLock.Lock()
	defer d.limitLock.Unlock()
	if d.executingFixTinyDeleteRecord > 0 {
		d.executingFixTinyDeleteRecord--
	}
}

func (d *Disk) evictExpiredFileDescriptor() {
	d.RLock()
	var partitions = make([]*DataPartition, 0, len(d.partitionMap))
	for _, partition := range d.partitionMap {
		partitions = append(partitions, partition)
	}
	d.RUnlock()

	for _, partition := range partitions {
		partition.EvictExpiredFileDescriptor()
	}
}

func (d *Disk) forceEvictFileDescriptor() {
	var count = atomic.LoadInt64(&d.fdCount)
	log.LogDebugf("action[forceEvictFileDescriptor] disk(%v) current FD count(%v)",
		d.Path, count)
	d.RLock()
	var partitions = make([]*DataPartition, 0, len(d.partitionMap))
	for _, partition := range d.partitionMap {
		partitions = append(partitions, partition)
	}
	d.RUnlock()
	for _, partition := range partitions {
		partition.ForceEvictFileDescriptor(d.forceEvictFDRatio)
	}
	log.LogDebugf("action[forceEvictFileDescriptor] disk(%v) evicted FD count [%v -> %v]",
		d.Path, count, atomic.LoadInt64(&d.fdCount))
}

func (d *Disk) evictExpiredExtentDeleteCache() {
	var expireTime uint64
	log.LogDebugf("action[evictExpiredExtentDeleteCache] disk(%v) evict start", d.Path)
	d.RLock()
	expireTime = d.space.normalExtentDeleteExpireTime
	var partitions = make([]*DataPartition, 0, len(d.partitionMap))
	for _, partition := range d.partitionMap {
		partitions = append(partitions, partition)
	}
	d.RUnlock()
	for _, partition := range partitions {
		partition.EvictExpiredExtentDeleteCache(int64(expireTime))
	}
	log.LogDebugf("action[evictExpiredExtentDeleteCache] disk(%v) evict end", d.Path)
}

// __flushInWindow flushes the data partitions in the window.
// ew: the window duration
func (d *Disk) __flushInWindow(parallelism int, ew time.Duration) {
	if !gHasLoadDataPartition {
		return
	}
	var flushTime = time.Now()
	var flushers = make([]infra.Flusher, 0, d.PartitionCount())
	var total int64
	d.WalkPartitions(func(partition *DataPartition) bool {
		var flusher = partition.Flusher()
		total += int64(flusher.Count())
		flushers = append(flushers, flusher)
		return true
	})
	if total == 0 {
		return
	}
	var opsLimiter = newFlushOpsLimiter(total, ew)                               // Limit operations per second
	var bpsLimiter = newFlushBpsLimiter(uint64(parallelism), d.isSSDMediaType()) // Limit bytes per second
	var flusherc = make(chan infra.Flusher, len(flushers))                       // 用于并发flush
	var failures int64
	var wg = new(sync.WaitGroup)
	var flushWorker async.WorkerFunc = func() {
		defer wg.Done()
		var err error
		for {
			var flusher = <-flusherc
			if flusher == nil {
				return
			}
			if err = flusher.Flush(opsLimiter, bpsLimiter); err != nil {
				err = errors.NewErrorf("__flushInWindow: flush failed: %v", err.Error())
				log.LogErrorf(err.Error())
				atomic.AddInt64(&failures, 1)
			}
		}
	}
	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		async.RunWorker(flushWorker)
	}
	for _, flusher := range flushers {
		flusherc <- flusher
	}
	close(flusherc)
	wg.Wait()
	if atomic.LoadInt64(&failures) == 0 {
		if err := d.persistLatestFlushTime(flushTime.Unix()); err != nil {
			log.LogErrorf("disk[%v] persist latest flush time failed: %v", d.Path, err)
		}
	}
	if log.IsWarnEnabled() {
		log.LogWarnf("Disk %v: schedule flush complete, parallelism %v, dps %v, fds %v, ops limit %v, bps limit %v, elapsed %v",
			d.Path, parallelism, len(flushers), total, opsLimiter.Limit(), bpsLimiter.Limit(), time.Now().Sub(flushTime))
	}
}

func (d *Disk) forcePersistPartitions(partitions []*DataPartition) {
	if log.IsDebugEnabled() {
		log.LogDebugf("action[forcePersistPartitions] disk(%v) partition count(%v) begin",
			d.Path, len(partitions))
	}
	pChan := make(chan *DataPartition, len(partitions))
	for _, dp := range partitions {
		pChan <- dp
	}
	wg := new(sync.WaitGroup)
	var failedCount int64
	var parallelism = d.forceFlushFDParallelism
	if parallelism <= 0 {
		parallelism = DefaultForceFlushFDParallelismOnDisk
	}
	if log.IsDebugEnabled() {
		log.LogDebugf("disk[%v] start to force persist partitions [parallelism: %v]", d.Path, parallelism)
	}
	var flushTime = time.Now()
	for i := uint64(0); i < parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case dp := <-pChan:
					if err := dp.Flush(); err != nil {
						err = errors.NewErrorf("[forcePersistPartitions]: persist all failed, partition=%d: %v", dp.config.PartitionID, err.Error())
						log.LogErrorf(err.Error())
						atomic.AddInt64(&failedCount, 1)
					}
				default:
					return
				}
			}
		}()
	}
	wg.Wait()
	close(pChan)
	if atomic.LoadInt64(&failedCount) == 0 {
		if err := d.persistLatestFlushTime(flushTime.Unix()); err != nil {
			log.LogErrorf("disk[%v] persist latest flush time failed: %v", d.Path, err)
		}
	}
	if log.IsWarnEnabled() {
		log.LogWarnf("disk[%v] flush partitions: %v/%v, elapsed %v",
			d.Path, len(partitions)-int(atomic.LoadInt64(&failedCount)), len(partitions), time.Now().Sub(flushTime))
	}
}

func newFlushBpsLimiter(parallelism uint64, isSSD bool) *rate.Limiter {
	if parallelism <= 0 {
		parallelism = DefaultForceFlushFDParallelismOnDisk
	}
	var bps uint64
	if isSSD {
		bps = parallelism * DefaultForceFlushDataSizeOnEachSSDDisk
	} else {
		bps = parallelism * DefaultForceFlushDataSizeOnEachHDDDisk
	}
	bpsLimiter := rate.NewLimiter(rate.Limit(bps), int(bps))
	return bpsLimiter
}

func newFlushOpsLimiter(total int64, ew time.Duration) *rate.Limiter {
	var opsLimiter = rate.NewLimiter(rate.Every(ew/time.Duration(total)), 1)
	return opsLimiter
}

func (d *Disk) isSSDMediaType() bool {
	return d.space.dataNode != nil && strings.Contains(d.space.dataNode.zoneName, "ssd")
}

func (d *Disk) persistLatestFlushTime(unix int64) (err error) {

	tmpFilename := path.Join(d.Path, TempLatestFlushTimeFile)
	tmpFile, err := os.OpenFile(tmpFilename, os.O_RDWR|os.O_APPEND|os.O_TRUNC|os.O_CREATE, 0755)
	if err != nil {
		return
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFilename)
	}()
	if _, err = tmpFile.WriteString(fmt.Sprintf("%d", unix)); err != nil {
		return
	}
	if err = tmpFile.Sync(); err != nil {
		return
	}
	err = os.Rename(tmpFilename, path.Join(d.Path, LatestFlushTimeFile))
	log.LogInfof("Disk %v: persist latest flush time [unix: %v]", d.Path, unix)
	return
}

func (d *Disk) loadLatestFlushTime() (err error) {
	var filename = path.Join(d.Path, LatestFlushTimeFile)
	var (
		fileBytes []byte
	)
	if fileBytes, err = ioutil.ReadFile(filename); err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}
	if _, err = fmt.Sscanf(string(fileBytes), "%d", &d.latestFlushTimeOnInit); err != nil {
		err = nil
		return
	}
	return
}

func (d *Disk) freeExtentLockInfo() {
	log.LogDebugf("action[freeExtentLockInfo] disk(%v) free start", d.Path)
	d.RLock()
	var partitions = make([]*DataPartition, 0, len(d.partitionMap))
	for _, partition := range d.partitionMap {
		partitions = append(partitions, partition)
	}
	d.RUnlock()
	for _, partition := range partitions {
		partition.ExtentStore().FreeExtentLockInfo()
	}
	log.LogDebugf("action[freeExtentLockInfo] disk(%v) free end", d.Path)
}

func (d *Disk) createPartition(dpCfg *dataPartitionCfg, request *proto.CreateDataPartitionRequest) (dp *DataPartition, err error) {

	if dp, err = newDataPartition(dpCfg, d, true, d.topoManager, d.interceptors); err != nil {
		return
	}
	dp.ForceLoadHeader()

	// persist file metadata
	dp.DataPartitionCreateType = request.CreateType
	err = dp.persistMetaDataOnly()
	d.AddSize(uint64(dp.Size()))
	if err = dp.initIssueProcessor(0); err != nil {
		return
	}
	return
}

// LoadDataPartition loads and returns a partition instance based on the specified directory.
// It reads the partition metadata file stored under the specified directory
// and creates the partition instance.
func (d *Disk) loadPartition(partitionDir string) (dp *DataPartition, err error) {
	var (
		metaFileData []byte
	)
	if metaFileData, err = ioutil.ReadFile(path.Join(partitionDir, DataPartitionMetadataFileName)); err != nil {
		return
	}
	meta := &DataPartitionMetadata{}
	if err = json.Unmarshal(metaFileData, meta); err != nil {
		return
	}
	if err = meta.Validate(); err != nil {
		return
	}

	dpCfg := &dataPartitionCfg{
		VolName:       meta.VolumeID,
		PartitionSize: meta.PartitionSize,
		PartitionID:   meta.PartitionID,
		ReplicaNum:    meta.ReplicaNum,
		Peers:         meta.Peers,
		Hosts:         meta.Hosts,
		Learners:      meta.Learners,
		RaftStore:     d.space.GetRaftStore(),
		NodeID:        d.space.GetNodeID(),
		ClusterID:     d.space.GetClusterID(),
		CreationType:  meta.DataPartitionCreateType,

		VolHAType: meta.VolumeHAType,
		Mode:      meta.ConsistencyMode,
	}
	if dp, err = newDataPartition(dpCfg, d, false, d.topoManager, d.interceptors); err != nil {
		return
	}
	// dp.PersistMetadata()

	var appliedID uint64
	if appliedID, err = dp.LoadAppliedID(); err != nil {
		log.LogErrorf("action[loadApplyIndex] %v", err)
	}
	log.LogInfof("Action(LoadDataPartition) PartitionID(%v) meta(%v)", dp.partitionID, meta)
	dp.DataPartitionCreateType = meta.DataPartitionCreateType
	dp.isCatchUp = meta.IsCatchUp
	dp.needServerFaultCheck = meta.NeedServerFaultCheck
	dp.serverFaultCheckLevel = CheckAllCommitID

	if !dp.applyStatus.Init(appliedID, meta.LastTruncateID) {
		err = fmt.Errorf("action[loadApplyIndex] illegal metadata, appliedID %v, lastTruncateID %v", appliedID, meta.LastTruncateID)
		return
	}

	d.AddSize(uint64(dp.Size()))
	dp.ForceLoadHeader()

	// 检查是否有需要更新Volume信息
	var maybeNeedUpdateCrossRegionHAType = func() bool {
		return (len(dp.config.Hosts) > 3 && dp.config.VolHAType == proto.DefaultCrossRegionHAType) ||
			(len(dp.config.Hosts) <= 3 && dp.config.VolHAType == proto.CrossRegionHATypeQuorum)
	}
	var maybeNeedUpdateReplicaNum = func() bool {
		return dp.config.ReplicaNum == 0 || len(dp.config.Hosts) != dp.config.ReplicaNum
	}
	if maybeNeedUpdateCrossRegionHAType() || maybeNeedUpdateReplicaNum() {
		dp.proposeUpdateVolumeInfo()
	}

	dp.persistedApplied = appliedID
	dp.persistedMetadata = meta
	dp.maybeUpdateFaultOccurredCheckLevel()
	if err = dp.initIssueProcessor(d.latestFlushTimeOnInit); err != nil {
		return
	}
	return
}
