package riskdata

import (
	"bufio"
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/cubefs/cubefs/util/connman"
	"hash/crc32"
	"io"
	"math"
	"net"
	"os"
	libpath "path"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/util/multirate"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/repl"
	"github.com/cubefs/cubefs/storage"
	"github.com/cubefs/cubefs/util/async"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
)

var (
	ErrIllegalFragmentLength = errors.New("illegal issue data fragment length")
	ErrBrokenFragmentsFile   = errors.New("broken issue fragments file")
	ErrBrokenFragmentData    = errors.New("broken issue fragment data")
)

const (
	fragmentsFilename    = "ISSUE_FRAGMENTS"
	fragmentBinaryLength = 28

	maxProcessorWorkers = 4
	minFixesPerWorker   = 16

	emptyResponse = 'E'
)

type GetRemotesFunc func() []string
type GetHATypeFunc func() proto.CrossRegionHAType
type LimiterFunc func(ctx context.Context, op int, size uint32, bandType string) (err error)

var (
	emptyFragmentBinary = []byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0xa3, 0xc1, 0xca, 0x20,
	}
)

type FixResult int

const (
	Success FixResult = 0
	Retry   FixResult = 1
	Failed  FixResult = 2
)

func (r FixResult) String() string {
	switch r {
	case Success:
		return "Success"
	case Retry:
		return "Retry"
	case Failed:
		return "Failed"
	}
	return "Unknown"
}

type WriterAtFunc func(b []byte, off int64) (n int, err error)

func (f WriterAtFunc) WriteAt(b []byte, off int64) (n int, err error) {
	if f != nil {
		n, err = f(b, off)
	}
	return
}

// Fixer 是用于系统级宕机引起的数据检查及损坏修复的处理器。
// 它具备以下几个功能:
// 1. 注册可能有损坏的数据区域
// 2. 判断给定数据区域是否在已注册的疑似损坏数据区域内
// 3. 检查并尝试修复疑似被损坏的数据区域.
type Fixer struct {
	path            string
	partitionID     uint64
	connPool        *connman.ConnManager
	indexes         []*fragmentIndex
	indexNextOffset int64
	indexesMu       sync.RWMutex
	queue           *list.List
	queueMu         sync.Mutex
	storage         *storage.ExtentStore
	getRemotes      GetRemotesFunc
	getHAType       GetHATypeFunc
	persistSyncCh   chan struct{}
	codecReuseBuf   []byte
	persistFp       *os.File
	limiter         LimiterFunc
	diskPath        string
	workers         int32
	workerStop      func()
	statusMu        sync.Mutex
}

func (p *Fixer) Start() {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	fragmentCount := p.fragmentCount()
	if fragmentCount > 0 && atomic.LoadInt32(&p.workers) == 0 {
		// 启动多个Worker用于修复
		workerNum := int(math.Min(math.Max(float64(fragmentCount/minFixesPerWorker), 1), float64(maxProcessorWorkers)))
		var ctx, cancel = context.WithCancel(context.Background())
		p.workerStop = cancel
		atomic.StoreInt32(&p.workers, int32(workerNum))
		for i := 0; i < workerNum; i++ {
			async.RunWorker(p.createWorker(ctx, i), p.handleWorkerPanic)
		}
	}
}

func (p *Fixer) Stop() {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	if p.workerStop != nil {
		p.workerStop()
	}
	return
}

func (p *Fixer) Status() *FixerStatus {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	return &FixerStatus{
		Fragments: p.copyFragments(),
		Count:     p.fragmentCount(),
		Running:   atomic.LoadInt32(&p.workers) > 0,
	}
}

func (p *Fixer) lockPersist() (release func()) {
	p.persistSyncCh <- struct{}{}
	release = func() {
		<-p.persistSyncCh
	}
	return
}

func (p *Fixer) createWorker(ctx context.Context, workerid int) func() {
	return func() {
		p.worker(ctx, workerid)
	}
}

func (p *Fixer) worker(ctx context.Context, id int) {
	if log.IsDebugEnabled() {
		log.LogDebugf("Fixer[%v] [Worker=%v] started", p.partitionID, id)
	}
	var fragment *Fragment
	defer func() {
		if fragment != nil {
			p.pushFragmentToQueue(fragment)
		}
		atomic.AddInt32(&p.workers, -1)
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] [Worker=%v] exit", p.partitionID, id)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		fragment = p.pickFragmentFromQueue()
		if fragment == nil {
			// 已不存在还需修复的数据段落, worker可以退出
			return
		}
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] [Worker=%v] start to fix %v", p.partitionID, id, fragment)
		}
		var start = time.Now()
		var result = p.checkAndFixFragment(fragment)
		if result == Retry {
			log.LogErrorf("Fixer[%v] [Worker=%v] can not fix %v temporary and will be retry later", p.partitionID, id, fragment)
			// 归还队列以重试
			p.pushFragmentToQueue(fragment)
			fragment = nil
			continue
		}
		if result == Failed {
			// 该数据片段无法修复，进行报警
			log.LogCriticalf("Fixer[%v] can not fixes %v", p.partitionID, fragment)
			exporter.Warning(fmt.Sprintf("CAN NOT FIX BROKEN EXTENT!\n"+
				"Found issue data fragment cause server fault and can not fix it.\n"+
				"Partition: %v\n"+
				"Extent: %v\n"+
				"Offset: %v\n"+
				"Size: %v",
				p.partitionID, fragment.ExtentID, fragment.Offset, fragment.Size))
			// 确定无法修复的不在归还队列进行重试。
			fragment = nil
			continue
		}
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] [Worker=%v] fixed %v, elapsed %v", p.partitionID, id, fragment, time.Now().Sub(start))
		}
		var err error
		if err = p.unregisterRisk(fragment); err != nil {
			return
		}
		fragment = nil
		if p.fragmentCount() == 0 {
			if err = p.cleanupFragmentRecords(); err != nil {
				return
			}
		}
	}
}

func (p *Fixer) handleWorkerPanic(i interface{}) {
	// Worker 发生panic，进行报警
	var callstack = string(debug.Stack())
	log.LogCriticalf("Fixer[%v] fix worker occurred panic: %v\n"+
		"Callstack: %v\n", p.partitionID, i, callstack)
	exporter.Warning(fmt.Sprintf("ISSUE PROCESSOR WORKER PANIC!\n"+
		"Fix worker occurred panic and stopped:\n"+
		"Partition: %v\n"+
		"Message  : %v\n",
		p.partitionID, i))
	return
}

func (p *Fixer) pickFragmentFromQueue() (fragment *Fragment) {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	var element = p.queue.Front()
	if element == nil {
		return
	}
	fragment = element.Value.(*Fragment)
	p.queue.Remove(element)
	return
}

func (p *Fixer) pushFragmentToQueue(fragments ...*Fragment) {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	for _, fragment := range fragments {
		p.queue.PushBack(fragment)
	}
}

func (p *Fixer) copyFragments() []*Fragment {
	var fragments []*Fragment
	p.indexesMu.RLock()
	defer p.indexesMu.RUnlock()
	fragments = make([]*Fragment, 0, len(p.indexes))
	for _, index := range p.indexes {
		fragments = append(fragments, index.fragment)
	}
	return fragments
}

func (p *Fixer) fragmentCount() int {
	p.indexesMu.RLock()
	defer p.indexesMu.RUnlock()
	return len(p.indexes)
}

func (p *Fixer) computeLocalFingerprint(fragment *Fragment) (fgp storage.Fingerprint, err error) {
	fgp, err = p.storage.Fingerprint(fragment.ExtentID, int64(fragment.Offset), int64(fragment.Size), true)
	switch {
	case err == proto.ExtentNotFoundError,
		os.IsNotExist(err),
		err == io.EOF,
		err != nil && strings.Contains(err.Error(), "parameter mismatch"):
		return fgp, nil
	case err != nil:
		return fgp, err
	default:
	}
	return fgp, nil
}

func (p *Fixer) checkLocalExists(fragment *Fragment) (exist bool, err error) {
	if !p.storage.IsExists(fragment.ExtentID) {
		return false, nil
	}
	var localSize uint64
	if proto.IsTinyExtent(fragment.ExtentID) {
		if localSize, err = p.storage.TinyExtentGetFinfoSize(fragment.ExtentID); err != nil {
			return false, err
		}
	} else {
		var ei *storage.ExtentInfoBlock
		if ei, err = p.storage.Watermark(fragment.ExtentID); err != nil {
			return false, err
		}
		localSize = ei[storage.Size]
	}
	if localSize < fragment.Offset+fragment.Size {
		return false, nil
	}
	return true, nil
}

func (p *Fixer) computeLocalCRC(fragment *Fragment) (crc uint32, err error) {
	var (
		extentID   = fragment.ExtentID
		offset     = fragment.Offset
		size       = fragment.Size
		buf        = make([]byte, unit.BlockSize)
		remain     = int64(size)
		ieee       = crc32.NewIEEE()
		readOffset = int64(offset)
	)

	for remain > 0 {
		var readSize = int64(math.Min(float64(remain), float64(unit.BlockSize)))
		err = p.limiter(context.Background(), proto.OpExtentRepairReadToComputeCrc_, uint32(readSize), multirate.FlowDisk)
		if err != nil {
			return
		}
		_, err = p.storage.Read(extentID, readOffset, readSize, buf[:readSize], false)
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] read storage: extent=%v, offset=%v, size=%v, error=%v",
				p.partitionID, extentID, readOffset, readSize, err)
		}
		switch {
		case err == proto.ExtentNotFoundError,
			os.IsNotExist(err),
			err == io.EOF,
			err != nil && strings.Contains(err.Error(), "parameter mismatch"):
			return 0, nil
		case err != nil:
			return 0, err
		default:
		}
		if _, err = ieee.Write(buf[:readSize]); err != nil {
			return 0, err
		}
		readOffset += readSize
		remain -= readSize
	}
	return ieee.Sum32(), nil
}

func (p *Fixer) fetchRemoveFingerprint(host string, extentID, offset, size uint64, force bool) (fgp storage.Fingerprint, rejected bool, err error) {
	var request = repl.NewPacketToFingerprint(context.Background(), &repl.FingerprintRequest{
		PartitionID: p.partitionID,
		ExtentID:    extentID,
		Offset:      int64(offset),
		Size:        int64(size),
		Force:       force,
	})
	var conn net.Conn
	if conn, err = p.connPool.GetConnect(host); err != nil {
		return
	}
	defer func() {
		p.connPool.PutConnect(conn, err != nil)
	}()

	if err = request.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		return
	}

	if err = request.ReadFromConn(conn, proto.ReadDeadlineTime); err != nil {
		return
	}

	if request.ResultCode != proto.OpOk {
		var msg = string(request.Data[:request.Size])
		switch {
		case strings.Contains(msg, proto.ExtentNotFoundError.Error()),
			strings.Contains(msg, io.EOF.Error()),
			strings.Contains(msg, "parameter mismatch"):
			return fgp, false, nil
		case strings.Contains(msg, proto.ErrOperationDisabled.Error()):
			return fgp, true, nil
		default:
		}
		return fgp, false, errors.New(msg)
	}

	fgp.DecodeBinary(request.Data[:request.Size])
	return fgp, false, nil
}

func (p *Fixer) fetchRemoteDataTo(host string, extentID, offset, size uint64, force bool, wat io.WriterAt) (n int64, rejected bool, err error) {

	var readOffset = int(offset)
	var readSize = int(size)
	var remain = int64(size)
	request := repl.NewExtentRepairReadPacket(context.Background(), p.partitionID, extentID, readOffset, readSize, force)
	if proto.IsTinyExtent(extentID) {
		request = repl.NewTinyExtentRepairReadPacket(context.Background(), p.partitionID, extentID, readOffset, readSize, force)
	}
	var conn net.Conn
	if conn, err = p.connPool.GetConnect(host); err != nil {
		return
	}
	defer p.connPool.PutConnect(conn, true)

	if err = request.WriteToConn(conn, proto.WriteDeadlineTime); err != nil {
		return
	}
	var fileOffset int64 = 0

	var buf = make([]byte, unit.BlockSize)
	var getReplyDataBuffer = func(size uint32) []byte {
		if int(size) > cap(buf) {
			return make([]byte, size)
		}
		return buf[:size]
	}

	for remain > 0 {
		reply := repl.NewPacket(context.Background())
		if err = reply.ReadFromConnWithSpecifiedDataBuffer(conn, 60, getReplyDataBuffer); err != nil {
			return
		}

		if reply.ResultCode != proto.OpOk {
			var msg = string(reply.Data[:reply.Size])
			switch {
			case strings.Contains(msg, proto.ExtentNotFoundError.Error()),
				strings.Contains(msg, io.EOF.Error()),
				strings.Contains(msg, "parameter mismatch"):
				return 0, false, nil
			case strings.Contains(msg, proto.ErrOperationDisabled.Error()):
				return 0, true, nil
			default:
			}
			return 0, false, errors.New(msg)
		}

		// Write it to local extent file
		var writeSize = int64(reply.Size)
		if proto.IsTinyExtent(extentID) {
			if isEmptyResponse := len(reply.Arg) > 0 && reply.Arg[0] == emptyResponse; isEmptyResponse {
				if reply.KernelOffset > 0 && reply.KernelOffset != uint64(crc32.ChecksumIEEE(reply.Arg)) {
					return 0, false, errors.New("CRC mismatch")
				}
				writeSize = int64(binary.BigEndian.Uint64(reply.Arg[1:9]))
				fileOffset += writeSize
				remain -= writeSize
				n += writeSize
				continue
			}
		}
		if _, err = wat.WriteAt(reply.Data[:reply.Size], fileOffset); err != nil {
			return 0, false, err
		}
		fileOffset += int64(reply.Size)
		remain -= int64(reply.Size)
		n += int64(reply.Size)
	}
	return n, false, nil
}

func (p *Fixer) applyTempFileToExtent(f *os.File, extentID, offset, size uint64) (err error) {

	var (
		tempFileOffset int64
		extentOffset   = int64(offset)
		buf            = make([]byte, unit.BlockSize)
		remain         = int64(size)
	)
	for remain > 0 {
		var readSize = remain
		if proto.IsTinyExtent(extentID) {
			var nextDataOff int64
			if nextDataOff, err = p.getFileNextDataPos(f, tempFileOffset); err != nil {
				return
			}
			if nextDataOff != tempFileOffset {
				var holeSize = nextDataOff - tempFileOffset
				remain -= holeSize
				tempFileOffset += holeSize
				extentOffset += holeSize
				continue
			}
			var nextHoleOff int64
			if nextHoleOff, err = p.getFileNextHolePos(f, tempFileOffset); err != nil {
				return
			}
			if nextHoleOff != tempFileOffset {
				readSize = int64(math.Min(float64(readSize), float64(nextHoleOff-tempFileOffset)))
			}
		}
		readSize = int64(math.Min(float64(readSize), float64(unit.BlockSize)))
		if _, err = f.ReadAt(buf[:readSize], tempFileOffset); err != nil {
			return
		}
		var crc = crc32.ChecksumIEEE(buf[:readSize])
		err = p.limiter(context.Background(), proto.OpExtentRepairWrite_, uint32(readSize), multirate.FlowDisk)
		if err != nil {
			return
		}
		if err = p.storage.Write(context.Background(), extentID, extentOffset, readSize, buf[:readSize], crc, storage.RandomWriteType, false); err != nil {
			return
		}
		remain -= readSize
		tempFileOffset += readSize
		extentOffset += readSize
	}
	return
}

func (p *Fixer) getFileNextDataPos(f *os.File, offset int64) (nextDataOffset int64, err error) {
	const (
		SEEK_DATA = 3
	)
	nextDataOffset, err = f.Seek(offset, SEEK_DATA)
	defer func() {
		if err != nil && strings.Contains(err.Error(), syscall.ENXIO.Error()) {
			nextDataOffset = offset
			err = nil
		}
	}()
	if err != nil {
		return
	}
	return
}

func (p *Fixer) getFileNextHolePos(f *os.File, offset int64) (nextHoleOffset int64, err error) {
	const (
		SEEK_HOLE = 4
	)
	nextHoleOffset, err = f.Seek(offset, SEEK_HOLE)
	defer func() {
		if err != nil && strings.Contains(err.Error(), syscall.ENXIO.Error()) {
			nextHoleOffset = offset
			err = nil
		}
	}()
	if err != nil {
		return
	}
	return
}

func (p *Fixer) createRepairTmpFile(host string, extentID, offset, size uint64) (f *os.File, err error) {
	var repairTempPath = libpath.Join(p.path, ".temp")
	if err = os.MkdirAll(repairTempPath, 0777); err != nil {
		return
	}
	var repairTempFilepath = libpath.Join(repairTempPath, fmt.Sprintf("%v_%v_%v_%v", extentID, offset, size, host))
	if f, err = os.OpenFile(repairTempFilepath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0666); err != nil {
		return
	}
	return
}

func (p *Fixer) checkAndFixFragment(fragment *Fragment) FixResult {
	err := multirate.WaitConcurrency(context.Background(), proto.OpExtentRepairWrite_, p.diskPath)
	if err != nil {
		return Retry
	}
	defer multirate.DoneConcurrency(proto.OpExtentRepairWrite_, p.diskPath)

	var remoteHosts = p.getRemotes()
	var haType = p.getHAType()

	for _, handler := range p.getHandlers(remoteHosts, haType, fragment) {
		var result = handler.handle()
		log.LogWarnf("Fixer: Handler(%v) fixed Partition(%v)_Extent(%v)_Offset(%v)_Size(%v) response %v",
			handler.name, p.partitionID, fragment.ExtentID, fragment.Offset, fragment.Size, result)
		if result == Success || result == Retry {
			return result
		}
	}

	// 所有策略均无法修复目标
	log.LogErrorf("Fixer: all handlers fixes Partition(%v)_Extent(%v)_Offset(%v)_Size(%v) response Failed",
		p.partitionID, fragment.ExtentID, fragment.Offset, fragment.Size)
	return Failed
}

func (p *Fixer) initFragments() (err error) {
	var release = p.lockPersist()
	defer release()
	if err = p.checkFp(false); err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}
	var info os.FileInfo
	if info, err = p.persistFp.Stat(); err != nil {
		return
	}
	var filesize int64
	if filesize = info.Size(); filesize%fragmentBinaryLength != 0 {
		filesize = (filesize / fragmentBinaryLength) * fragmentBinaryLength
		if err = p.persistFp.Truncate(filesize); err != nil {
			return
		}
	}
	if p.codecReuseBuf == nil || len(p.codecReuseBuf) < fragmentBinaryLength {
		p.codecReuseBuf = make([]byte, fragmentBinaryLength)
	}
	var bufR = bufio.NewReader(p.persistFp)
	var n int
	var offset int64 = 0
	var indexes = make([]*fragmentIndex, 0, filesize/fragmentBinaryLength)
	for {
		n, err = io.ReadFull(bufR, p.codecReuseBuf)
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			return
		}
		if n != fragmentBinaryLength {
			err = ErrBrokenFragmentsFile
			return
		}
		if reflect.DeepEqual(p.codecReuseBuf[:n], emptyFragmentBinary) {
			offset += int64(n)
			continue
		}
		var fragment = new(Fragment)
		if err = fragment.DecodeFrom(p.codecReuseBuf[:n]); err != nil {
			return
		}
		if fragment.Empty() {
			offset += int64(n)
			continue
		}
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] loaded %v from persisted data", p.partitionID, fragment)
		}
		indexes = append(indexes, &fragmentIndex{
			fragment: fragment,
			offset:   offset,
		})
		offset += int64(n)
	}

	p.indexesMu.Lock()
	p.indexes = indexes
	p.indexNextOffset = offset
	p.indexesMu.Unlock()

	for _, index := range indexes {
		p.pushFragmentToQueue(index.fragment)
	}
	return
}

func (p *Fixer) removeFromFile(offset int64) (err error) {
	var release = p.lockPersist()
	defer release()
	if err = p.checkFp(false); err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}
	if _, err = p.persistFp.WriteAt(emptyFragmentBinary[:fragmentBinaryLength], offset); err != nil {
		return
	}
	return
}

func (p *Fixer) appendToFile(fragment *Fragment) (offset int64, err error) {
	var release = p.lockPersist()
	defer release()
	if err = p.checkFp(true); err != nil {
		return
	}
	if p.codecReuseBuf == nil || len(p.codecReuseBuf) < fragmentBinaryLength {
		p.codecReuseBuf = make([]byte, fragmentBinaryLength)
	}
	if err = fragment.EncodeTo(p.codecReuseBuf); err != nil {
		return
	}
	offset = p.indexNextOffset
	if _, err = p.persistFp.WriteAt(p.codecReuseBuf[:fragmentBinaryLength], offset); err != nil {
		return
	}
	p.indexNextOffset += fragmentBinaryLength
	return
}

func (p *Fixer) cleanupFragmentRecords() (err error) {
	var release = p.lockPersist()
	defer release()
	defer func() {
		if err == nil {
			p.indexNextOffset = 0
		}
	}()
	if err = p.checkFp(false); err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}
	err = p.persistFp.Truncate(0)
	if log.IsDebugEnabled() {
		log.LogDebugf("Fixer[%v] cleanup records file", p.partitionID)
	}
	_ = p.closeFp()
	return
}

func (p *Fixer) registerRisk(fragment *Fragment) (err error) {
	p.indexesMu.Lock()
	defer p.indexesMu.Unlock()
	for i := 0; i < len(p.indexes); i++ {
		if p.indexes[i].fragment.Equals(fragment) {
			return
		}
	}
	var offset int64
	if offset, err = p.appendToFile(fragment); err != nil {
		return
	}
	p.indexes = append(p.indexes, &fragmentIndex{
		fragment: fragment,
		offset:   offset,
	})
	if log.IsDebugEnabled() {
		log.LogDebugf("Fixer[%v] registered risk %v", p.partitionID, fragment)
	}
	return
}

func (p *Fixer) unregisterRisk(fragment *Fragment) (err error) {
	p.indexesMu.Lock()
	var offsets []int64
	var i = 0
	for i < len(p.indexes) {
		if !p.indexes[i].fragment.Equals(fragment) {
			i++
			continue
		}
		offsets = append(offsets, p.indexes[i].offset)
		switch {
		case len(p.indexes) == i+1:
			p.indexes = p.indexes[:i]
		case i == 0:
			p.indexes = p.indexes[1:]
		default:
			p.indexes = append(p.indexes[:i], p.indexes[i+1:]...)
		}
	}
	p.indexesMu.Unlock()

	for _, offset := range offsets {
		if err = p.removeFromFile(offset); err != nil {
			return
		}
	}
	if log.IsDebugEnabled() {
		log.LogDebugf("Fixer[%v] unregistered risk %v", p.partitionID, fragment)
	}
	return
}

func (p *Fixer) FindOverlap(extentID, offset, size uint64) bool {
	if len(p.indexes) == 0 {
		return false
	}
	p.indexesMu.RLock()
	defer p.indexesMu.RUnlock()
	for i := 0; i < len(p.indexes); i++ {
		if p.indexes[i].fragment.Overlap(extentID, offset, size) {
			return true
		}
	}
	return false
}

func (p *Fixer) checkFp(create bool) (err error) {
	if p.persistFp == nil {
		var fp *os.File
		var flag = os.O_RDWR
		if create {
			flag |= os.O_CREATE
		}
		if fp, err = os.OpenFile(libpath.Join(p.path, fragmentsFilename), flag, os.ModePerm); err != nil {
			return
		}
		p.persistFp = fp
	}
	return
}

func (p *Fixer) closeFp() (err error) {
	if p.persistFp != nil {
		err = p.persistFp.Close()
		p.persistFp = nil
	}
	return
}

func NewFixer(partitionID uint64, path string, storage *storage.ExtentStore, getRemotes GetRemotesFunc, getHAType GetHATypeFunc,
	fragments []*Fragment, connPool *connman.ConnManager, diskPath string, limiter LimiterFunc) (*Fixer, error) {
	var err error
	var p = &Fixer{
		partitionID:   partitionID,
		path:          path,
		storage:       storage,
		getRemotes:    getRemotes,
		getHAType:     getHAType,
		persistSyncCh: make(chan struct{}, 1),
		queue:         list.New(),
		indexes:       make([]*fragmentIndex, 0, 16),
		codecReuseBuf: make([]byte, fragmentBinaryLength),
		connPool:      connPool,
		limiter:       limiter,
		persistFp:     nil,
		diskPath:      diskPath,
	}
	if err = p.initFragments(); err != nil {
		return nil, err
	}
	for _, fragment := range fragments {
		if err = p.registerRisk(fragment); err != nil {
			return nil, err
		}
		p.pushFragmentToQueue(fragment)
	}
	if p.fragmentCount() == 0 {
		_ = p.cleanupFragmentRecords()
	}
	return p, nil
}

func (p *Fixer) getHandlers(remotes []string, hat proto.CrossRegionHAType, fragment *Fragment) []fixHandler {
	var handlers []fixHandler
	if !proto.IsTinyExtent(fragment.ExtentID) {
		handlers = append(handlers, p.getFastHandlers(remotes, hat, fragment)...)
	}
	handlers = append(handlers, p.getStdHandlers(remotes, hat, fragment)...)
	return handlers
}

func (p *Fixer) getFastHandlers(remotes []string, hat proto.CrossRegionHAType, fragment *Fragment) []fixHandler {
	var (
		getLocalFGPOnce = new(sync.Once)
		localFGP        storage.Fingerprint
		localFGPError   error
	)
	var getLocalFGP = func() (storage.Fingerprint, error) {
		getLocalFGPOnce.Do(func() {
			localFGP, localFGPError = p.computeLocalFingerprint(fragment)
			if log.IsDebugEnabled() {
				log.LogDebugf("Fixer[%v] compute local fingerprint %v: fgp=%v, error=%v", p.partitionID, fragment, localFGP.String(), localFGPError)
			}
		})
		return localFGP, localFGPError
	}
	return []fixHandler{
		{
			name: "FastTrust",
			handle: func() FixResult {
				return p.fixByFastTrustPolicy(remotes, hat, getLocalFGP, fragment)
			},
		},
	}
}

func (p *Fixer) getStdHandlers(remotes []string, hat proto.CrossRegionHAType, fragment *Fragment) []fixHandler {
	var (
		getLocalCRCOnce = new(sync.Once)
		localCRC        uint32
		localCRCError   error
	)
	var getLocalCRC = func() (uint32, error) {
		getLocalCRCOnce.Do(func() {
			localCRC, localCRCError = p.computeLocalCRC(fragment)
			if log.IsDebugEnabled() {
				log.LogDebugf("Fixer[%v] compute local CRC %v: crc=%v, error=%v", p.partitionID, fragment, localCRC, localCRCError)
			}
		})
		return localCRC, localCRCError
	}
	return []fixHandler{
		{
			name: "StdTrust",
			handle: func() FixResult {
				return p.fixByStdTrustPolicy(remotes, hat, getLocalCRC, fragment)
			},
		},
		{
			name: "StdQuorum",
			handle: func() FixResult {
				return p.fixByStdQuorumPolicy(remotes, hat, getLocalCRC, fragment)
			},
		},
	}
}

func (p *Fixer) fixByFastTrustPolicy(hosts []string, hat proto.CrossRegionHAType, getLocalFGP func() (storage.Fingerprint, error), fragment *Fragment) FixResult {
	var (
		extentID = fragment.ExtentID
		offset   = fragment.Offset
		size     = fragment.Size

		rejects    int
		failures   int
		unsupports int
	)

	if proto.IsTinyExtent(extentID) {
		return Failed
	}

	var err error
	var localFGP storage.Fingerprint
	if localFGP, err = getLocalFGP(); err != nil {
		return Retry
	}
	if localFGP.Empty() {
		return Success
	}

	var start time.Time
	for _, host := range hosts {
		var remoteFgp storage.Fingerprint
		var rejected bool
		var err error
		start = time.Now()
		remoteFgp, rejected, err = p.fetchRemoveFingerprint(host, extentID, offset, size, false)
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] fetched remote FGP %v: host=%v, fgp=%v, rejected=%v, error=%v, elapsed=%v", p.partitionID, fragment, host, remoteFgp.String(), rejected, err, time.Now().Sub(start))
		}
		if err != nil {
			if strings.Contains(err.Error(), repl.ErrorUnknownOp.Error()) {
				unsupports++
			}
			failures++
			continue
		}
		if rejected {
			rejects++
			continue
		}
		if remoteFgp.Empty() {
			if hat == proto.CrossRegionHATypeQuorum {
				continue
			}
			return Success
		}
		if localFGP.Equals(remoteFgp) {
			return Success
		}

		var firstConflict = localFGP.FirstConflict(remoteFgp)
		var issueStartBlkNo = int(offset / unit.BlockSize)
		var conflictStartBlkNo = issueStartBlkNo + firstConflict
		var conflictOffset = uint64(math.Max(float64(uint64(conflictStartBlkNo)*unit.BlockSize), float64(offset)))
		var conflictSize = (offset + size) - conflictOffset
		var extentStartOffset = int64(conflictOffset)

		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] computed conflict %v: offset=%v, size=%v", p.partitionID, fragment, conflictOffset, conflictSize)
		}

		var writeAtExtent WriterAtFunc = func(b []byte, off int64) (n int, err error) {
			var extentWriteOffset = extentStartOffset + off
			var extentWriteSize = int64(len(b))
			var crc = crc32.ChecksumIEEE(b)
			err = p.limiter(context.Background(), proto.OpExtentRepairWrite_, uint32(extentWriteSize), multirate.FlowDisk)
			if err != nil {
				return
			}
			err = p.storage.Write(context.Background(), extentID, extentWriteOffset, extentWriteSize, b, crc, storage.RandomWriteType, false)
			n = int(extentWriteSize)
			return
		}
		start = time.Now()
		var n int64
		n, rejected, err = p.fetchRemoteDataTo(host, extentID, conflictOffset, conflictSize, false, writeAtExtent)
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] fetched remote data %v: host=%v, bytes=%v, rejected=%v, error=%v, elapsed=%v", p.partitionID, fragment, host, n, rejected, err, time.Now().Sub(start))
		}
		if err != nil {
			failures++
			continue
		}
		if rejected {
			rejects++
			continue
		}
		if n < int64(size) && hat == proto.CrossRegionHATypeQuorum {
			continue
		}
		return Success
	}
	if failures > 0 {
		// 存在失败远端，稍后重试
		if failures == unsupports {
			// 所有远端副本均为老版本不支持该操作
			return Failed
		}
		return Retry
	}
	if rejects == 0 {
		// 所有远端均汇报无数据, 无需修复
		return Success
	}
	// 当前修复策略无法认定数据是否安全及进行修复, 由下一个策略继续处理
	log.LogErrorf("Fixer: Partition(%v)_Extent(%v)_Offset(%v)_Size(%v) can not determine correct data by FastTrustPolicy",
		p.partitionID, extentID, offset, size)
	return Failed
}

// 使用可信副本策略对指定数据片段进行修复
func (p *Fixer) fixByStdTrustPolicy(hosts []string, hat proto.CrossRegionHAType, _ func() (uint32, error), fragment *Fragment) FixResult {
	var (
		err       error
		extentID  = fragment.ExtentID
		offset    = fragment.Offset
		size      = fragment.Size
		rejects   int
		failures  []error
		tempFiles = make([]*os.File, 0, len(hosts))
	)

	defer func() {
		for _, tempFile := range tempFiles {
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name())
		}
	}()

	var exists bool
	exists, err = p.checkLocalExists(fragment)
	if err != nil {
		return Retry
	}
	if !exists {
		return Success
	}

	var start time.Time
	for _, host := range hosts {
		var extentStartOffset = int64(offset)
		var writeAtExtent WriterAtFunc = func(b []byte, off int64) (n int, err error) {
			var extentWriteOffset = extentStartOffset + off
			var extentWriteSize = int64(len(b))
			var crc = crc32.ChecksumIEEE(b)
			err = p.limiter(context.Background(), proto.OpExtentRepairWrite_, uint32(extentWriteSize), multirate.FlowDisk)
			if err != nil {
				return
			}
			err = p.storage.Write(context.Background(), extentID, extentWriteOffset, extentWriteSize, b, crc, storage.RandomWriteType, false)
			if log.IsDebugEnabled() {
				log.LogDebugf("Fixer[%v] write data to local storage: extent=%v, offset=%v, size=%v", p.partitionID, extentID, extentWriteOffset, extentWriteSize)
			}
			n = int(extentWriteSize)
			return
		}
		start = time.Now()
		var n, rejected, fetchedErr = p.fetchRemoteDataTo(host, extentID, offset, size, false, writeAtExtent)
		if log.IsDebugEnabled() {
			log.LogDebugf("Fixer[%v] fetched remote data %v: host=%v, bytes=%v, rejected=%v, error=%v, elapsed=%v", p.partitionID, fragment, host, n, rejected, err, time.Now().Sub(start))
		}
		if fetchedErr != nil {
			failures = append(failures, fmt.Errorf("fetch extent data (partition=%v, extent=%v, offset=%v, size=%v) from %v failed: %v",
				p.partitionID, extentID, offset, size, host, fetchedErr))
			continue
		}
		if rejected {
			rejects++
			continue
		}
		if n < int64(size) && hat == proto.CrossRegionHATypeQuorum {
			continue
		}
		return Success
	}
	if len(failures) > 0 {
		// 存在失败远端，稍后重试
		return Retry
	}
	if rejects == 0 {
		// 所有远端均汇报无数据, 无需修复
		return Success
	}
	// 当前修复策略无法认定数据是否安全及进行修复, 由下一个策略继续处理
	log.LogErrorf("Fixer: Partition(%v)_Extent(%v)_Offset(%v)_Size(%v) can not determine correct data by TrustPolicy",
		p.partitionID, extentID, offset, size)
	return Failed
}

// 使用超半数版本策略对制定数据片段进行修复
func (p *Fixer) fixByStdQuorumPolicy(hosts []string, _ proto.CrossRegionHAType, getLocalCRC func() (uint32, error), fragment *Fragment) FixResult {
	var (
		err              error
		extentID         = fragment.ExtentID
		offset           = fragment.Offset
		size             = fragment.Size
		failures         int
		versions         = make(map[uint32][]*os.File) // crc -> temp files
		tempFiles        = make([]*os.File, 0, len(hosts))
		registerTempFile = func(f *os.File) {
			tempFiles = append(tempFiles, f)
		}
	)

	defer func() {
		for _, tempFile := range tempFiles {
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name())
		}
	}()

	var localCRC uint32
	if localCRC, err = getLocalCRC(); err != nil {
		return Retry
	}
	if localCRC == 0 {
		return Success
	}

	// 从远端收集数据
	for _, host := range hosts {
		var tempFile *os.File
		if tempFile, err = p.createRepairTmpFile(host, extentID, offset, size); err != nil {
			return Retry
		}
		registerTempFile(tempFile)
		var hash = crc32.NewIEEE()
		var writeAtTempFile WriterAtFunc = func(b []byte, off int64) (n int, err error) {
			_, _ = hash.Write(b)
			n, err = tempFile.WriteAt(b, off)
			return
		}
		var _, _, fetchedErr = p.fetchRemoteDataTo(host, extentID, offset, size, true, writeAtTempFile)
		if fetchedErr != nil {
			failures++
			continue
		}
		var crc = hash.Sum32()
		versions[crc] = append(versions[crc], tempFile)
	}
	var quorum = (len(hosts)+1)/2 + 1
	for crc, files := range versions {
		if len(files) >= quorum {
			// 找到了超半数版本, 使用该版本数据
			if crc != 0 && crc != localCRC {
				// 仅在目标数据非空洞且有效长度超过本地数据的情况下进行修复
				if err = p.applyTempFileToExtent(files[0], extentID, offset, size); err != nil {
					// 覆盖本地数据时出错，延迟修复
					log.LogErrorf("Fixer: Partition(%v)_Extent(%v)_Offset(%v)_Size(%v) fix CRC(%v -> %v) failed: %v", p.partitionID, extentID, offset, size, localCRC, crc, err)
					return Retry
				}
			}
			log.LogWarnf("Fixer: Partition(%v)_Extent(%v)_Offset(%v)_Size(%v) skip fix cause quorum version same as local or empty.", p.partitionID, extentID, offset, size)
			return Success
		}
	}
	log.LogErrorf("Fixer: Partition(%v)_Extent(%v)_Offset(%v)_Size(%v) can not determine correct data by quorum",
		p.partitionID, extentID, offset, size)
	return Failed
}
