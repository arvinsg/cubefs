package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/cubefs/cubefs/util/log"
	"github.com/stretchr/testify/assert"
	"hash/crc32"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/async"
	"github.com/cubefs/cubefs/util/testutil"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func RandStringRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func createFile(name string, data []byte) (err error) {
	os.RemoveAll(name)
	fp, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0777)
	if err != nil {
		return
	}
	fp.Write(data)
	fp.Close()
	return
}

func removeFile(name string) {
	os.RemoveAll(name)
}

func computeMd5(data []byte) string {
	h := md5.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func compareMd5(s *ExtentStore, extent, offset, size uint64, expectMD5 string) (err error) {
	actualMD5, err := s.ComputeMd5Sum(extent, offset, size)
	if err != nil {
		return fmt.Errorf("ComputeMd5Sum failed on extent(%v),"+
			"offset(%v),size(%v) expect(%v) actual(%v) err(%v)", extent, offset, size, expectMD5, actualMD5, err)
	}
	if actualMD5 != expectMD5 {
		return fmt.Errorf("ComputeMd5Sum failed on extent(%v),"+
			"offset(%v),size(%v) expect(%v) actual(%v) err(%v)", extent, offset, size, expectMD5, actualMD5, err)
	}
	return nil
}

func TestExtentStore_ComputeMd5Sum(t *testing.T) {
	allData := []byte(RandStringRunes(100 * 1024))
	allMd5 := computeMd5(allData)
	dataPath := "/tmp"
	extentID := 3675
	err := createFile(path.Join(dataPath, strconv.Itoa(extentID)), allData)
	if err != nil {
		t.Logf("createFile failed %v", err)
		t.FailNow()
	}
	s := new(ExtentStore)
	s.dataPath = dataPath
	err = compareMd5(s, uint64(extentID), 0, 0, allMd5)
	if err != nil {
		t.Logf("compareMd5 failed %v", err)
		t.FailNow()
	}

	for i := 0; i < 100; i++ {
		data := allData[i*1024 : (i+1)*1024]
		expectMD5 := computeMd5(data)
		err = compareMd5(s, uint64(extentID), uint64(i*1024), 1024, expectMD5)
		if err != nil {
			t.Logf("compareMd5 failed %v", err)
			t.FailNow()
		}
	}
	removeFile(path.Join(dataPath, strconv.Itoa(extentID)))

}

func TestExtentStore_PlaybackTinyDelete(t *testing.T) {
	const (
		testStoreSize          int    = 128849018880 // 120GB
		testStoreCacheCapacity int    = 1
		testPartitionID        uint64 = 1
		testTinyExtentID       uint64 = 1
		testTinyFileCount      int    = 1024
		testTinyFileSize       int    = PageSize
	)
	var baseTestPath = testutil.InitTempTestPath(t)
	t.Log(baseTestPath.Path())
	defer func() {
		baseTestPath.Cleanup()
	}()

	var (
		err error
	)

	var store *ExtentStore
	if store, err = NewExtentStore(baseTestPath.Path(), testPartitionID, testStoreSize, testStoreCacheCapacity, nil, false, IOInterceptors{}); err != nil {
		t.Fatalf("init test store failed: %v", err)
	}
	// 准备小文件数据，向TinyExtent 1写入1024个小文件数据, 每个小文件size为1024.
	var (
		tinyFileData    = make([]byte, testTinyFileSize)
		testFileDataCrc = crc32.ChecksumIEEE(tinyFileData[:testTinyFileSize])
		holes           = make([]*proto.TinyExtentHole, 0)
	)
	for i := 0; i < testTinyFileCount; i++ {
		off := int64(i * PageSize)
		size := int64(testTinyFileSize)
		if err = store.Write(context.Background(), testTinyExtentID, off, size,
			tinyFileData[:testTinyFileSize], testFileDataCrc, AppendWriteType, false); err != nil {
			t.Fatalf("prepare tiny data [index: %v, off: %v, size: %v] failed: %v", i, off, size, err)
		}
		if i%2 == 0 {
			continue
		}
		if err = store.RecordTinyDelete(testTinyExtentID, off, size); err != nil {
			t.Fatalf("write tiny delete record [index: %v, off: %v, size: %v] failed: %v", i, off, size, err)
		}
		holes = append(holes, &proto.TinyExtentHole{
			Offset: uint64(off),
			Size:   uint64(size),
		})
	}
	// 执行TinyDelete回放
	if err = store.PlaybackTinyDelete(0); err != nil {
		t.Fatalf("playback tiny delete failed: %v", err)
	}
	// 验证回放后测试TinyExtent的洞是否可预期一致
	var (
		testTinyExtent *Extent
		newOffset      int64
	)
	if testTinyExtent, err = store.extentWithHeaderByExtentID(testTinyExtentID); err != nil {
		t.Fatalf("load test tiny extent %v failed: %v", testTinyExtentID, err)
	}
	for _, hole := range holes {
		newOffset, _, err = testTinyExtent.tinyExtentAvaliOffset(int64(hole.Offset))
		if err != nil && strings.Contains(err.Error(), syscall.ENXIO.Error()) {
			newOffset = testTinyExtent.dataSize
			err = nil
		}
		if err != nil {
			t.Fatalf("check tiny extent avali offset failed: %v", err)
		}
		if hole.Offset+hole.Size != uint64(newOffset) {
			t.Fatalf("punch hole record [offset: %v, size: %v] not applied to extent.", hole.Offset, hole.Size)
		}
	}
}

func TestExtentStore_UsageOnConcurrentModification(t *testing.T) {
	var testPath = testutil.InitTempTestPath(t)
	defer testPath.Cleanup()

	const (
		partitionID       uint64 = 1
		storageSize              = 1 * 1024 * 1024
		cacheCapacity            = 10
		writeWorkers             = 10
		dataSizePreWrite         = 16
		executionDuration        = 30 * time.Second
	)

	var testPartitionPath = path.Join(testPath.Path(), fmt.Sprintf("datapartition_%d", partitionID))
	var storage *ExtentStore
	var err error
	if storage, err = NewExtentStore(testPartitionPath, partitionID, storageSize, cacheCapacity,
		func(event CacheEvent, e *Extent) {}, true, IOInterceptors{}); err != nil {
		t.Fatalf("Create extent store failed: %v", err)
		return
	}

	storage.Load()
	for {
		if storage.IsFinishLoad() {
			break
		}
		time.Sleep(time.Second)
	}

	var futures = make(map[string]*async.Future, 0)
	var dataSize = int64(dataSizePreWrite)
	var data = make([]byte, dataSize)
	var dataCRC = crc32.ChecksumIEEE(data)
	var ctx, cancel = context.WithCancel(context.Background())

	var handleWorkerPanic async.PanicHandler = func(i interface{}) {
		t.Fatalf("unexpected panic orrcurred: %v\nCallStack:\n%v", i, string(debug.Stack()))
	}

	// Start workers to write extents
	for i := 0; i < writeWorkers; i++ {
		var future = async.NewFuture()
		var worker async.ParamWorkerFunc = func(args ...interface{}) {
			var (
				future     = args[0].(*async.Future)
				ctx        = args[1].(context.Context)
				cancelFunc = args[2].(context.CancelFunc)
				err        error
			)
			defer func() {
				if err != nil {
					cancelFunc()
				}
				future.Respond(nil, err)
			}()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				var extentID uint64
				if extentID, err = storage.NextExtentID(); err != nil {
					return
				}
				if err = storage.Create(extentID, 0, true); err != nil {
					return
				}
				var offset int64 = 0
				for offset+int64(dataSize) <= 64*1024*1024 {
					select {
					case <-ctx.Done():
						return
					default:
					}
					if err = storage.Write(context.Background(), extentID, offset, dataSize, data, dataCRC, AppendWriteType, false); err != nil {
						err = nil
						break
					}
					offset += dataSize
				}
			}
		}
		async.ParamWorker(worker, handleWorkerPanic).RunWith(future, ctx, cancel)
		futures[fmt.Sprintf("WriteWorker-%d", i)] = future
	}

	// Start extents delete worker
	{
		var future = async.NewFuture()
		var worker async.ParamWorkerFunc = func(args ...interface{}) {
			var (
				future     = args[0].(*async.Future)
				ctx        = args[1].(context.Context)
				cancelFunc = args[2].(context.CancelFunc)
				err        error
			)
			defer func() {
				if err != nil {
					cancelFunc()
				}
				future.Respond(nil, err)
			}()
			var batchDeleteTicker = time.NewTicker(1 * time.Second)
			for {
				select {
				case <-batchDeleteTicker.C:
				case <-ctx.Done():
					return
				}
				var eibs []ExtentInfoBlock
				if eibs, err = storage.GetAllWatermarks(proto.NormalExtentType, nil); err != nil {
					return
				}
				if _, err = storage.GetAllExtentInfoWithByteArr(ExtentFilterForValidateCRC()); err != nil {
					return
				}
				for _, eib := range eibs {
					_ = storage.MarkDelete(SingleMarker(0, eib[FileID], 0, 0))
				}
			}
		}
		async.ParamWorker(worker, handleWorkerPanic).RunWith(future, ctx, cancel)
		futures["DeleteWorker"] = future
	}

	// Start control worker
	{
		var future = async.NewFuture()
		var worker async.ParamWorkerFunc = func(args ...interface{}) {
			var (
				future     = args[0].(*async.Future)
				ctx        = args[1].(context.Context)
				cancelFunc = args[2].(context.CancelFunc)
			)
			defer func() {
				future.Respond(nil, nil)
			}()
			var startTime = time.Now()
			var stopTimer = time.NewTimer(executionDuration)
			var displayTicker = time.NewTicker(time.Second * 10)
			for {
				select {
				case <-stopTimer.C:
					t.Logf("Execution finish.")
					cancelFunc()
					return
				case <-displayTicker.C:
					t.Logf("Execution time: %.0fs.", time.Now().Sub(startTime).Seconds())
				case <-ctx.Done():
					t.Logf("Execution aborted.")
					return
				}
			}
		}
		async.ParamWorker(worker, handleWorkerPanic).RunWith(future, ctx, cancel)
		futures["ControlWorker"] = future
	}

	for name, future := range futures {
		if _, err = future.Response(); err != nil {
			t.Fatalf("%v respond error: %v", name, err)
		}
	}
	_, _, _ = storage.FlushDelete(NewFuncInterceptor(nil, nil), 0)

	// 结果检查
	{
		// 计算本地文件系统中实际Size总和
		var actualNormalExtentTotalUsed int64
		var files []os.FileInfo
		var extentFileRegexp = regexp.MustCompile("^(\\d)+$")
		if files, err = ioutil.ReadDir(testPartitionPath); err != nil {
			t.Fatalf("Stat test partition path %v failed, error message: %v", testPartitionPath, err)
		}
		for _, file := range files {
			if extentFileRegexp.Match([]byte(file.Name())) {
				actualNormalExtentTotalUsed += file.Size()
			}
		}

		var eibs []ExtentInfoBlock
		if eibs, err = storage.GetAllWatermarks(proto.AllExtentType, nil); err != nil {
			t.Fatalf("Get extent info from storage failed, error message: %v", err)
		}
		var eibTotalUsed int64
		for _, eib := range eibs {
			eibTotalUsed += int64(eib[Size])
		}

		var storeUsedSize = storage.GetStoreUsedSize() // 存储引擎统计的使用Size总和

		if !((actualNormalExtentTotalUsed == eibTotalUsed) && (eibTotalUsed == storeUsedSize)) {
			t.Fatalf("Used size validation failed, actual total used %v, store used size %v, extent info total used %v", actualNormalExtentTotalUsed, storeUsedSize, eibTotalUsed)
		}
	}

	storage.Close()
}

func TestTrashExtent(t *testing.T) {
	var testPath = testutil.InitTempTestPath(t)
	defer testPath.Cleanup()
	_, err := log.InitLog(path.Join(os.TempDir(), t.Name(), "logs"), "datanode_test", log.DebugLevel, nil)
	if err != nil {
		t.Errorf("Init log failed: %v", err)
		return
	}
	defer log.LogFlush()
	const (
		partitionID      uint64 = 1
		storageSize             = 1 * 1024 * 1024
		cacheCapacity           = 10
		dataSizePreWrite        = 16
	)

	var testPartitionPath = path.Join(testPath.Path(), fmt.Sprintf("datapartition_%d", partitionID))
	var storage *ExtentStore
	if storage, err = NewExtentStore(testPartitionPath, partitionID, storageSize, cacheCapacity,
		func(event CacheEvent, e *Extent) {}, true, IOInterceptors{}); err != nil {
		t.Fatalf("Create extent store failed: %v", err)
		return
	}

	storage.Load()
	for {
		if storage.IsFinishLoad() {
			break
		}
		time.Sleep(time.Second)
	}

	var dataSize = int64(dataSizePreWrite)
	var data = make([]byte, dataSize)
	var dataCRC = crc32.ChecksumIEEE(data)
	tStart := time.Now()
	extents := make([]uint64, 0)
	inode := 10
	for i := 0; i < 2; i++ {
		extentID, _ := storage.NextExtentID()
		_ = storage.Create(extentID, uint64(inode), true)
		var offset int64 = 0
		for offset+dataSize <= 64*1024*1024 {
			if err = storage.Write(context.Background(), extentID, offset, dataSize, data, dataCRC, AppendWriteType, false); err != nil {
				err = nil
				break
			}
			offset += dataSize
		}
		extents = append(extents, extentID)
	}

	// using wrong inode number
	for _, extentID := range extents {
		assert.Error(t, storage.TrashExtent(extentID, 11, 64*1024*1024))
	}

	// mv extent to trash dir
	for _, extentID := range extents {
		assert.NoError(t, storage.TrashExtent(extentID, uint64(inode), 64*1024*1024))
	}
	trashExtents := 0
	storage.trashExtents.Range(func(k, v interface{}) bool {
		trashExtents++
		return true
	})
	assert.Equal(t, len(extents), trashExtents)

	t.Log("starting mark delete trash with long keep time")
	// delete trash less than keep time
	storage.MarkDeleteTrashExtents(uint64(time.Since(tStart).Seconds()) + uint64(60*60))
	recentDelete := 0
	storage.recentDeletedExtents.Range(func(k, v interface{}) bool {
		recentDelete++
		return true
	})
	t.Log("verify recent delete")
	assert.Equal(t, 0, recentDelete)

	// sleep 2 sec waiting trash expire
	time.Sleep(time.Second * 2)

	t.Log("starting mark delete trash with short keep time")
	// delete trash
	storage.MarkDeleteTrashExtents(1)
	recentDelete = 0
	storage.recentDeletedExtents.Range(func(k, v interface{}) bool {
		recentDelete++
		return true
	})
	t.Log("verify recent delete")
	assert.Equal(t, len(extents), recentDelete)

	t.Log("starting flush delete")
	deleted, remain, err := storage.FlushDelete(NewFuncInterceptor(nil, nil), 128)
	assert.NoError(t, err)
	assert.Equal(t, len(extents), deleted)
	assert.Equal(t, 0, remain)
	var files []os.DirEntry
	files, err = os.ReadDir(path.Join(storage.dataPath, ExtentTrashDirName))
	assert.NoError(t, err)
	assert.Equal(t, 0, len(files))
}

func TestRecoverTrashExtents(t *testing.T) {
	var testPath = testutil.InitTempTestPath(t)
	defer testPath.Cleanup()
	_, err := log.InitLog(path.Join(os.TempDir(), t.Name(), "logs"), "datanode_test", log.DebugLevel, nil)
	if err != nil {
		t.Errorf("Init log failed: %v", err)
		return
	}
	defer log.LogFlush()
	const (
		partitionID      uint64 = 1
		storageSize             = 1 * 1024 * 1024
		cacheCapacity           = 10
		dataSizePreWrite        = 16
	)

	var testPartitionPath = path.Join(testPath.Path(), fmt.Sprintf("datapartition_%d", partitionID))
	var storage *ExtentStore
	if storage, err = NewExtentStore(testPartitionPath, partitionID, storageSize, cacheCapacity,
		func(event CacheEvent, e *Extent) {}, true, IOInterceptors{}); err != nil {
		t.Fatalf("Create extent store failed: %v", err)
		return
	}

	storage.Load()
	for {
		if storage.IsFinishLoad() {
			break
		}
		time.Sleep(time.Second)
	}

	var dataSize = int64(dataSizePreWrite)
	var data = make([]byte, dataSize)
	var dataCRC = crc32.ChecksumIEEE(data)
	extents := make([]uint64, 0)
	inode := 10
	for i := 0; i < 2; i++ {
		extentID, _ := storage.NextExtentID()
		_ = storage.Create(extentID, uint64(inode), true)
		var offset int64 = 0
		for offset+dataSize <= 64*1024*1024 {
			if err = storage.Write(context.Background(), extentID, offset, dataSize, data, dataCRC, AppendWriteType, false); err != nil {
				err = nil
				break
			}
			offset += dataSize
		}
		extents = append(extents, extentID)
	}

	// mv extent to trash dir
	for _, extentID := range extents {
		assert.NoError(t, storage.TrashExtent(extentID, uint64(inode), 64*1024*1024))
	}
	trashExtents := 0
	storage.trashExtents.Range(func(k, v interface{}) bool {
		trashExtents++
		return true
	})
	assert.Equal(t, len(extents), trashExtents)

	t.Log("starting recover trash extents")
	recovered := 0
	for _, extentID := range extents {
		if storage.RecoverTrashExtent(extentID) {
			recovered++
		}
	}
	assert.Equal(t, len(extents), recovered)

	trashExtents = 0
	storage.trashExtents.Range(func(k, v interface{}) bool {
		trashExtents++
		return true
	})
	assert.Equal(t, 0, trashExtents)
	for _, extentID := range extents {
		var e *Extent
		e, err = storage.extentWithHeaderByExtentID(extentID)
		assert.NoError(t, err)
		assert.NotNil(t, e)
	}

	var files []os.DirEntry
	files, err = os.ReadDir(path.Join(storage.dataPath, ExtentTrashDirName))
	assert.NoError(t, err)
	assert.Equal(t, 0, len(files))

}
