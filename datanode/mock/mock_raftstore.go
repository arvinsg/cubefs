package mock

import (
	cfsproto "github.com/cubefs/cubefs/proto"

	"github.com/cubefs/cubefs/raftstore"
	"github.com/tiglabs/raft"
	"github.com/tiglabs/raft/proto"
)

type mockRaftPartition struct {
	cfg     *raftstore.PartitionConfig
	raftCfg *raft.Config
}

func (m mockRaftPartition) Start() error {
	return nil
}

func (m mockRaftPartition) Submit(cmd []byte, ack proto.AckType) (resp interface{}, err error) {
	return nil, err
}

func (m mockRaftPartition) ChangeMember(changeType proto.ConfChangeType, peer proto.Peer, context []byte) (resp interface{}, err error) {
	return nil, err
}

func (m mockRaftPartition) ResetMember(peers []proto.Peer, learners []proto.Learner, context []byte) (err error) {
	return nil
}

func (m mockRaftPartition) Stop() error {
	return nil
}

func (m mockRaftPartition) Delete() error {
	return nil
}

func (m mockRaftPartition) Expired() error {
	return nil
}

func (m mockRaftPartition) Status() (status *raftstore.PartitionStatus) {
	return nil
}

func (m mockRaftPartition) HardState() (hs proto.HardState, err error) {
	return proto.HardState{}, nil
}

func (m mockRaftPartition) LeaderTerm() (leaderID, term uint64) {
	return 0, 0
}

func (m mockRaftPartition) IsRaftLeader() bool {
	return true
}

func (m mockRaftPartition) AppliedIndex() uint64 {
	return 0
}

func (m mockRaftPartition) CommittedIndex() uint64 {
	return 0
}

func (m mockRaftPartition) Truncate(index uint64) {
}

func (m mockRaftPartition) TryToLeader(nodeID uint64) error {
	return nil
}

func (m mockRaftPartition) IsOfflinePeer() bool {
	return false
}

func (m mockRaftPartition) RaftConfig() *raft.Config {
	return m.raftCfg
}

func (m mockRaftPartition) FlushWAL(wait bool) error {
	return nil
}

func (m mockRaftPartition) SetWALFileSize(filesize int) {
	m.cfg.WALFileSize = filesize
}

func (m mockRaftPartition) GetWALFileSize() int {
	return m.cfg.WALFileSize
}

func (m mockRaftPartition) SetWALFileCacheCapacity(capacity int) {
	m.cfg.WALFileCacheCapacity = capacity
}

func (m mockRaftPartition) GetWALFileCacheCapacity() int {
	return m.cfg.WALFileCacheCapacity
}

func (m mockRaftPartition) SetConsistencyMode(mode cfsproto.ConsistencyMode) {
	return
}

func (m mockRaftPartition) GetConsistencyMode() cfsproto.ConsistencyMode {
	return cfsproto.StandardMode
}

func (m mockRaftPartition) IsAllEmptyMsg(end uint64) (isAllEmptyMsg bool, err error) {
	return
}

func (m mockRaftPartition) GetLastIndex() (li uint64, err error) {
	return
}

type mockRaftStore struct {
	cfg *raft.Config
}

func (m mockRaftStore) CreatePartition(cfg *raftstore.PartitionConfig) raftstore.Partition {
	return &mockRaftPartition{
		cfg:     cfg,
		raftCfg: m.cfg,
	}
}

func (m mockRaftStore) Stop() {
}

func (m mockRaftStore) RaftConfig() *raft.Config {
	return m.cfg
}

func (m mockRaftStore) RaftStatus(raftID uint64) (raftStatus *raft.Status) {
	return nil
}

func (m mockRaftStore) AddNodeWithPort(nodeID uint64, addr string, heartbeat int, replicate int) {
}

func (m mockRaftStore) DeleteNode(nodeID uint64) {
}

func (m mockRaftStore) RaftServer() *raft.RaftServer {
	return nil
}

func (m mockRaftStore) SetSyncWALOnUnstable(enable bool) {
	m.cfg.SyncWALOnUnstable = enable
}

func (m mockRaftStore) IsSyncWALOnUnstable() (enabled bool) {
	return m.cfg.SyncWALOnUnstable
}

func (m mockRaftStore) RaftPath() string {
	return ""
}

func NewMockRaftStore() raftstore.RaftStore {
	return &mockRaftStore{
		cfg: raft.DefaultConfig(),
	}
}
