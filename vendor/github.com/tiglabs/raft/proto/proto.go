// Copyright 2018 The tiglabs raft Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proto

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
)

type (
	MsgType        byte
	EntryType      byte
	ConfChangeType byte
	PeerType       byte
	AckType        byte
)

const (
	ReqMsgAppend MsgType = iota
	ReqMsgVote
	ReqMsgHeartBeat
	ReqMsgSnapShot
	ReqMsgElectAck
	RespMsgAppend
	RespMsgVote
	RespMsgHeartBeat
	RespMsgSnapShot
	RespMsgElectAck
	LocalMsgHup
	LocalMsgProp
	LeaseMsgOffline
	LeaseMsgTimeout
	ReqCheckQuorum
	RespCheckQuorum
	ReqCompleteEntry		// candidate -> recorder
	RespCompleteEntry		// recorder -> candidate
	ReqMsgGetApplyIndex		// recorder -> peers
	RespMsgGetApplyIndex	// peers -> recorder
)

const (
	ConfAddNode        ConfChangeType = 0
	ConfRemoveNode     ConfChangeType = 1
	ConfUpdateNode     ConfChangeType = 2
	ConfAddLearner     ConfChangeType = 3
	ConfPromoteLearner ConfChangeType = 4
	ConfAddRecorder    ConfChangeType = 5
	ConfRemoveRecorder ConfChangeType = 6

	EntryNormal     EntryType = 0
	EntryConfChange EntryType = 1
	EntryRollback   EntryType = 2

	PeerNormal  	PeerType = 0
	PeerArbiter 	PeerType = 1
	PeerRecorder	PeerType = 2

	LearnerProgress = 90
)

const (
	AckTypeCommitted AckType = 0
	AckTypeApplied   AckType = 1
)

// The Snapshot interface is supplied by the application to access the snapshot data of application.
type Snapshot interface {
	SnapIterator
	ApplyIndex() uint64
	Close()
	Version() uint32
}

type SnapIterator interface {
	// if error=io.EOF represent snapshot terminated.
	Next() ([]byte, error)
}

type SnapshotMeta struct {
	Index    uint64
	Term     uint64
	Peers    []Peer
	Learners []Learner
	SnapV    uint32
}

type Peer struct {
	Type     PeerType
	Priority uint16
	ID       uint64 // NodeID
	PeerID   uint64 // Replica ID, unique over all raft groups and all replicas in the same group
}

func (p *Peer) IsRecorder() bool {
	return p.Type == PeerRecorder
}

type Learner struct {
	ID         uint64         `json:"id"` // NodeID
	PromConfig *PromoteConfig `json:"promote_config"`
}

// HardState is the repl state,must persist to the storage.
type HardState struct {
	Term   uint64
	Commit uint64
	Vote   uint64
}

var (
	LeaderGetEntryCnt   uint64
	LeaderPutEntryCnt   uint64
	FollowerGetEntryCnt uint64
	FollowerPutEntryCnt uint64
)

func LoadLeaderGetEntryCnt() uint64 {
	return atomic.LoadUint64(&LeaderGetEntryCnt)
}

func LoadLeaderPutEntryCnt() uint64 {
	return atomic.LoadUint64(&LeaderPutEntryCnt)
}

func LoadFollowerGetEntryCnt() uint64 {
	return atomic.LoadUint64(&FollowerGetEntryCnt)
}

func LoadFollowerPutEntryCnt() uint64 {
	return atomic.LoadUint64(&FollowerPutEntryCnt)
}

const (
	LeaderLogEntryRefCnt   = 4
	FollowerLogEntryRefCnt = 2
)

// Entry is the repl log entry.
type Entry struct {
	Type      EntryType
	Term      uint64
	Index     uint64
	Data      []byte
	ctx       context.Context // Tracer context
	RefCnt    int32
	OrgRefCnt uint8
}

func (e *Entry) SetCtx(ctx context.Context) {
	e.ctx = ctx
}

func (e *Entry) IsLeaderLogEntry() bool {
	return e.OrgRefCnt > MinLeaderLogEntryRefCnt
}

func (e *Entry) IsFollowerLogEntry() bool {
	return e.OrgRefCnt == FollowerLogEntryRefCnt
}

func (e *Entry) DecRefCnt() {
	if e.IsLeaderLogEntry() || e.IsFollowerLogEntry() {
		atomic.AddInt32(&e.RefCnt, -1)
	}
}

func (e *Entry) Ctx() context.Context {
	return e.ctx
}

type FailureListener func(err error)

func (ln FailureListener) OnFailure(err error) {
	if ln != nil {
		ln(err)
	}
}

// Message is the transport message.
type Message struct {
	Type         MsgType
	ForceVote    bool
	Reject       bool
	RejectIndex  uint64
	ID           uint64
	From         uint64
	To           uint64
	Term         uint64
	LogTerm      uint64
	Index        uint64
	Commit       uint64
	SnapshotMeta SnapshotMeta
	Entries      []*Entry
	Context      []byte
	Snapshot     Snapshot // No need for codec
	ctx          context.Context
	magic        uint8

	HeartbeatContext HeartbeatContext
}

func (m *Message) Ctx() context.Context {
	return m.ctx
}
func (m *Message) SetCtx(ctx context.Context) {
	m.ctx = ctx
}

func (m *Message) ToString() (mesg string) {
	return fmt.Sprintf("Mesg:[%v] type(%v) ForceVote(%v) Reject(%v) RejectIndex(%v) "+
		"From(%v) To(%v) Term(%v) LogTrem(%v) Index(%v) Commit(%v) EntryLen(%v)", m.ID, m.Type.String(), m.ForceVote,
		m.Reject, m.RejectIndex, m.From, m.To, m.Term, m.LogTerm, m.Index, m.Commit, len(m.Entries))
}

type ConfChange struct {
	Type    ConfChangeType
	Peer    Peer
	Context []byte
}

type Rollback struct {
	Index uint64 // Index of Entry to be rollback.
	Data  []byte
}

type PromoteConfig struct {
	PromThreshold uint8 `json:"prom_threshold"`
	AutoPromote   bool  `json:"auto_prom"`
}

type ConfChangeLearnerReq struct {
	Id            uint64  `json:"pid"`
	ChangeLearner Learner `json:"learner"`
}
type ResetPeers struct {
	Peers    []Peer
	Learners []Learner
	Context  []byte
}

type ContextInfo struct {
	ID         uint64
	IsUnstable bool
}

type HeartbeatContext []ContextInfo

func (ctx HeartbeatContext) Get(id uint64) (e ContextInfo, exist bool) {
	if len(ctx) == 0 {
		return
	}
	var i = sort.Search(len(ctx), func(i int) bool {
		return ctx[i].ID >= id
	})
	if i >= 0 && i < len(ctx) {
		e = ctx[i]
		exist = true
		return
	}
	return
}

func (t MsgType) String() string {
	switch t {
	case ReqMsgAppend:
		return "ReqMsgAppend"
	case ReqMsgVote:
		return "ReqMsgVote"
	case ReqMsgHeartBeat:
		return "ReqMsgHeartBeat"
	case ReqMsgSnapShot:
		return "ReqMsgSnapShot"
	case ReqMsgElectAck:
		return "ReqMsgElectAck"
	case RespMsgAppend:
		return "RespMsgAppend"
	case RespMsgVote:
		return "RespMsgVote"
	case RespMsgHeartBeat:
		return "RespMsgHeartBeat"
	case RespMsgSnapShot:
		return "RespMsgSnapShot"
	case RespMsgElectAck:
		return "RespMsgElectAck"
	case LocalMsgHup:
		return "LocalMsgHup"
	case LocalMsgProp:
		return "LocalMsgProp"
	case LeaseMsgOffline:
		return "LeaseMsgOffline"
	case LeaseMsgTimeout:
		return "LeaseMsgTimeout"
	case ReqCheckQuorum:
		return "ReqCheckQuorum"
	case RespCheckQuorum:
		return "RespCheckQuorum"
	case ReqCompleteEntry:
		return "ReqCompleteEntry"
	case RespCompleteEntry:
		return "RespCompleteEntry"
	case ReqMsgGetApplyIndex:
		return "ReqMsgGetApplyIndex"
	case RespMsgGetApplyIndex:
		return "RespMsgGetApplyIndex"
	}
	return "unknown"
}

func (t EntryType) String() string {
	switch t {
	case EntryNormal:
		return "EntryNormal"
	case EntryConfChange:
		return "EntryConfChange"
	case EntryRollback:
		return "EntryRollback"
	}
	return "unknown"
}

func (t ConfChangeType) String() string {
	switch t {
	case ConfAddNode:
		return "ConfAddNode"
	case ConfRemoveNode:
		return "ConfRemoveNode"
	case ConfUpdateNode:
		return "ConfUpdateNode"
	case ConfAddLearner:
		return "ConfAddLearner"
	case ConfPromoteLearner:
		return "ConfPromoteLearner"
	case ConfAddRecorder:
		return "ConfAddRecorder"
	case ConfRemoveRecorder:
		return "ConfRemoveRecorder"
	}
	return "unknown"
}

func (t PeerType) String() string {
	switch t {
	case PeerNormal:
		return "PeerNormal"
	case PeerArbiter:
		return "PeerArbiter"
	case PeerRecorder:
		return "PeerRecorder"
	}
	return "unknown"
}

func (p Peer) String() string {
	return fmt.Sprintf(`"nodeID":"%v","peerID":"%v","priority":"%v","type":"%v"`,
		p.ID, p.PeerID, p.Priority, p.Type.String())
}

func (cc *ConfChange) String() string {
	return fmt.Sprintf(`{"type":"%v",%v}`, cc.Type, cc.Peer.String())
}

func (m *Message) IsResponseMsg() bool {
	return m.Type == RespMsgAppend || m.Type == RespMsgHeartBeat || m.Type == RespMsgVote ||
		m.Type == RespMsgElectAck || m.Type == RespMsgSnapShot || m.Type == RespCheckQuorum ||
		m.Type == RespCompleteEntry || m.Type == RespMsgGetApplyIndex
}

func (m *Message) IsElectionMsg() bool {
	return m.Type == ReqMsgHeartBeat || m.Type == RespMsgHeartBeat || m.Type == ReqMsgVote || m.Type == RespMsgVote ||
		m.Type == ReqMsgElectAck || m.Type == RespMsgElectAck || m.Type == LeaseMsgOffline || m.Type == LeaseMsgTimeout
}

func (m *Message) IsHeartbeatMsg() bool {
	return m.Type == ReqMsgHeartBeat || m.Type == RespMsgHeartBeat
}

func (m *Message) IsAppendMsg() bool {
	switch m.Type {
	case ReqMsgAppend, RespMsgAppend:
		return true
	default:
	}
	return false
}

func (s *HardState) IsEmpty() bool {
	return s.Term == 0 && s.Vote == 0 && s.Commit == 0
}
