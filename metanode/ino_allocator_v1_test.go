package metanode

import (
	"fmt"
	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/assert"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)


func TestInoAllocatorV1_MaxId(t *testing.T) {
	allocator := NewInoAllocatorV1(2000001, 4000000)
	allocator.SetStatus(allocatorStatusInit)
	allocator.SetStatus(allocatorStatusAvailable)
	allocator.SetId(4000000 - 1)
	_, needFreeze, _ := allocator.AllocateId()
	assert.Equal(t, true, needFreeze)
	allocator.FreezeAllocator(time.Now().Unix(), time.Now().Unix())
	allocator.Active(false)
	id, _, _ := allocator.AllocateId()
	assert.Equal(t, allocator.Start+bitmapCursorStart+1, id, fmt.Sprintf("expect id is %v, but allocate %v", allocator.Start+bitmapCursorStart+1, id))
	allocator.SetId(id)
	id, _, _ = allocator.AllocateId()
	assert.Equal(t, allocator.Start+bitmapCursorStart+2, id, fmt.Sprintf("expect id is %v, but allocate %v", allocator.Start+bitmapCursorStart+2, id))
	allocator.SetId(id)
	allocator.ResetBitCursorToEnd()
	allocator.ClearId(4000000)
	id, needFreeze, _ = allocator.AllocateId()
	assert.Equal(t, true, needFreeze)
	allocator.FreezeAllocator(time.Now().Unix(), time.Now().Unix())
	allocator.Active(false)
	id, _, _ = allocator.AllocateId()
	assert.Equal(t, allocator.Start+bitmapCursorStart+3, id, fmt.Sprintf("expect id is %v, but allocate %v", allocator.Start+bitmapCursorStart+3, id))
}

func TestInoAllocatorV1_MaxCost(t *testing.T) {
	allocator := NewInoAllocatorV1(0, 1<<24 + uint64(rand.Int()) % 64)
	allocator.SetStatus(allocatorStatusInit)
	allocator.SetStatus(allocatorStatusAvailable)
	//set all 1
	for i := 0; i < len(allocator.Bits); i++ {
		allocator.Bits[i] = math.MaxUint64
		allocator.BitsSnap[i] = math.MaxUint64
	}

	allocator.ClearId(3)
	allocator.BitsSnap.ClearBit(3)
	allocator.BitCursor = 4
	id, _, err := allocator.AllocateId()
	if err == nil {
		t.Fatalf("allocate id expect failed, but allocator %v", id)
		return
	}
	allocator.BitCursor = 256
	id, _, err = allocator.AllocateId()
	if err == nil {
		t.Fatalf("allocate id failed, expect err, but now success.id:%d", id)
		return
	}
	allocator.ClearId(allocator.Cnt - 1)
	allocator.BitsSnap.ClearBit(int(allocator.Cnt - 1))
	id, _, err = allocator.AllocateId()
	if err != nil {
		t.Logf(allocator.Bits.GetU64BitInfo(len(allocator.Bits) - 1))
		t.Fatalf("allocate id failed")
		return
	}
	assert.Equal(t, allocator.Cnt - 1, id, "expect allocate id:16777215")
	t.Logf("allocate max id:%d, cnt:%d, end:%d ", id, allocator.Cnt, allocator.End)
}

func TestInoAllocatorV1_NotU64Len(t *testing.T) {
	allocator := NewInoAllocatorV1(0, 1<<24 + 1)
	allocator.SetStatus(allocatorStatusInit)
	//set all 1

	allocator.SetId(3)

	t.Logf(allocator.Bits.GetU64BitInfo(0))
	t.Logf(allocator.Bits.GetU64BitInfo(len(allocator.Bits) - 1))
}

func TestInoAllocatorV1_U64Len(t *testing.T) {
	allocator := NewInoAllocatorV1(0, 1<<24)
	allocator.SetStatus(allocatorStatusInit)
	//set all 1

	allocator.SetId(3)
	t.Logf(allocator.Bits.GetU64BitInfo(0))
	t.Logf(allocator.Bits.GetU64BitInfo(len(allocator.Bits) - 1))
}

func InoAlloterv1UsedCnt(t *testing.T, allocator *inoAllocatorV1) {
	var cnt = 0
	for i := uint64(0); cnt < 100; i++ {
		if i*100+i <= bitmapCursorStart {
			continue
		}
		cnt++
		allocator.SetId(i*100 + i + allocator.Start)
	}
	if allocator.GetUsed() != 100 + 2 {
		t.Fatalf("allocate 100, but record:%d, cap:%d", allocator.GetUsed(), allocator.Cnt+2)
	}

	cnt = 0
	for i := uint64(0); cnt < 100; i++ {
		if i*100+i <= bitmapCursorStart {
			continue
		}
		if allocator.Bits.IsBitFree(int((i*100 + i - allocator.Start) % allocator.Cnt)) {
			t.Fatalf("id allocator:%d but now free, cap:%d", i*100+i, allocator.Cnt)
		}
		cnt++
	}

	cnt = 0
	for i := uint64(0); cnt < 100; i++ {
		if i*100+i <= bitmapCursorStart {
			continue
		}
		cnt++
		allocator.ClearId(i*100 + i + allocator.Start)
	}
	if allocator.GetUsed() != 2 {
		t.Fatalf("allocate 0, but record:%d, cap:%d", allocator.GetUsed(), allocator.Cnt+2)
	}
	cnt = 0
	for i := uint64(0); cnt < 100; i++ {
		if i*100+i <= bitmapCursorStart {
			continue
		}
		if !allocator.Bits.IsBitFree(int((i*100 + i - allocator.Start) % allocator.Cnt)) {
			t.Fatalf("id allocator:%d but now free, cap:%d", i*100+i, allocator.Cnt)
		}
		cnt++
	}
}

func TestInoAllocatorV1_UsedCnt(t *testing.T) {
	allocator  := NewInoAllocatorV1(0, 1<<24 + 1)
	//set all 1
	allocator1 := NewInoAllocatorV1(0, 1<<24)
	allocator.SetStatus(allocatorStatusInit)
	allocator1.SetStatus(allocatorStatusInit)
	InoAlloterv1UsedCnt(t, allocator)
	InoAlloterv1UsedCnt(t, allocator1)
}

func InoAlloterv1Allocate(t *testing.T, allocator *inoAllocatorV1, start uint64) {
	allocator.SetId(start)
	allocator.BitsSnap.SetBit(int(start - allocator.Start))
	for i := uint64(0) ; i < 100; i++ {
		id, _, _ := allocator.AllocateId()
		allocator.SetId(id)
	}
	t.Logf(allocator.Bits.GetU64BitInfo(int(start / 64)))
	t.Logf(allocator.Bits.GetU64BitInfo(int(start / 64 + 1)))
	for i := uint64(0); i < 100; i++ {
		if allocator.Bits.IsBitFree(int((i + 1 + start) % allocator.Cnt)) {
			t.Fatalf("id allocator:%d but now free, cap:%d", i, allocator.Cnt)
		}
	}

	for i := uint64(0) ; i < 100; i++ {
		allocator.ClearId((i + 1 + start)%allocator.Cnt)
	}

	t.Logf(allocator.Bits.GetU64BitInfo(int(start / 64)))
	t.Logf(allocator.Bits.GetU64BitInfo(int(start / 64 + 1)))
	for i := uint64(0); i < 100; i++ {
		if !allocator.Bits.IsBitFree(int((i + 1 + start) % allocator.Cnt)) {
			t.Fatalf("id allocator:%d but now free, cap:%d", i, allocator.Cnt)
		}
	}
	return
}

func TestInoAllocatorV1_Allocate(t *testing.T) {
	allocator  := NewInoAllocatorV1(0, 1<<24 + 1)
	//set all
	allocator1 := NewInoAllocatorV1(0, 1<<24)
	allocator.SetStatus(allocatorStatusInit)
	allocator.SetStatus(allocatorStatusAvailable)
	allocator1.SetStatus(allocatorStatusInit)
	allocator1.SetStatus(allocatorStatusAvailable)
	InoAlloterv1Allocate(t, allocator, uint64(rand.Int()) % allocator.Cnt)
	InoAlloterv1Allocate(t, allocator1, uint64(rand.Int()) % allocator1.Cnt)
}

func TestInoAllocatorV1_StTest(t *testing.T) {
	var err error
	allocator := NewInoAllocatorV1(0, 1<<24)
	//stopped
	err = allocator.SetStatus(allocatorStatusAvailable)
	if err == nil {
		t.Fatalf("expect err, but now nil")
	}
	t.Logf("stat stopped-->started :%v", err)

	err = allocator.SetStatus(allocatorStatusUnavailable)
	if err != nil {
		t.Fatalf("expect nil, but err:%v", err.Error())
	}
	t.Logf("stat stopped-->stopped")

	err = allocator.SetStatus(allocatorStatusInit)
	if err != nil {
		t.Fatalf("expect nil, but err:%v", err.Error())
	}
	t.Logf("stat stopped-->init")

	//init
	err = allocator.SetStatus(allocatorStatusUnavailable)
	if err != nil {
		t.Fatalf("expect nil, but err:%v", err.Error())
	}
	t.Logf("stat init-->stopped")
	allocator.SetStatus(allocatorStatusInit)

	err = allocator.SetStatus(allocatorStatusAvailable)
	if err != nil {
		t.Fatalf("expect nil, but err:%v", err.Error())
	}
	t.Logf("stat init-->start")

	//start
	err = allocator.SetStatus(allocatorStatusAvailable)
	if err != nil {
		t.Fatalf("expect nil, but err:%v", err.Error())
	}
	t.Logf("stat start-->start")

	err = allocator.SetStatus(allocatorStatusInit)
	if err == nil {
		t.Fatalf("expect err, but now nil")
	}
	t.Logf("stat started-->init :%v", err)

	err = allocator.SetStatus(allocatorStatusUnavailable)
	if err != nil {
		t.Fatalf("expect nil, but err:%v", err.Error())
	}
	t.Logf("stat start-->stopped")
}

func TestInoAllocatorV1_AllocateIdBySnap(t *testing.T) {
	allocator := NewInoAllocatorV1(0, proto.DefaultMetaPartitionInodeIDStep)
	_ = allocator.SetStatus(allocatorStatusInit)
	_ = allocator.SetStatus(allocatorStatusAvailable)

	cnt := 100
	occupiedIDs := make([]uint64, 0, cnt)
	rand.Seed(time.Now().UnixMicro())
	for index := 1; index <= cnt; index++ {
		id := uint64(rand.Intn(int(allocator.Cnt)))
		occupiedIDs = append(occupiedIDs, id)
		allocator.SetId(id)
	}

	sort.Slice(occupiedIDs, func(i, j int) bool {
		return occupiedIDs[i] < occupiedIDs[j]
	})

	FreezeAllocator(allocator)
	if _, _, err := allocator.AllocateId(); err == nil {
		t.Fatalf("allocator has been freezed, expect allocate failed")
		return
	}

	releaseIDs := make([]uint64, 0)
	for index, id := range occupiedIDs {
		if index % 3 == 0 {
			allocator.ClearId(id)
			releaseIDs = append(releaseIDs, id)
		}
	}

	for _, releaseID := range releaseIDs {
		assert.Equal(t, true, allocator.Bits.IsBitFree(int(releaseID)), fmt.Sprintf("%v expect release in allocator bits", releaseID))
		assert.Equal(t, false, allocator.BitsSnap.IsBitFree(int(releaseID)), fmt.Sprintf("%v expect has been occupied", releaseID))
	}

	if !waitAllocatorActive(allocator) {
		t.Fatalf("active allocator failed")
		return
	}

	allocateCnt := 0
	for index := uint64(0); index < allocator.Cnt; index++ {
		allocateID, needFreeze, err := allocator.AllocateId()
		if err != nil {
			if needFreeze{
				break
			}
			t.Fatalf("allocate failed:%v", err)
			return
		}

		allocateCnt++
		for _, id := range occupiedIDs {
			if allocateID == id {
				t.Fatalf("occupied id(%v) has been allocated, unexpect", id)
			}
		}
		allocator.SetId(allocateID)
	}

	assert.Equal(t, proto.DefaultMetaPartitionInodeIDStep - uint64(cnt+3), uint64(allocateCnt))

	FreezeAllocator(allocator)
	if _, _, err := allocator.AllocateId(); err == nil {
		t.Fatalf("allocator has been freezed, expect allocate failed")
		return
	}

	//release all
	for index := uint64(0); index < allocator.Cnt; index++ {
		allocator.ClearId(index)
	}

	if !waitAllocatorActive(allocator) {
		t.Fatalf("allocator active failed")
		return
	}

	for index := uint64(0); index < allocator.Cnt; index++ {
		allocateID, needFreeze, err := allocator.AllocateId()
		if err != nil {
			if needFreeze{
				break
			}
			t.Fatalf("allocate failed:%v", err)
			return
		}

		find := false
		for _, id := range releaseIDs {
			if allocateID == id {
				find = true
				break
			}
		}
		if !find {
			t.Fatalf("%v still occupied, but has been allocated, already relase IDs%v", allocateID, releaseIDs)
		}
	}

	return
}

func FreezeAllocator(allocator *inoAllocatorV1) {
	allocator.FreezeAllocator(time.Now().Unix(), time.Now().Add(time.Second*5).Unix())
	go func() {
		intervalCheckActive := time.Second*1
		timer := time.NewTimer(intervalCheckActive)
		for {
			select {
			case <- timer.C:
				timer.Reset(intervalCheckActive)
				if time.Now().Before(time.Unix(allocator.ActiveTime, 0)) {
					continue
				}
				allocator.Active(false)
				return
			}
		}
	}()
}

func waitAllocatorActive(allocator *inoAllocatorV1) (activeSuccess bool) {
	time.Sleep(time.Second * 5)
	retryCnt := 5
	for retryCnt > 0 {
		if allocator.GetStatus() == allocatorStatusFrozen {
			time.Sleep(time.Second*1)
			retryCnt--
			continue
		}
		break
	}
	if retryCnt == 0 {
		return
	}
	activeSuccess = true
	return
}

func TestInoAllocatorV1_BitCursor(t *testing.T) {
	allocator := NewInoAllocatorV1(0, proto.DefaultMetaPartitionInodeIDStep)
	_ = allocator.SetStatus(allocatorStatusInit)
	_ = allocator.SetStatus(allocatorStatusAvailable)

	for index := uint64(0); index < 20033; index++ {
		allocator.SetId(index)
	}

	allocator.ResetBitCursorToEnd()

	id, needFreeze, _ := allocator.AllocateId()
	assert.Equal(t, true, needFreeze, fmt.Sprintf("need freeze, but allocate : %v", id))

	FreezeAllocator(allocator)
	_, _, err := allocator.AllocateId()
	if err == nil {
		t.Fatalf("allocator has been freezed, expect allocate failed")
		return
	}

	for index := uint64(1000); index < 1500; index++ {
		allocator.ClearId(index)
	}

	if !waitAllocatorActive(allocator) {
		t.Fatalf("active allocator failed")
		return
	}

	id, needFreeze, _ = allocator.AllocateId()
	assert.Equal(t, false, needFreeze, fmt.Sprintf("expect allocate success"))
	assert.Equal(t, uint64(20033), id)
}

func TestInoAllocatorV1_MarshalAndUnmarshal(t *testing.T) {
	allocator := NewInoAllocatorV1(0, proto.DefaultMetaPartitionInodeIDStep)
	_ = allocator.SetStatus(allocatorStatusInit)
	_ = allocator.SetStatus(allocatorStatusAvailable)

	for index := uint64(0); index < 20033; index++ {
		allocator.SetId(index)
	}

	allocatorSnap := allocator.GenAllocatorSnap()
	assert.NotEmpty(t, allocatorSnap)

	data := allocatorSnap.MarshalBinary()
	assert.NotEmpty(t, data)

	newAllocator := NewInoAllocatorV1(0, proto.DefaultMetaPartitionInodeIDStep)
	err := newAllocator.UnmarshalBinary(data)
	assert.Empty(t, err)

	assert.Equal(t, allocator.Version, newAllocator.Version)
	assert.Equal(t, allocator.Status, newAllocator.Status)
	assert.Equal(t, allocator.BitCursor, newAllocator.BitCursor)
	assert.Equal(t, allocator.FreezeTime, newAllocator.FreezeTime)
	assert.Equal(t, allocator.ActiveTime, newAllocator.ActiveTime)
	assert.Equal(t, allocator.BitsSnap, newAllocator.BitsSnap)
}