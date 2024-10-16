package data

import (
	"context"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"path"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/common"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
	"github.com/stretchr/testify/assert"
)

type Rule struct {
	offset      uint64
	size        int
	writeCount  int
	isFlush     bool
	isOverWrite bool
	isRead      bool
}

type ExtentRule struct {
	FileOffset uint64
	Size       uint32
}

func (rule *ExtentRule) String() string {
	if rule == nil {
		return ""
	}
	return fmt.Sprintf("FileOffset(%v) Size(%v)", rule.FileOffset, rule.Size)
}

func TestStreamer_WritePendingPacket(t *testing.T) {
	s := &Streamer{handler: &ExtentHandler{inode: 999, storeMode: proto.NormalExtentType}}
	tests := []struct {
		name                    string
		writeOffset             uint64
		writeSize               int
		orgPendingPacketList    []*common.Packet
		expectPendingPacketList []*common.Packet
	}{
		{
			name:                 "test01",
			writeOffset:          10,
			writeSize:            128,
			orgPendingPacketList: []*common.Packet{},
			expectPendingPacketList: []*common.Packet{
				newPacket(10, 128),
			},
		},
		{
			// 插入头
			name:        "test02",
			writeOffset: 5,
			writeSize:   10,
			orgPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(5, 10),
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
			},
		},
		{
			// 插入中间
			name:        "test03",
			writeOffset: 160,
			writeSize:   30,
			orgPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(160, 30),
				newPacket(200, 100),
				newPacket(300, 400),
			},
		},
		{
			// 插入尾
			name:        "test04",
			writeOffset: 500,
			writeSize:   600,
			orgPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
				newPacket(500, 600),
			},
		},
		{
			// 连接第一个packet
			name:        "test05",
			writeOffset: 50,
			writeSize:   50,
			orgPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(50, 50),
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
			},
		},
		{
			// 写入第一个packet
			name:        "test06",
			writeOffset: 150,
			writeSize:   50,
			orgPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(200, 100),
				newPacket(300, 400),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(100, 100),
				newPacket(200, 100),
				newPacket(300, 400),
			},
		},
		{
			// 写入第一个packet + 跨packet
			name:        "test07",
			writeOffset: 150,
			writeSize:   128 * 1024,
			orgPendingPacketList: []*common.Packet{
				newPacket(100, 50),
				newPacket(2*128*1024, 128*1024),
				newPacket(3*128*1024, 100),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(100, 128*1024),
				newPacket(100+128*1024, 50),
				newPacket(2*128*1024, 128*1024),
				newPacket(3*128*1024, 100),
			},
		},
		{
			// 写入第二个packet + 跨packet + 与第三个packet相接
			name:        "test08",
			writeOffset: (2*128 + 64) * 1024,
			writeSize:   (2*128 - 64) * 1024,
			orgPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 64*1024),
				newPacket(4*128*1024, 128*1024),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 128*1024),
				newPacket(3*128*1024, 128*1024),
				newPacket(4*128*1024, 128*1024),
			},
		},
		{
			// 接在第二个packet尾部 + 跨packet
			name:        "test09",
			writeOffset: (2*128 + 64) * 1024,
			writeSize:   128 * 1024,
			orgPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 64*1024),
				newPacket(4*128*1024, 128*1024),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 128*1024),
				newPacket(3*128*1024, 64*1024),
				newPacket(4*128*1024, 128*1024),
			},
		},
		{
			// 接在第三个packet尾部
			name:        "test10",
			writeOffset: (4*128 + 64) * 1024,
			writeSize:   64 * 1024,
			orgPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 64*1024),
				newPacket(4*128*1024, 64*1024),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 64*1024),
				newPacket(4*128*1024, 128*1024),
			},
		},
		{
			// 接在第三个packet之后
			name:        "test11",
			writeOffset: 5 * 128 * 1024,
			writeSize:   128 * 1024,
			orgPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 64*1024),
				newPacket(4*128*1024, 128*1024),
			},
			expectPendingPacketList: []*common.Packet{
				newPacket(0, 128*1024),
				newPacket(2*128*1024, 64*1024),
				newPacket(4*128*1024, 128*1024),
				newPacket(5*128*1024, 128*1024),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.pendingPacketList = tt.orgPendingPacketList
			writeData := make([]byte, tt.writeSize)
			for i := 0; i < tt.writeSize; i++ {
				writeData[i] = 'a'
			}
			ek, err := s.WritePendingPacket(writeData, tt.writeOffset, tt.writeSize, true)
			if err != nil {
				t.Fatalf("TestStreamer_WritePendingPacket: write err(%v) name(%v) offset(%v) size(%v)", err, tt.name, tt.writeOffset, tt.writeSize)
			}
			// check ek
			if ek.FileOffset != uint64(tt.writeOffset) || ek.Size != uint32(tt.writeSize) {
				t.Fatalf("TestStreamer_WritePendingPacket: name(%v) expect offset(%v) but offset(%v) expect size(%v) but size(%v)",
					tt.name, tt.writeOffset, ek.FileOffset, tt.writeSize, ek.Size)
			}
			// check pending packet list
			if len(tt.expectPendingPacketList) != len(s.pendingPacketList) {
				t.Fatalf("TestStreamer_WritePendingPacket: name(%v) expect list len(%v) but(%v)", tt.name, len(tt.expectPendingPacketList), len(s.pendingPacketList))
			}
			//t.Log("pending packet list: ", s.pendingPacketList)
			for i := 0; i < len(tt.expectPendingPacketList); i++ {
				expectPacket := tt.expectPendingPacketList[i]
				packet := s.pendingPacketList[i]
				if expectPacket.KernelOffset != packet.KernelOffset || expectPacket.Size != expectPacket.Size {
					t.Fatalf("TestStreamer_WritePendingPacket: name(%v) expect offset(%v) but offset(%v) expect size(%v) but size(%v)",
						tt.name, expectPacket.KernelOffset, packet.KernelOffset, expectPacket.Size, packet.Size)
				}
			}
			return
		})
	}
}

func newPacket(offset uint64, size uint32) (packet *common.Packet) {
	packet = &common.Packet{}
	packet.KernelOffset = offset
	packet.Size = size
	packet.Data = make([]byte, 128*1024)
	return packet
}

func TestStreamer_WriteFile_Pending(t *testing.T) {
	ec.SetEnableWriteCache(true)
	ec.tinySize = unit.DefaultTinySizeLimit
	ec.dataWrapper.followerRead = false

	// create local directory
	localTestDir := "/tmp/pending_packet_test"
	os.MkdirAll(localTestDir, 0755)

	defer func() {
		//mw.Delete_ll(context.Background(), 1, "TestPendingPacket", false)
		//os.RemoveAll(localTestDir)
		log.LogFlush()
	}()

	t.Log("TestExtentHandler_PendingPacket: start test")

	tests := []struct {
		name           string
		operationRules []*Rule
		extentTypeList []*ExtentRule
	}{
		{
			name: "test01",
			operationRules: []*Rule{
				{offset: 0, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 2 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 3 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
			},
			extentTypeList: []*ExtentRule{{FileOffset: 0, Size: 4 * 128 * 1024}},
		},
		{
			name: "test02",
			operationRules: []*Rule{
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false},
				{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 8 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
				{offset: 6 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
			},
			extentTypeList: []*ExtentRule{{FileOffset: 128 * 1024, Size: 10 * 128 * 1024}},
		},
		{
			name: "test02_flush_continuous_packet",
			operationRules: []*Rule{
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false},
				{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 8 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 7 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 6 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 9 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
			},
			extentTypeList: []*ExtentRule{{FileOffset: 128 * 1024, Size: 10 * 128 * 1024}},
		},
		{
			name: "test02_read_pending_packet",
			operationRules: []*Rule{
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false},
				{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 8 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
				{offset: 9 * 128 * 1024, size: 2 * 128 * 1024, isRead: true},
				{offset: 6 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 7 * 128 * 1024},
				{FileOffset: 8 * 128 * 1024, Size: 3 * 128 * 1024},
			},
		},
		//{
		//	name: "test02_read_extent",
		//	operationRules: []*Rule{
		//		{offset: 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false},
		//		{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
		//		{offset: 8 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
		//		{offset: 3 * 128 * 1024, size: 2 * 128 * 1024, isRead: true},
		//		{offset: 6 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
		//	},
		//	extentTypeList: []*ExtentRule{{FileOffset: 128 * 1024, Size: 10 * 128 * 1024}},
		//},
		{
			name: "test03",
			operationRules: []*Rule{
				// eh满了之后关闭，下一个包跳了offset
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 1020, isFlush: false},
				{offset: 1030 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 1025 * 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false},
				{offset: 1021 * 128 * 1024, size: 128 * 1024, writeCount: 4, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 1020 * 128 * 1024},
				{FileOffset: 1021 * 128 * 1024, Size: 9 * 128 * 1024},
				{FileOffset: 1030 * 128 * 1024, Size: 128 * 1024},
			},
		},
		{
			name: "test04",
			operationRules: []*Rule{
				// eh满了之后关闭，下一个包跳了offset
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 1020, isFlush: false},
				{offset: 1022 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 1030 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 1021 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 1023 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
				{offset: 1027 * 128 * 1024, size: 128 * 1024, writeCount: 3, isFlush: false},
				{offset: 1025 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
				{offset: 1031 * 128 * 1024, size: 128 * 1024, writeCount: 1024, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 1020 * 128 * 1024},
				{FileOffset: 1021 * 128 * 1024, Size: 128 * 1024},
				{FileOffset: 1022 * 128 * 1024, Size: 128 * 1024},
				{FileOffset: 1023 * 128 * 1024, Size: 7 * 128 * 1024},
				{FileOffset: 1030 * 128 * 1024, Size: 1024 * 128 * 1024},
				{FileOffset: (1030 + 1024) * 128 * 1024, Size: 128 * 1024},
			},
		},
		{
			name: "test05",
			operationRules: []*Rule{
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 4, isFlush: false},
				{offset: 5 * 128 * 1024, size: 16 * 1024, writeCount: 1, isFlush: false},
				{offset: 6 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 7 * 128 * 1024, size: 16 * 1024, writeCount: 7, isFlush: false},
				{offset: 9 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: (7*128 + 7*16) * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: (8*128 + 7*16) * 1024, size: 16 * 1024, writeCount: 1, isFlush: false},
				{offset: (5*128 + 16) * 1024, size: 112 * 1024, writeCount: 1, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 9 * 128 * 1024},
			},
		},
		{
			name: "overwrite_local_pending_packet_01",
			operationRules: []*Rule{
				// overwrite写本地多个pendingPacket
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 9, isFlush: false},
				{offset: 12 * 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false},
				{offset: 15 * 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false}, // overwrite 15*128*1024 ~ 17*128*1024
				{offset: 20 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 20 * 128 * 1024},
			},
		},
		{
			name: "overwrite_local_pending_packet_02",
			operationRules: []*Rule{
				// overwrite写本地一个pendingPacket
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 9, isFlush: false},
				{offset: 12 * 128 * 1024, size: 128 * 1024, writeCount: 5, isFlush: false},
				{offset: 16 * 128 * 1024, size: 128 * 1024, writeCount: 4, isFlush: false}, // overwrite 16*128*1024 ~ 17*128*1024
				{offset: 20 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 2, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 20 * 128 * 1024},
			},
		},
		{
			name: "overwrite_local_pending_packet_03",
			operationRules: []*Rule{
				// overwrite写本地多个pendingPacket
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 3, isFlush: false},
				{offset: 4 * 128 * 1024, size: 16 * 1024, writeCount: 1, isFlush: false},
				{offset: 7 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 5 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 6 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 9 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: (6*128 - 16) * 1024, size: 32 * 1024, writeCount: 1, isFlush: false}, // overwrite (6*128-16)*1024 ~ (6*128+16)*1024
				{offset: 11 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 8 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: (4*128 + 16) * 1024, size: (128 - 16) * 1024, writeCount: 1, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 11 * 128 * 1024},
			},
		},
		{
			name: "overwrite_local_eh_packet",
			operationRules: []*Rule{
				// overwrite写本地的eh.packet
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 9, isFlush: false},
				{offset: 16 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 15 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 10 * 128 * 1024, size: 64 * 1024, writeCount: 1, isFlush: false},
				{offset: (10*128 + 60) * 1024, size: 128 * 1024, writeCount: 4, isFlush: false}, // overwrite (10*128+60)*1024 ~ (10*128+64)*1024
				{offset: (14*128 + 60) * 1024, size: (128 - 60) * 1024, writeCount: 1, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 16 * 128 * 1024},
			},
		},
		{
			name: "overwrite_remote_eh_packet",
			operationRules: []*Rule{
				// overwrite超过了eh.packet的范围，flush后写远程
				{offset: 128 * 1024, size: 128 * 1024, writeCount: 3, isFlush: false},
				{offset: 4 * 128 * 1024, size: 16 * 1024, writeCount: 1, isFlush: false},
				{offset: 7 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 5 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 6 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 9 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: (4*128 - 16) * 1024, size: 32 * 1024, writeCount: 1, isFlush: false}, // flush and overwrite (4*128-16)*1024 ~ (4*128+16)*1024
				{offset: 8 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: (4*128 + 16) * 1024, size: (128 - 16) * 1024, writeCount: 1, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 128 * 1024, Size: 4 * 128 * 1024},
				{FileOffset: 5 * 128 * 1024, Size: 3 * 128 * 1024},
				{FileOffset: 8 * 128 * 1024, Size: 1 * 128 * 1024},
				{FileOffset: 9 * 128 * 1024, Size: 1 * 128 * 1024},
			},
		},
		{
			name: "tiny_pending_packet",
			operationRules: []*Rule{
				{offset: 0, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 2 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 5 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 10 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 12 * 128 * 1024, size: 128 * 1024, writeCount: 1, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 0, Size: 128 * 1024},
				{FileOffset: 2 * 128 * 1024, Size: 128 * 1024},
				{FileOffset: 5 * 128 * 1024, Size: 128 * 1024},
				{FileOffset: 10 * 128 * 1024, Size: 128 * 1024},
				{FileOffset: 12 * 128 * 1024, Size: 128 * 1024},
			},
		},
		{
			name: "pending_packet_length_max",
			operationRules: []*Rule{
				{offset: 0, size: 128 * 1024, writeCount: 1, isFlush: false},
				{offset: 2 * 128 * 1024, size: 128 * 1024, writeCount: 16, isFlush: false},
				{offset: 20 * 128 * 1024, size: 128 * 1024, writeCount: 1024, isFlush: false},
			},
			extentTypeList: []*ExtentRule{
				{FileOffset: 0, Size: 128 * 1024},
				{FileOffset: 2 * 128 * 1024, Size: 16 * 128 * 1024},
				{FileOffset: 20 * 128 * 1024, Size: 128 * 1024 * 1024},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// create local file
			localFile, err := os.Create(path.Join(localTestDir, tt.name))
			if err != nil {
				t.Fatalf("TestStreamer_WriteFile_Pending create local file(%v) err: %v", tt.name, err)
			}
			defer localFile.Close()
			// create CFS file
			mw.Delete_ll(context.Background(), 1, "TestPendingPacket_"+tt.name, false)
			inodeInfo, err := mw.Create_ll(context.Background(), 1, "TestPendingPacket_"+tt.name, 0644, 0, 0, nil)
			if err != nil {
				t.Fatalf("TestExtentHandler_PendingPacket: creat inode failed, err(%v)", err)
			}
			streamer := NewStreamer(ec, inodeInfo.Inode, ec.streamerConcurrentMap.GetMapSegment(inodeInfo.Inode), false)
			streamer.refcnt++
			isRecover := false
			hasROW := false
			t.Log("TestExtentHandler_PendingPacket: done create inode")

			defer func() {
				close(streamer.done)
			}()

			writeIndex := 0
			// write
			for _, rule := range tt.operationRules {
				if rule.isRead {
					time.Sleep(10 * time.Second)
					readCFSData := make([]byte, rule.size)
					readSize, _, err := streamer.read(context.Background(), readCFSData, rule.offset, rule.size)
					if err != nil || readSize != rule.size {
						t.Fatalf("TestStreamer_WriteFile_Pending read CFS file offset(%v) size(%v) err(%v) test(%v) expect size(%v) but size(%v)",
							rule.offset, rule.size, err, tt.name, rule.size, readSize)
					}
					if streamer.handler != nil && streamer.handler.getStatus() >= ExtentStatusRecovery {
						isRecover = true
					}
					// check data crc
					readLocalData := make([]byte, rule.size)
					_, err = localFile.ReadAt(readLocalData, int64(rule.offset))
					if err != nil {
						t.Fatalf("TestStreamer_WriteFile_Pending read local file offset(%v) size(%v) err(%v) file(%v)",
							rule.offset, rule.size, err, localFile.Name())
					}
					if crc32.ChecksumIEEE(readCFSData[:rule.size]) != crc32.ChecksumIEEE(readLocalData[:rule.size]) {
						t.Fatalf("TestStreamer_WriteFile_Pending failed: test(%v) offset(%v) size(%v) crc is inconsistent",
							tt.name, rule.offset, rule.size)
					}
					continue
				}
				size := rule.size
				writeData := make([]byte, size)
				for i := 0; i < rule.writeCount; i++ {
					writeIndex++
					for j := 0; j < size; j++ {
						writeData[j] = byte(writeIndex)
					}
					offset := rule.offset + uint64(i*size)
					t.Logf("test(%v) write offset(%v) size(%v)\n", tt.name, offset, size)
					total, isROW, err := streamer.write(context.Background(), writeData, offset, size, false)
					hasROW = hasROW || isROW
					if err != nil || total != size {
						t.Fatalf("TestStreamer_WriteFile_Pending write: name(%v) err(%v) total(%v) isROW(%v) expect size(%v)", tt.name, err, total, isROW, size)
					}
					if streamer.handler != nil && streamer.handler.getStatus() >= ExtentStatusRecovery {
						isRecover = true
					}
					if _, err = localFile.WriteAt(writeData, int64(offset)); err != nil {
						t.Fatalf("TestStreamer_WriteFile_Pending failed: write local file err(%v) name(%v)", err, localFile.Name())
					}
				}
				// flush
				if rule.isFlush {
					if err = streamer.flush(context.Background(), true); err != nil {
						t.Fatalf("TestStreamer_WriteFile_Pending cfs flush err(%v) test(%v)", err, tt.name)
					}
					if streamer.handler != nil && streamer.handler.getStatus() >= ExtentStatusRecovery {
						isRecover = true
					}
					if err = localFile.Sync(); err != nil {
						t.Fatalf("TestStreamer_WriteFile_Pending local flush err(%v) file(%v)", err, localFile.Name())
					}
				}
			}
			// flush
			t.Log("TestStreamer_WriteFile_Pending: start flush file")
			if err = streamer.flush(context.Background(), true); err != nil {
				t.Fatalf("TestStreamer_WriteFile_Pending cfs flush err(%v) test(%v)", err, tt.name)
			}
			if streamer.handler != nil && streamer.handler.getStatus() >= ExtentStatusRecovery {
				isRecover = true
			}
			if err = localFile.Sync(); err != nil {
				t.Fatalf("TestStreamer_WriteFile_Pending local flush err(%v) file(%v)", err, localFile.Name())
			}
			extents := streamer.extents.List()
			if !isRecover && !hasROW {
				// check extent
				t.Log("TestStreamer_WriteFile_Pending: start check extent")
				if len(extents) == 0 || len(extents) != len(tt.extentTypeList) {
					t.Fatalf("TestStreamer_WriteFile_Pending failed: test(%v) expect extent length(%v) but(%v) extents(%v)",
						tt.name, len(tt.extentTypeList), len(extents), extents)
				}
				unexpectedExtent := false
				for i, ext := range extents {
					expectExtent := tt.extentTypeList[i]
					if ext.FileOffset != expectExtent.FileOffset || ext.Size != expectExtent.Size {
						unexpectedExtent = true
						break
					}
				}
				if unexpectedExtent {
					t.Fatalf("TestStreamer_WriteFile_Pending failed: test(%v) expect extent list(%v) but(%v)",
						tt.name, tt.extentTypeList, extents)
				}
				t.Log("TestStreamer_WriteFile_Pending: extent list: ", extents)
			}

			// read data and check crc
			for _, ext := range extents {
				//t.Log("TestStreamer_WriteFile_Pending: start read file extent: ", ext)
				offset := ext.FileOffset
				size := int(ext.Size)
				readCFSData := make([]byte, size)
				readSize, _, err := streamer.read(context.Background(), readCFSData, offset, size)
				if err != nil || readSize != size {
					t.Fatalf("TestStreamer_WriteFile_Pending read CFS file offset(%v) size(%v) err(%v) test(%v) expect size(%v) but size(%v)",
						offset, size, err, tt.name, size, readSize)
				}
				// check data crc
				readLocalData := make([]byte, size)
				_, err = localFile.ReadAt(readLocalData, int64(offset))
				if err != nil {
					t.Fatalf("TestStreamer_WriteFile_Pending read local file offset(%v) size(%v) err(%v) file(%v)", offset, size, err, localFile.Name())
				}
				if crc32.ChecksumIEEE(readCFSData[:size]) != crc32.ChecksumIEEE(readLocalData[:size]) {
					t.Fatalf("TestStreamer_WriteFile_Pending failed: test(%v) offset(%v) size(%v) crc is inconsistent", tt.name, offset, size)
				}
			}
			return
		})
	}
}

func TestStreamer_WriteFile_discontinuous(t *testing.T) {
	ec.autoFlush = true
	ec.SetEnableWriteCache(true)
	ec.tinySize = unit.DefaultTinySizeLimit
	mw.Delete_ll(context.Background(), 1, "TestStreamer_WriteFile_discontinuous", false)
	inodeInfo, err := mw.Create_ll(context.Background(), 1, "TestStreamer_WriteFile_discontinuous", 0644, 0, 0, nil)
	if err != nil {
		t.Fatalf("TestExtentHandler_PendingPacket: creat inode failed, err(%v)", err)
	}

	err = ec.OpenStream(inodeInfo.Inode, false)
	assert.Equal(t, nil, err, "open stream")

	localPath := "/tmp/TestStreamer_WriteFile_discontinuous"
	localFile, _ := os.Create(localPath)

	defer func() {
		log.LogFlush()
		localFile.Close()
	}()
	// discontinuous write
	writeSize := 128 * 1024
	if err = writeLocalAndCFS(localFile, ec, inodeInfo.Inode, 0, writeSize); err != nil {
		t.Fatalf("TestStreamer_WriteFile_discontinuous: err(%v)", err)
		return
	}
	if err = writeLocalAndCFS(localFile, ec, inodeInfo.Inode, 2*128*1024, writeSize); err != nil {
		t.Fatalf("TestStreamer_WriteFile_discontinuous: err(%v)", err)
		return
	}
	if err = writeLocalAndCFS(localFile, ec, inodeInfo.Inode, 4*128*1024, writeSize); err != nil {
		t.Fatalf("TestStreamer_WriteFile_discontinuous: err(%v)", err)
		return
	}
	time.Sleep(30 * time.Second)
	localFile.Sync()
	// verify data
	if err = verifyLocalAndCFS(localFile, ec, inodeInfo.Inode, 0, writeSize); err != nil {
		t.Fatalf("TestStreamer_WriteFile_discontinuous: err(%v)", err)
		return
	}
	if err = verifyLocalAndCFS(localFile, ec, inodeInfo.Inode, 2*128*1024, writeSize); err != nil {
		t.Fatalf("TestStreamer_WriteFile_discontinuous: err(%v)", err)
		return
	}
	if err = verifyLocalAndCFS(localFile, ec, inodeInfo.Inode, 4*128*1024, writeSize); err != nil {
		t.Fatalf("TestStreamer_WriteFile_discontinuous: err(%v)", err)
		return
	}
}

func TestStreamer_RandWritePending(t *testing.T) {
	ec.autoFlush = true
	ec.SetEnableWriteCache(true)
	ec.tinySize = unit.DefaultTinySizeLimit

	defer func() {
		log.LogFlush()
	}()

	mw.Delete_ll(context.Background(), 1, "TestStreamer_RandPending", false)
	inodeInfo, err := mw.Create_ll(context.Background(), 1, "TestStreamer_RandPending", 0644, 0, 0, nil)
	if err != nil {
		t.Fatalf("TestStreamer_RandPending: creat inode failed, err(%v)", err)
	}
	err = ec.OpenStream(inodeInfo.Inode, false)
	assert.Equal(t, nil, err, "open stream")

	localPath := "/tmp/TestStreamer_RandPending"
	localFile, _ := os.Create(localPath)

	go func() {
		for {
			streamer := ec.GetStreamer(inodeInfo.Inode)
			if streamer != nil {
				streamer.GetExtents(context.Background())
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()
	timestamp := time.Now().Unix()
	rand.Seed(timestamp)
	t.Log("time: ", timestamp)
	for i := 0; i < 1024; i++ {
		wOffset, wSize := randOffset(128 * 1024 * 1024)
		//t.Logf("write offset: %v size: %v\n", wOffset, wSize)
		if err = writeLocalAndCFS(localFile, ec, inodeInfo.Inode, wOffset, wSize); err != nil {
			panic(err)
		}
		rOffset, rSize := randOffset(128 * 1024 * 1024)
		//t.Logf("read offset: %v size: %v\n", rOffset, rSize)
		if err = verifyLocalAndCFS(localFile, ec, inodeInfo.Inode, rOffset, rSize); err != nil {
			log.LogFlush()
			panic(err)
		}
	}
	cfsSize, _, _ := ec.FileSize(inodeInfo.Inode)
	localInfo, _ := localFile.Stat()
	assert.Equal(t, uint64(localInfo.Size()), cfsSize, "file size")
	verifySize := 1024 * 1024
	for off := int64(0); off < int64(cfsSize); off += int64(verifySize) {
		if err = verifyLocalAndCFS(localFile, ec, inodeInfo.Inode, off, verifySize); err != nil {
			log.LogFlush()
			panic(err)
		}
	}
	if err = ec.Flush(context.Background(), inodeInfo.Inode); err != nil {
		panic(err)
	}
	ec.CloseStream(context.Background(), inodeInfo.Inode)
	localFile.Close()
}

func randOffset(fileSize int64) (off int64, size int) {
	off = rand.Int63n(fileSize)
	size = rand.Intn(256 * 1024)
	return
}

func writeLocalAndCFS(localF *os.File, ec *ExtentClient, inoID uint64, offset int64, size int) error {
	writeBytes := randTestData(size)
	localF.WriteAt(writeBytes, offset)
	n, _, err := ec.Write(context.Background(), inoID, uint64(offset), writeBytes, false)
	if err != nil || n != size {
		return fmt.Errorf("write file err(%v) write off(%v) size(%v)", err, offset, size)
	}
	return nil
}

func verifyLocalAndCFS(localF *os.File, ec *ExtentClient, inoID uint64, offset int64, size int) error {
	readLocalData := make([]byte, size)
	readCFSData := make([]byte, size)
	n1, err1 := localF.ReadAt(readLocalData, offset)
	n2, _, err2 := ec.Read(context.Background(), inoID, readCFSData, uint64(offset), size)
	if err1 != err2 || n1 != n2 {
		return fmt.Errorf("read file off(%v) size(%v) cfs(%v %v) local(%v %v)", offset, size, n2, err2, n1, err1)
	}
	incorrectBegin, incorrectEnd := n1, -1
	for j := 0; j < n1; j++ {
		if readLocalData[j] != readCFSData[j] {
			if incorrectBegin > j {
				incorrectBegin = j
			}
			if incorrectEnd < j {
				incorrectEnd = j
			}
		}
	}
	if incorrectEnd != -1 {
		return fmt.Errorf("read offset(%v) size(%v) cfs(%v %v) local(%v %v) incorrect(%v~%v)\n",
			offset, size, n2, err2, n1, err1, incorrectBegin, incorrectEnd)
	}
	return nil
}

func truncateLocalAndCFS(localF *os.File, ec *ExtentClient, inoID uint64, truncateSize int64) error {
	localF.Truncate(truncateSize)
	err := ec.Truncate(context.Background(), inoID, 0, uint64(truncateSize))
	if err != nil {
		return fmt.Errorf("truncate file ino(%v) to size(%v) err(%v)", inoID, truncateSize, err)
	}
	return nil
}
