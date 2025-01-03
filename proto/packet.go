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

package proto

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/util/buf"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/unit"
	pb "github.com/gogo/protobuf/proto"
)

var (
	GRequestID = int64(1)
	Buffers    = buf.NewBufferPool()
)

// GenerateRequestID generates the request ID.
func GenerateRequestID() int64 {
	return atomic.AddInt64(&GRequestID, 1)
}

const (
	AddrSplit = "/"
)

const (
	OpInodeGetVersion1   uint8 = 1
	OpInodeGetVersion2   uint8 = 2
	OpInodeGetCurVersion uint8 = OpInodeGetVersion2
)

// Operations
const (
	ProtoMagic           uint8 = 0xFF
	OpInitResultCode     uint8 = 0x00
	OpCreateExtent       uint8 = 0x01
	OpMarkDelete         uint8 = 0x02
	OpWrite              uint8 = 0x03
	OpRead               uint8 = 0x04
	OpStreamRead         uint8 = 0x05
	OpStreamFollowerRead uint8 = 0x06
	OpGetAllWatermarks   uint8 = 0x07
	OpEcRead                   = OpRead
	OpStreamEcRead             = OpStreamRead

	OpNotifyReplicasToRepair         uint8 = 0x08
	OpExtentRepairRead               uint8 = 0x09
	OpBroadcastMinAppliedID          uint8 = 0x0A
	OpRandomWrite                    uint8 = 0x0F
	OpGetAppliedId                   uint8 = 0x10
	OpGetPartitionSize               uint8 = 0x11
	OpSyncRandomWrite                uint8 = 0x12
	OpSyncWrite                      uint8 = 0x13
	OpReadTinyDeleteRecord           uint8 = 0x14
	OpTinyExtentRepairRead           uint8 = 0x15
	OpGetMaxExtentIDAndPartitionSize uint8 = 0x16
	OpGetAllWatermarksV2             uint8 = 0x17
	OpGetAllExtentInfo               uint8 = 0x18
	OpTinyExtentAvaliRead            uint8 = 0x19
	OpGetPersistedAppliedId          uint8 = 0x1a
	OpGetAllWatermarksV3             uint8 = 0x1b
	OpFingerprint                    uint8 = 0x1c
	OpEcTinyDelInfoRead                    = OpReadTinyDeleteRecord
	OpLockOrUnlockExtent             uint8 = 0x1d

	// Operations: Client -> MetaNode.
	OpMetaCreateInode   uint8 = 0x20
	OpMetaUnlinkInode   uint8 = 0x21
	OpMetaCreateDentry  uint8 = 0x22
	OpMetaDeleteDentry  uint8 = 0x23
	OpMetaOpen          uint8 = 0x24
	OpMetaLookup        uint8 = 0x25
	OpMetaReadDir       uint8 = 0x26
	OpMetaInodeGet      uint8 = 0x27
	OpMetaBatchInodeGet uint8 = 0x28
	OpMetaExtentsAdd    uint8 = 0x29
	OpMetaExtentsDel    uint8 = 0x2A
	OpMetaExtentsList   uint8 = 0x2B
	OpMetaUpdateDentry  uint8 = 0x2C
	OpMetaTruncate      uint8 = 0x2D
	OpMetaLinkInode     uint8 = 0x2E
	OpMetaEvictInode    uint8 = 0x2F
	OpMetaSetattr       uint8 = 0x30
	OpMetaReleaseOpen   uint8 = 0x31

	//Operations: MetaNode Leader -> MetaNode Follower
	OpMetaFreeInodesOnRaftFollower uint8 = 0x32

	OpMetaDeleteInode        uint8 = 0x33 // delete specified inode immediately and do not remove data.
	OpMetaBatchExtentsAdd    uint8 = 0x34 // for extents batch attachment
	OpMetaSetXAttr           uint8 = 0x35
	OpMetaGetXAttr           uint8 = 0x36
	OpMetaRemoveXAttr        uint8 = 0x37
	OpMetaListXAttr          uint8 = 0x38
	OpMetaBatchGetXAttr      uint8 = 0x39
	OpMetaGetAppliedID       uint8 = 0x3A
	OpMetaExtentsInsert      uint8 = 0x3B
	OpMetaInodeGetV2         uint8 = 0x3C //new op code, old(get) compatible the old client
	OpGetMetaNodeVersionInfo uint8 = 0x3D
	OpMetaGetTruncateIndex   uint8 = 0x3E

	// Operations: Master -> MetaNode
	OpCreateMetaPartition             uint8 = 0x40
	OpMetaNodeHeartbeat               uint8 = 0x41
	OpDeleteMetaPartition             uint8 = 0x42
	OpUpdateMetaPartition             uint8 = 0x43
	OpLoadMetaPartition               uint8 = 0x44
	OpDecommissionMetaPartition       uint8 = 0x45
	OpAddMetaPartitionRaftMember      uint8 = 0x46
	OpRemoveMetaPartitionRaftMember   uint8 = 0x47
	OpMetaPartitionTryToLeader        uint8 = 0x48
	OpRaftAddVirtualMetaPartition     uint8 = 0x49 // Deprecated
	OpDelVirtualMetaPartition         uint8 = 0x4A // Deprecated
	OpResetMetaPartitionRaftMember    uint8 = 0x4B
	OpAddMetaPartitionRaftLearner     uint8 = 0x4C
	OpPromoteMetaPartitionRaftLearner uint8 = 0x4D
	OpSyncVirtualMetaPartitions       uint8 = 0x4E // Deprecated
	//OpAddVirtualMetaPartition         uint8 = 0x4F // Deprecated

	OpAddVirtualMetaPartition          uint8 = 0x5A //添加虚拟mp的逻辑发生变化，防止master升级后使用旧的op code给metanode发送添加虚拟mp的请求
	OpMetaGetExtentsNoModifyAccessTime uint8 = 0x5B
	OpResetMetaRecorderRaftMember      uint8 = 0x5C

	// Operations client-->datanode
	OpRandomWriteV3     uint8 = 0x50
	OpSyncRandomWriteV3 uint8 = 0x51

	// Operations for raft recorder
	OpAddMetaPartitionRaftRecorder    uint8 = 0x52
	OpRemoveMetaPartitionRaftRecorder uint8 = 0x53
	OpCreateMetaRecorder              uint8 = 0x54
	OpDeleteMetaRecorder              uint8 = 0x55

	// Operations: Master -> DataNode
	OpCreateDataPartition             uint8 = 0x60
	OpDeleteDataPartition             uint8 = 0x61
	OpLoadDataPartition               uint8 = 0x62
	OpDataNodeHeartbeat               uint8 = 0x63
	OpReplicateFile                   uint8 = 0x64
	OpDeleteFile                      uint8 = 0x65
	OpDecommissionDataPartition       uint8 = 0x66
	OpAddDataPartitionRaftMember      uint8 = 0x67
	OpRemoveDataPartitionRaftMember   uint8 = 0x68
	OpDataPartitionTryToLeader        uint8 = 0x69
	OpSyncDataPartitionReplicas       uint8 = 0x6A
	OpAddDataPartitionRaftLearner     uint8 = 0x6B
	OpPromoteDataPartitionRaftLearner uint8 = 0x6C
	OpResetDataPartitionRaftMember    uint8 = 0x6D
	OpBatchTrashExtent                uint8 = 0x6E

	// Operations: MultipartInfo
	OpCreateMultipart   uint8 = 0x70
	OpGetMultipart      uint8 = 0x71
	OpAddMultipartPart  uint8 = 0x72
	OpRemoveMultipart   uint8 = 0x73
	OpListMultiparts    uint8 = 0x74
	OpBatchDeleteExtent uint8 = 0x75 // SDK to MetaNode

	OpMetaRecoverDeletedDentry      uint8 = 0x80
	OpMetaRecoverDeletedInode       uint8 = 0x81
	OpMetaCleanDeletedDentry        uint8 = 0x82
	OpMetaCleanDeletedInode         uint8 = 0x83
	OpMetaCleanExpiredDentry        uint8 = 0x84
	OpMetaCleanExpiredInode         uint8 = 0x85
	OpMetaLookupForDeleted          uint8 = 0x86
	OpMetaGetDeletedInode           uint8 = 0x87
	OpMetaBatchGetDeletedInode      uint8 = 0x88
	OpMetaReadDeletedDir            uint8 = 0x89
	OpMetaStatDeletedFileInfo       uint8 = 0x8A
	OpMetaBatchRecoverDeletedDentry uint8 = 0x8B
	OpMetaBatchRecoverDeletedInode  uint8 = 0x8C
	OpMetaBatchCleanDeletedDentry   uint8 = 0x8D
	OpMetaBatchCleanDeletedInode    uint8 = 0x8E

	//Operations: MetaNode Leader -> MetaNode Follower
	OpMetaBatchDeleteInode  uint8 = 0x90
	OpMetaBatchDeleteDentry uint8 = 0x91
	OpMetaBatchUnlinkInode  uint8 = 0x92
	OpMetaBatchEvictInode   uint8 = 0x93

	//inode reset
	OpMetaCursorReset     uint8 = 0x94
	OpMetaGetCmpInode     uint8 = 0x95
	OpMetaInodeMergeEks   uint8 = 0x96 //Deprecated
	OpMetaFileMigMergeEks uint8 = 0x97

	//Operations: Master -> CodecNode
	OpCodecNodeHeartbeat               uint8 = 0xA0
	OpIssueMigrationTask               uint8 = 0xA1
	OpStopMigratingByDatapartitionTask uint8 = 0xA2
	OpStopMigratingByNodeTask          uint8 = 0xA3

	//EC Operations: Master -> EcNode
	OpEcNodeHeartbeat                 uint8 = 0xB0
	OpCreateEcDataPartition           uint8 = 0xB1
	OpDeleteEcDataPartition           uint8 = 0xB2
	OpNotifyEcRepairTinyDelInfo       uint8 = 0xB3
	OpChangeEcPartitionMembers        uint8 = 0xB4
	OpUpdateEcDataPartition           uint8 = 0xB6
	OpEcWrite                         uint8 = 0xB7
	OpSyncEcWrite                     uint8 = 0xB8
	OpEcTinyRepairRead                uint8 = 0xB9
	OpPersistTinyExtentDelete         uint8 = 0xBA
	OpEcTinyDelete                    uint8 = 0xBB
	OpNotifyEcRepairOriginTinyDelInfo uint8 = 0xBC
	OpEcOriginTinyDelInfoRead         uint8 = 0xBD
	OpEcGetTinyDeletingInfo           uint8 = 0xBE
	OpEcRecordTinyDelInfo             uint8 = 0xBF
	OpEcNodeDail                      uint8 = 0xC0

	// Distributed cache related OP codes.
	OpFlashNodeHeartbeat uint8 = 0xD0
	OpCacheRead          uint8 = 0xD1
	OpCachePrepare       uint8 = 0xD2

	// Commons
	OpInodeMergeErr    uint8 = 0xF1
	OpInodeOutOfRange  uint8 = 0xF2
	OpIntraGroupNetErr uint8 = 0xF3
	OpArgMismatchErr   uint8 = 0xF4
	OpNotExistErr      uint8 = 0xF5
	OpDiskNoSpaceErr   uint8 = 0xF6
	OpDiskErr          uint8 = 0xF7
	OpErr              uint8 = 0xF8
	OpAgain            uint8 = 0xF9
	OpExistErr         uint8 = 0xFA
	OpInodeFullErr     uint8 = 0xFB
	OpTryOtherAddr     uint8 = 0xFC
	OpNotPerm          uint8 = 0xFD
	OpNotEmtpy         uint8 = 0xFE
	OpDisabled         uint8 = 0xFF
	OpOk               uint8 = 0xF0

	OpPing uint8 = 0xFF
)

const (
	WriteDeadlineTime            = 5
	ReadDeadlineTime             = 5
	SyncSendTaskDeadlineTime     = 20
	NoReadDeadlineTime           = -1
	MaxWaitFollowerRepairTime    = 60 * 5
	GetAllWatermarksDeadLineTime = 60
	MaxPacketProcessTime         = 5
	MinReadDeadlineTime          = 1
)

const (
	TinyExtentType   = 0
	NormalExtentType = 1
	AllExtentType    = 2
)

const (
	CheckPreExtentExist = 1
)

const (
	NormalCreateDataPartition         = 0
	DecommissionedCreateDataPartition = 1
)

const (
	NormalCreateMetaPartition         = 0
	DecommissionedCreateMetaPartition = 1
)

const (
	FollowerReadFlag        = 'F'
	FollowerReadForwardFlag = 'T'
)

var GReadOps = []uint8{OpMetaLookup, OpMetaReadDir, OpMetaInodeGet, OpMetaBatchInodeGet, OpMetaExtentsList, OpMetaGetXAttr,
	OpMetaListXAttr, OpMetaBatchGetXAttr, OpMetaGetAppliedID, OpGetMultipart, OpListMultiparts}

// Packet defines the packet structure.
type Packet struct {
	Magic              uint8
	ExtentType         uint8
	Opcode             uint8
	ResultCode         uint8
	RemainingFollowers uint8
	CRC                uint32
	Size               uint32
	ArgLen             uint32
	KernelOffset       uint64
	PartitionID        uint64
	ExtentID           uint64
	ExtentOffset       int64
	ReqID              int64
	Arg                []byte // for create or append ops, the data contains the address
	Data               []byte
	StartT             int64
	SendT              int64
	WaitT              int64
	RecvT              int64
	mesg               string
	PoolFlag           int64
	ctx                context.Context

	RemoteAddr string
}

// NewPacket returns a new packet.
func NewPacket(ctx context.Context) *Packet {
	p := new(Packet)
	p.Magic = ProtoMagic
	p.StartT = time.Now().UnixNano()
	p.SetCtx(ctx)

	return p
}

// NewPacketReqID returns a new packet with ReqID assigned.
func NewPacketReqID(ctx context.Context) *Packet {
	p := NewPacket(ctx)
	p.ReqID = GenerateRequestID()
	return p
}

func (p *Packet) Ctx() context.Context {
	return p.ctx
}

func (p *Packet) SetCtx(ctx context.Context) {
	p.ctx = ctx
}

func (p *Packet) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("Req(%v)Op(%v)PartitionID(%v)ResultCode(%v)", p.ReqID, p.GetOpMsg(), p.PartitionID, p.GetResultMsg())
}

func (p *Packet) IsReadOp() bool {
	for _, opCode := range GReadOps {
		if p.Opcode == opCode {
			return true
		}
	}
	return false
}

// GetStoreType returns the store type.
func (p *Packet) GetStoreType() (m string) {
	switch p.ExtentType {
	case TinyExtentType:
		m = "TinyExtent"
	case NormalExtentType:
		m = "NormalExtent"
	default:
		m = "Unknown"
	}
	return
}

func (p *Packet) GetOpMsgWithReqAndResult() (m string) {
	return fmt.Sprintf("Req(%v)_(%v)_Result(%v)_Body(%v)", p.ReqID, p.GetOpMsg(), p.GetResultMsg(), string(p.Data[0:p.Size]))
}

// GetOpMsg returns the operation type.
func (p *Packet) GetOpMsg() (m string) {
	return GetOpMsg(p.Opcode)
}

func GetOpMsg(opcode uint8) (m string) {
	switch opcode {
	case OpCreateExtent:
		m = "OpCreateExtent"
	case OpMarkDelete:
		m = "OpMarkDelete"
	case OpWrite:
		m = "OpWrite"
	case OpRandomWrite:
		m = "OpRandomWrite"
	case OpRead:
		m = "OpRead"
	case OpStreamRead:
		m = "OpStreamRead"
	case OpStreamFollowerRead:
		m = "OpStreamFollowerRead"
	case OpGetAllWatermarks:
		m = "OpGetAllWatermarks"
	case OpGetAllWatermarksV2:
		m = "OpGetAllWatermarksV2"
	case OpGetAllWatermarksV3:
		m = "OpGetAllWatermarksV3"
	case OpNotifyReplicasToRepair:
		m = "OpNotifyReplicasToRepair"
	case OpExtentRepairRead:
		m = "OpExtentRepairRead"
	case OpTinyExtentAvaliRead:
		m = "OpTinyExtentAvaliRead"
	case OpInodeOutOfRange:
		m = "InodeOutOfRange"
	case OpIntraGroupNetErr:
		m = "IntraGroupNetErr"
	case OpMetaCreateInode:
		m = "OpMetaCreateInode"
	case OpMetaUnlinkInode:
		m = "OpMetaUnlinkInode"
	case OpMetaBatchUnlinkInode:
		m = "OpMetaBatchUnlinkInode"
	case OpMetaCreateDentry:
		m = "OpMetaCreateDentry"
	case OpMetaDeleteDentry:
		m = "OpMetaDeleteDentry"
	case OpMetaBatchDeleteDentry:
		m = "OpMetaBatchDeleteDentry"
	case OpMetaOpen:
		m = "OpMetaOpen"
	case OpMetaReleaseOpen:
		m = "OpMetaReleaseOpen"
	case OpMetaLookup:
		m = "OpMetaLookup"
	case OpMetaReadDir:
		m = "OpMetaReadDir"
	case OpMetaInodeGet:
		m = "OpMetaInodeGet"
	case OpMetaInodeGetV2:
		m = "OpMetaInodeGetV2"
	case OpMetaBatchInodeGet:
		m = "OpMetaBatchInodeGet"
	case OpMetaExtentsAdd:
		m = "OpMetaExtentsAdd"
	case OpMetaExtentsInsert:
		m = "OpMetaExtentsInsert"
	case OpMetaExtentsDel:
		m = "OpMetaExtentsDel"
	case OpMetaExtentsList:
		m = "OpMetaExtentsList"
	case OpMetaUpdateDentry:
		m = "OpMetaUpdateDentry"
	case OpMetaTruncate:
		m = "OpMetaTruncate"
	case OpMetaLinkInode:
		m = "OpMetaLinkInode"
	case OpMetaEvictInode:
		m = "OpMetaEvictInode"
	case OpMetaBatchEvictInode:
		m = "OpMetaBatchEvictInode"
	case OpMetaSetattr:
		m = "OpMetaSetattr"
	case OpCreateMetaPartition:
		m = "OpCreateMetaPartition"
	case OpMetaNodeHeartbeat:
		m = "OpMetaNodeHeartbeat"
	case OpDeleteMetaPartition:
		m = "OpDeleteMetaPartition"
	case OpUpdateMetaPartition:
		m = "OpUpdateMetaPartition"
	case OpLoadMetaPartition:
		m = "OpLoadMetaPartition"
	case OpDecommissionMetaPartition:
		m = "OpDecommissionMetaPartition"
	case OpCreateDataPartition:
		m = "OpCreateDataPartition"
	case OpDeleteDataPartition:
		m = "OpDeleteDataPartition"
	case OpLoadDataPartition:
		m = "OpLoadDataPartition"
	case OpDecommissionDataPartition:
		m = "OpDecommissionDataPartition"
	case OpDataNodeHeartbeat:
		m = "OpDataNodeHeartbeat"
	case OpReplicateFile:
		m = "OpReplicateFile"
	case OpDeleteFile:
		m = "OpDeleteFile"
	case OpGetAppliedId:
		m = "OpGetAppliedId"
	case OpGetPersistedAppliedId:
		m = "OpGetPersistedAppliedId"
	case OpGetPartitionSize:
		m = "OpGetPartitionSize"
	case OpSyncWrite:
		m = "OpSyncWrite"
	case OpSyncRandomWrite:
		m = "OpSyncRandomWrite"
	case OpReadTinyDeleteRecord:
		m = "OpReadTinyDeleteRecord"
	case OpPing:
		m = "OpPing"
	case OpTinyExtentRepairRead:
		m = "OpTinyExtentRepairRead"
	case OpGetMaxExtentIDAndPartitionSize:
		m = "OpGetMaxExtentIDAndPartitionSize"
	case OpBroadcastMinAppliedID:
		m = "OpBroadcastMinAppliedID"
	case OpRemoveDataPartitionRaftMember:
		m = "OpRemoveDataPartitionRaftMember"
	case OpAddDataPartitionRaftMember:
		m = "OpAddDataPartitionRaftMember"
	case OpAddDataPartitionRaftLearner:
		m = "OpAddDataPartitionRaftLearner"
	case OpPromoteDataPartitionRaftLearner:
		m = "OpPromoteDataPartitionRaftLearner"
	case OpResetDataPartitionRaftMember:
		m = "OpResetDataPartitionRaftMember"
	case OpAddMetaPartitionRaftMember:
		m = "OpAddMetaPartitionRaftMember"
	case OpRemoveMetaPartitionRaftMember:
		m = "OpRemoveMetaPartitionRaftMember"
	case OpAddMetaPartitionRaftLearner:
		m = "OpAddMetaPartitionRaftLearner"
	case OpPromoteMetaPartitionRaftLearner:
		m = "OpPromoteMetaPartitionRaftLearner"
	case OpResetMetaPartitionRaftMember:
		m = "OpResetMetaPartitionRaftMember"
	case OpCreateMetaRecorder:
		m = "OpCreateMetaRecorder"
	case OpDeleteMetaRecorder:
		m = "OpDeleteMetaRecorder"
	case OpAddMetaPartitionRaftRecorder:
		m = "OpAddMetaPartitionRaftRecorder"
	case OpRemoveMetaPartitionRaftRecorder:
		m = "OpRemoveMetaPartitionRaftRecorder"
	case OpResetMetaRecorderRaftMember:
		m = "OpResetMetaRecorderRaftMember"
	case OpMetaPartitionTryToLeader:
		m = "OpMetaPartitionTryToLeader"
	case OpDataPartitionTryToLeader:
		m = "OpDataPartitionTryToLeader"
	case OpMetaDeleteInode:
		m = "OpMetaDeleteInode"
	case OpMetaBatchDeleteInode:
		m = "OpMetaBatchDeleteInode"
	case OpMetaBatchExtentsAdd:
		m = "OpMetaBatchExtentsAdd"
	case OpMetaSetXAttr:
		m = "OpMetaSetXAttr"
	case OpMetaGetXAttr:
		m = "OpMetaGetXAttr"
	case OpMetaRemoveXAttr:
		m = "OpMetaRemoveXAttr"
	case OpMetaListXAttr:
		m = "OpMetaListXAttr"
	case OpMetaBatchGetXAttr:
		m = "OpMetaBatchGetXAttr"
	case OpCreateMultipart:
		m = "OpCreateMultipart"
	case OpGetMultipart:
		m = "OpGetMultipart"
	case OpAddMultipartPart:
		m = "OpAddMultipartPart"
	case OpRemoveMultipart:
		m = "OpRemoveMultipart"
	case OpListMultiparts:
		m = "OpListMultiparts"
	case OpBatchDeleteExtent:
		m = "OpBatchDeleteExtent"
	case OpBatchTrashExtent:
		m = "OpBatchTrashExtent"
	case OpMetaCursorReset:
		m = "OpMetaCursorReset"
	case OpSyncDataPartitionReplicas:
		m = "OpSyncDataPartitionReplicas"
	case OpMetaGetAppliedID:
		m = "OpMetaGetAppliedID"
	case OpMetaGetTruncateIndex:
		m = "OpMetaGetTruncateIndex"
	case OpRandomWriteV3:
		m = "OpRandomWriteV3"
	case OpFingerprint:
		m = "OpFingerprint"
	case OpSyncRandomWriteV3:
		m = "OpSyncRandomWriteV3"
	case OpMetaRecoverDeletedDentry:
		m = "OpMetaRecoverDeletedDentry"
	case OpMetaBatchRecoverDeletedDentry:
		m = "OpMetaBatchRecoverDeletedDentry"
	case OpMetaRecoverDeletedInode:
		m = "OpMetaRecoverDeletedInode"
	case OpMetaBatchRecoverDeletedInode:
		m = "OpMetaBatchRecoverDeletedInode"
	case OpMetaCleanExpiredInode:
		m = "OpMetaCleanExpiredInode"
	case OpMetaCleanExpiredDentry:
		m = "OpMetaCleanExpiredDentry"
	case OpMetaCleanDeletedInode:
		m = "OpMetaCleanDeletedInode"
	case OpMetaBatchCleanDeletedInode:
		m = "OpMetaBatchCleanDeletedInode"
	case OpMetaCleanDeletedDentry:
		m = "OpMetaCleanDeletedDentry"
	case OpMetaBatchCleanDeletedDentry:
		m = "OpMetaBatchCleanDeletedDentry"
	case OpMetaGetDeletedInode:
		m = "OpMetaGetDeletedInode"
	case OpMetaBatchGetDeletedInode:
		m = "OpMetaBatchGetDeletedInode"
	case OpMetaLookupForDeleted:
		m = "OpMetaLookupForDeleted"
	case OpMetaReadDeletedDir:
		m = "OpMetaReadDeletedDir"
	case OpMetaStatDeletedFileInfo:
		m = "OpMetaStatDeletedFileInfo"
	case OpMetaGetCmpInode:
		m = "OpMetaGetCmpInode"
	case OpMetaInodeMergeEks:
		m = "OpMetaInodeMergeEks"
	case OpFlashNodeHeartbeat:
		m = "OpFlashNodeHeartbeat"
	case OpCacheRead:
		m = "OpCacheRead"
	case OpCachePrepare:
		m = "OpCachePrepare"
	case OpLockOrUnlockExtent:
		m = "OpLockOrUnlockExtent"
	case OpGetAllExtentInfo:
		m = "OpGetAllExtentInfo"
	case OpMetaGetExtentsNoModifyAccessTime:
		m = "OpMetaGetExtentsNoModifyAccessTime"
	case OpMetaFileMigMergeEks:
		m = "OpMetaFileMigMergeEks"
	}
	return
}

// GetResultMsg returns the result message.
func (p *Packet) GetResultMsg() (m string) {
	if p == nil {
		return ""
	}
	m = GetResultMsg(p.ResultCode)
	resp := p.GetRespData()
	if p.ResultCode != OpInitResultCode && p.ResultCode != OpOk && len(resp) > 0 {
		m = fmt.Sprintf("%v: %v", m, resp)
	}
	return
}

func GetResultMsg(resultCode uint8) (m string) {
	switch resultCode {
	case OpInitResultCode:
		m = "init"
	case OpInodeOutOfRange:
		m = "Inode Out of Range"
	case OpIntraGroupNetErr:
		m = "IntraGroupNetErr"
	case OpDiskNoSpaceErr:
		m = "DiskNoSpaceErr"
	case OpDiskErr:
		m = "DiskErr"
	case OpErr:
		m = "Err"
	case OpAgain:
		m = "Again"
	case OpOk:
		m = "Ok"
	case OpExistErr:
		m = "ExistErr"
	case OpInodeFullErr:
		m = "InodeFullErr"
	case OpArgMismatchErr:
		m = "ArgUnmatchErr"
	case OpNotExistErr:
		m = "NotExistErr"
	case OpTryOtherAddr:
		m = "TryOtherAddr"
	case OpNotPerm:
		m = "NotPerm"
	case OpNotEmtpy:
		m = "DirNotEmpty"
	case OpDisabled:
		m = "Disabled"
	case OpInodeMergeErr:
		m = "OpInodeMergeErr"
	default:
		return fmt.Sprintf("Unknown ResultCode(%v)", resultCode)
	}
	return
}

func (p *Packet) GetReqID() int64 {
	return p.ReqID
}

func (p *Packet) GetRespData() (msg string) {
	if len(p.Data) > 0 && p.Size <= uint32(len(p.Data)) {
		msgLen := unit.Min(int(p.Size), 512)
		msg = string(p.Data[:msgLen])
	}
	return msg
}

// MarshalHeader marshals the packet header.
func (p *Packet) MarshalHeader(out []byte) {
	if IsDbBack {
		p.marshalHeaderForDbbak(out)
	} else {
		p.marshalHeader(out)
	}
}

func (p *Packet) marshalHeader(out []byte) {
	out[0] = p.Magic
	out[1] = p.ExtentType
	out[2] = p.Opcode
	out[3] = p.ResultCode
	out[4] = p.RemainingFollowers
	binary.BigEndian.PutUint32(out[5:9], p.CRC)
	binary.BigEndian.PutUint32(out[9:13], p.Size)
	binary.BigEndian.PutUint32(out[13:17], p.ArgLen)
	binary.BigEndian.PutUint64(out[17:25], p.PartitionID)
	binary.BigEndian.PutUint64(out[25:33], p.ExtentID)
	binary.BigEndian.PutUint64(out[33:41], uint64(p.ExtentOffset))
	binary.BigEndian.PutUint64(out[41:49], uint64(p.ReqID))
	binary.BigEndian.PutUint64(out[49:unit.PacketHeaderSize], p.KernelOffset)
}

func (p *Packet) marshalHeaderForDbbak(out []byte) {
	out[0] = p.Magic
	out[1] = p.ExtentType
	out[2] = p.Opcode
	out[3] = p.ResultCode
	out[4] = p.RemainingFollowers
	binary.BigEndian.PutUint32(out[5:9], p.CRC)
	binary.BigEndian.PutUint32(out[9:13], p.Size)
	binary.BigEndian.PutUint32(out[13:17], p.ArgLen)
	binary.BigEndian.PutUint32(out[17:21], uint32(p.PartitionID))
	binary.BigEndian.PutUint64(out[21:29], p.ExtentID)
	binary.BigEndian.PutUint64(out[29:37], uint64(p.ExtentOffset))
	binary.BigEndian.PutUint64(out[37:unit.PacketHeaderSizeForDbbak], uint64(p.ReqID))
}

// UnmarshalHeader unmarshals the packet header.
func (p *Packet) UnmarshalHeader(in []byte) error {
	p.Magic = in[0]
	if p.Magic != ProtoMagic {
		return errors.New("Bad Magic " + strconv.Itoa(int(p.Magic)))
	}
	if IsDbBack {
		return p.unmarshalHeaderForDbbak(in)
	} else {
		return p.unmarshalHeader(in)
	}
}

func (p *Packet) unmarshalHeader(in []byte) error {
	p.ExtentType = in[1]
	p.Opcode = in[2]
	p.ResultCode = in[3]
	p.RemainingFollowers = in[4]
	p.CRC = binary.BigEndian.Uint32(in[5:9])
	p.Size = binary.BigEndian.Uint32(in[9:13])
	p.ArgLen = binary.BigEndian.Uint32(in[13:17])
	p.PartitionID = binary.BigEndian.Uint64(in[17:25])
	p.ExtentID = binary.BigEndian.Uint64(in[25:33])
	p.ExtentOffset = int64(binary.BigEndian.Uint64(in[33:41]))
	p.ReqID = int64(binary.BigEndian.Uint64(in[41:49]))
	p.KernelOffset = binary.BigEndian.Uint64(in[49:unit.PacketHeaderSize])

	return nil
}

func (p *Packet) unmarshalHeaderForDbbak(in []byte) error {
	p.ExtentType = in[1]
	p.Opcode = in[2]
	p.ResultCode = in[3]
	p.RemainingFollowers = in[4]
	p.CRC = binary.BigEndian.Uint32(in[5:9])
	p.Size = binary.BigEndian.Uint32(in[9:13])
	p.ArgLen = binary.BigEndian.Uint32(in[13:17])
	p.PartitionID = uint64(binary.BigEndian.Uint32(in[17:21]))
	p.ExtentID = binary.BigEndian.Uint64(in[21:29])
	p.ExtentOffset = int64(binary.BigEndian.Uint64(in[29:37]))
	p.ReqID = int64(binary.BigEndian.Uint64(in[37:unit.PacketHeaderSizeForDbbak]))

	return nil
}

// MarshalData marshals the packet data.
func (p *Packet) MarshalData(v interface{}) error {
	data, err := json.Marshal(v)
	if err == nil {
		p.Data = data
		p.Size = uint32(len(p.Data))
	}
	return err
}

// UnmarshalData unmarshals the packet data.
func (p *Packet) UnmarshalData(v interface{}) error {
	return json.Unmarshal(p.Data, v)
}

func (p *Packet) MarshalDataPb(m pb.Message) error {
	data, err := pb.Marshal(m)
	if err == nil {
		p.Data = data
		p.Size = uint32(len(p.Data))
		//p.CRC = crc32.ChecksumIEEE(p.Data[:p.Size])
	}
	return err
}

func (p *Packet) UnmarshalDataPb(m pb.Message) error {
	return pb.Unmarshal(p.Data, m)
}

// WriteToNoDeadLineConn writes through the connection without deadline.
func (p *Packet) WriteToNoDeadLineConn(c net.Conn) (err error) {
	header, err := Buffers.Get(PacketHeaderSize())
	if err != nil {
		header = make([]byte, PacketHeaderSize())
	}
	defer Buffers.Put(header)

	p.MarshalHeader(header)
	if _, err = c.Write(header); err == nil {
		if _, err = c.Write(p.Arg[:int(p.ArgLen)]); err == nil {
			if p.Data != nil {
				_, err = c.Write(p.Data[:p.Size])
			}
		}
	}

	return
}

// WriteToConn writes through the given connection.
func (p *Packet) WriteToConn(c net.Conn, timeoutSec int) (err error) {
	if timeoutSec > 0 {
		c.SetWriteDeadline(time.Now().Add(time.Second * time.Duration(timeoutSec)))
	}

	return p.writeToConn(c)
}

// WriteToConn writes through the given connection.
func (p *Packet) WriteToConnNs(c net.Conn, timeoutNs int64) (err error) {
	if timeoutNs > 0 {
		c.SetWriteDeadline(time.Now().Add(time.Nanosecond * time.Duration(timeoutNs)))
	}

	return p.writeToConn(c)
}

func (p *Packet) writeToConn(c net.Conn) (err error) {
	header, err := Buffers.Get(PacketHeaderSize())
	if err != nil {
		header = make([]byte, PacketHeaderSize())
	}
	defer Buffers.Put(header)
	p.MarshalHeader(header)
	if _, err = c.Write(header); err == nil {
		if _, err = c.Write(p.Arg[:int(p.ArgLen)]); err == nil {
			if p.Data != nil && p.Size != 0 {
				_, err = c.Write(p.Data[:p.Size])
			}
		}
	}

	return
}

// ReadFull is a wrapper function of io.ReadFull.
func ReadFull(c net.Conn, buf *[]byte, readSize int) (err error) {
	*buf = make([]byte, readSize)
	_, err = io.ReadFull(c, (*buf)[:readSize])
	return
}

// ReadFromConn reads the data from the given connection.
func (p *Packet) ReadFromConn(c net.Conn, timeoutSec int) (err error) {
	if timeoutSec != NoReadDeadlineTime {
		c.SetReadDeadline(time.Now().Add(time.Second * time.Duration(timeoutSec)))
	} else {
		c.SetReadDeadline(time.Time{})
	}
	return p.readFromConn(c)
}

func (p *Packet) ReadFromConnNs(c net.Conn, timeoutNs int64) (err error) {
	if timeoutNs != NoReadDeadlineTime {
		c.SetReadDeadline(time.Now().Add(time.Nanosecond * time.Duration(timeoutNs)))
	} else {
		c.SetReadDeadline(time.Time{})
	}
	return p.readFromConn(c)
}

func (p *Packet) readFromConn(c net.Conn) (err error) {
	header, err := Buffers.Get(PacketHeaderSize())
	if err != nil {
		header = make([]byte, PacketHeaderSize())
	}
	defer Buffers.Put(header)
	var n int
	if n, err = io.ReadFull(c, header); err != nil {
		return
	}
	if n != PacketHeaderSize() {
		return syscall.EBADMSG
	}
	if err = p.UnmarshalHeader(header); err != nil {
		return
	}

	if p.ArgLen > 0 {
		p.Arg = make([]byte, int(p.ArgLen))
		if _, err = io.ReadFull(c, p.Arg[:int(p.ArgLen)]); err != nil {
			return err
		}
	}

	size := p.Size
	if (p.Opcode == OpRead || p.Opcode == OpStreamRead || p.Opcode == OpExtentRepairRead || p.Opcode == OpStreamFollowerRead) && p.ResultCode == OpInitResultCode {
		size = 0
	}
	p.Data = make([]byte, size)
	if n, err = io.ReadFull(c, p.Data[:size]); err != nil {
		return err
	}
	if n != int(size) {
		return syscall.EBADMSG
	}
	return nil
}

// PacketOkReply sets the result code as OpOk, and sets the body as empty.
func (p *Packet) PacketOkReply() {
	p.ResultCode = OpOk
	p.Size = 0
	p.Data = nil
	p.ArgLen = 0
}

// PacketOkWithBody sets the result code as OpOk, and sets the body with the give data.
func (p *Packet) PacketOkWithBody(reply []byte) {
	p.Size = uint32(len(reply))
	p.Data = make([]byte, p.Size)
	copy(p.Data[:p.Size], reply)
	p.ResultCode = OpOk
	p.ArgLen = 0
}

// PacketErrorWithBody sets the packet with error code whose body is filled with the given data.
func (p *Packet) PacketErrorWithBody(code uint8, reply []byte) {
	p.Size = uint32(len(reply))
	p.Data = make([]byte, p.Size)
	copy(p.Data[:p.Size], reply)
	p.ResultCode = code
	p.ArgLen = 0
}

func (p *Packet) prepareMesg() {
	if p.mesg == "" {
		p.mesg = p.getPacketCommonLog()
	}
}

func (p *Packet) AddMesgLog(m string) {
	p.prepareMesg()
	p.mesg += m
}

// GetUniqueLogId returns the unique log ID.
func (p *Packet) GetUniqueLogId() (m string) {
	defer func() {
		if p.PoolFlag == 0 {
			m = m + fmt.Sprintf("_ResultMesg(%v)", p.GetResultMsg())
		} else {
			m = m + fmt.Sprintf("_ResultMesg(%v)_PoolFLag(%v)", p.GetResultMsg(), p.PoolFlag)
		}
	}()
	p.prepareMesg()
	m = p.mesg
	return
}

func (p *Packet) getPacketCommonLog() (m string) {
	const pattern = "Req(%v)_Opcode(%v)_Partition(%v)_Extent(%v)_ExtentOffset(%v)_KernelOffset(%v)_Size(%v)_CRC(%v)_Remote(%v)"
	remoteAddr := p.RemoteAddr
	if remoteAddr == "" {
		remoteAddr = "Unknown"
	}
	return fmt.Sprintf(pattern, p.ReqID, p.GetOpMsg(), p.PartitionID, p.ExtentID, p.ExtentOffset, p.KernelOffset, p.Size, p.CRC, remoteAddr)
}

// IsForwardPkt returns if the packet is the forward packet (a packet that will be forwarded to the followers).
func (p *Packet) IsForwardPkt() bool {
	return p.RemainingFollowers > 0
}

// LogMessage logs the given message.
func (p *Packet) LogMessage(action, remote string, start int64, err error) (m string) {
	if err == nil {
		m = fmt.Sprintf("action[%v] id[%v] isPrimaryBackReplLeader[%v] remote[%v] "+
			" cost[%v]ms ", action, p.GetUniqueLogId(), p.IsForwardPkt(), remote, (time.Now().UnixNano()-start)/1e6)

	} else {
		m = fmt.Sprintf("action[%v] id[%v] isPrimaryBackReplLeader[%v] remote[%v]"+
			", err[%v]", action, p.GetUniqueLogId(), p.IsForwardPkt(), remote, err.Error())
	}

	return
}

// ShallRetry returns if we should retry the packet.
// As meta can not reentrant the unlink op, so unlink can not retry.
// Meta can not reentran ops [create dentry\ update dentry\ create indoe\ link inode\ unlink inode\]
func (p *Packet) ShouldRetry() bool {
	if p.ResultCode == OpTryOtherAddr {
		return true
	}
	return p.Opcode != OpMetaUnlinkInode && (p.ResultCode == OpAgain || p.ResultCode == OpErr)
}

func (p *Packet) IsReadMetaPkt() bool {
	if p.Opcode == OpMetaLookup || p.Opcode == OpMetaInodeGet || p.Opcode == OpMetaInodeGetV2 || p.Opcode == OpMetaBatchInodeGet ||
		p.Opcode == OpMetaReadDir || p.Opcode == OpMetaExtentsList || p.Opcode == OpGetMultipart ||
		p.Opcode == OpMetaGetXAttr || p.Opcode == OpMetaListXAttr || p.Opcode == OpListMultiparts ||
		p.Opcode == OpMetaBatchGetXAttr || p.Opcode == OpMetaLookupForDeleted || p.Opcode == OpMetaGetDeletedInode ||
		p.Opcode == OpMetaBatchGetDeletedInode || p.Opcode == OpMetaReadDeletedDir {
		return true
	}
	return false
}

func (p *Packet) IsRandomWrite() bool {
	if p.Opcode == OpRandomWriteV3 || p.Opcode == OpSyncRandomWriteV3 || p.Opcode == OpRandomWrite || p.Opcode == OpSyncRandomWrite {
		return true
	}
	return false
}

func (p *Packet) IsFollowerReadMetaPkt() bool {
	if p.ArgLen >= 1 && p.Arg[0] == FollowerReadFlag {
		return true
	}
	return false
}

func (p *Packet) SetFollowerReadMetaPkt() {
	p.ArgLen = 1
	p.Arg = make([]byte, p.ArgLen)
	p.Arg[0] = FollowerReadFlag
}

func (p *Packet) IsForwardFollowerReadMetaPkt() bool {
	if p.ArgLen >= 2 && p.Arg[0] == FollowerReadFlag && p.Arg[1] == FollowerReadForwardFlag {
		return true
	}
	return false
}

func (p *Packet) ResetPktDataForFollowerReadForward() {
	if p.ArgLen >= 2 {
		p.Arg[1] = FollowerReadForwardFlag
	} else {
		p.Arg = append(p.Arg, FollowerReadForwardFlag)
		p.ArgLen++
	}
}

func (p *Packet) ShallTryToLeader() bool {
	if p.Opcode == OpMetaReadDir || p.Opcode == OpMetaBatchInodeGet || p.Opcode == OpMetaExtentsList || p.Opcode == OpMetaBatchExtentsAdd ||
		p.Opcode == OpMetaBatchGetXAttr || p.Opcode == OpMetaBatchGetDeletedInode || p.Opcode == OpMetaBatchRecoverDeletedDentry ||
		p.Opcode == OpMetaBatchRecoverDeletedInode || p.Opcode == OpMetaBatchCleanDeletedInode || p.Opcode == OpMetaBatchCleanDeletedDentry ||
		p.Opcode == OpMetaBatchDeleteInode || p.Opcode == OpMetaBatchDeleteDentry || p.Opcode == OpMetaBatchUnlinkInode ||
		p.Opcode == OpMetaBatchEvictInode {
		return false
	}

	return true
}

func PacketHeaderSize() int {
	if IsDbBack {
		return unit.PacketHeaderSizeForDbbak
	} else {
		return unit.PacketHeaderSize
	}
}

func NewPacketToGetAllExtentInfo(ctx context.Context, partitionID uint64) (p *Packet) {
	p = new(Packet)
	p.Opcode = OpGetAllExtentInfo
	p.PartitionID = partitionID
	p.Magic = ProtoMagic
	p.ReqID = GenerateRequestID()
	p.SetCtx(ctx)
	return
}

func (p *Packet) ParseRequestSource() (source RequestSource) {
	if len(p.Arg) > 1 {
		return RequestSource(p.Arg[1])
	}
	return SourceBase
}
