package metamock

import (
	"context"
	cfsproto "github.com/cubefs/cubefs/proto"

	"github.com/cubefs/cubefs/raftstore"
	"github.com/tiglabs/raft"
	"github.com/tiglabs/raft/proto"
)

type ApplyFunc func(mp interface{}, command []byte, index uint64) (resp interface{}, err error)

type MockPartition struct {
	Id      uint64
	ApplyId uint64
	Buff    []byte
	Mp      []interface{}
	MemMp   interface{}
	RocksMp interface{}
	Apply   ApplyFunc
}

func NewMockPartition(id uint64) *MockPartition {
	mock := &MockPartition{Id: id}
	mock.Mp = make([]interface{}, 0)
	return mock
}

func (m *MockPartition) Submit(cmd []byte, ack proto.AckType) (resp interface{}, err error) {
	m.ApplyId++
	for i := 1; i < len(m.Mp); i++ {
		m.Apply(m.Mp[i], cmd, m.ApplyId)
	}

	//fmt.Printf("rocks mp:%v, mem mp:%v cmd:%v, apply id:%v\n", m.RocksMp, m.MemMp, cmd, m.ApplyId)
	//m.Apply(m.RocksMp, cmd, m.applyId)
	return m.Apply(m.Mp[0], cmd, m.ApplyId)
}

func (m *MockPartition) SubmitWithCtx(ctx context.Context, cmd []byte) (resp interface{}, err error) {
	m.ApplyId++
	for i := 1; i < len(m.Mp); i++ {
		m.Apply(m.Mp[i], cmd, m.ApplyId)
	}

	//fmt.Printf("rocks mp:%v, mem mp:%v cmd:%v, apply id:%v\n", m.RocksMp, m.MemMp, cmd, m.ApplyId)
	//m.Apply(m.RocksMp, cmd, m.applyId)
	return m.Apply(m.Mp[0], cmd, m.ApplyId)
}

func (m *MockPartition) ChangeMember(changeType proto.ConfChangeType, peer proto.Peer, context []byte) (resp interface{}, err error) {
	panic("implement me")
}

func (m *MockPartition) ResetMember(peers []proto.Peer, learners []proto.Learner, context []byte) (err error) {
	panic("implement me")
}

func (m *MockPartition) Stop() error {
	panic("implement me")
}

func (m *MockPartition) Delete() error {
	panic("implement me")
}

func (m *MockPartition) Expired() error {
	panic("implement me")
}

func (m *MockPartition) Status() (status *raftstore.PartitionStatus) {
	panic("implement me")
}

func (m *MockPartition) HardState() (hs proto.HardState, err error) {
	panic("implement me")
}

func (m *MockPartition) LeaderTerm() (leaderID, term uint64) {
	return m.Id, 0
}

func (m *MockPartition) IsRaftLeader() bool {
	return true
}

func (m *MockPartition) AppliedIndex() uint64 {
	return m.ApplyId
}

func (m *MockPartition) CommittedIndex() uint64 {
	panic("implement me")
}

func (m *MockPartition) Truncate(index uint64) {
	return
}

func (m *MockPartition) TryToLeader(nodeID uint64) error {
	panic("implement me")
}

func (m *MockPartition) IsOfflinePeer() bool {
	panic("implement me")
}

func (m *MockPartition) Start() error {
	panic("implement me")
}

func (m *MockPartition) FlushWAL(wait bool) error {
	panic("implement me")
}

func (m *MockPartition) RaftConfig() *raft.Config {
	return raft.DefaultConfig()
}

func (m *MockPartition) SetWALFileSize(filesize int) {
}

func (m *MockPartition) GetWALFileSize() int {
	return 0
}

func (m *MockPartition) SetWALFileCacheCapacity(capacity int) {
}

func (m *MockPartition) GetWALFileCacheCapacity() int {
	return 0
}

func (m *MockPartition) SetConsistencyMode(mode cfsproto.ConsistencyMode) {
	return
}

func (m *MockPartition) GetConsistencyMode() cfsproto.ConsistencyMode {
	return cfsproto.StandardMode
}

func (m *MockPartition) IsAllEmptyMsg(end uint64) (isAllEmptyMsg bool, err error) {
	return
}

func (m *MockPartition) GetLastIndex() (li uint64, err error) {
	return
}
