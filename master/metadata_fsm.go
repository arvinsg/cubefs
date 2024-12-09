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

package master

import (
	"encoding/json"
	"fmt"
	cProto "github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/util/log"
	"github.com/tiglabs/raft"
	"github.com/tiglabs/raft/proto"
	"io"
	"strconv"
)

const (
	applied = "applied"
)

type raftLeaderChangeHandler func(leader uint64)

type raftPeerChangeHandler func(confChange *proto.ConfChange) (err error)

type raftCmdApplyHandler func(cmdMap map[string]*RaftCmd) (err error)

type raftApplySnapshotHandler func()

type raftDeleteCmdApplyHandler func(cmd *RaftCmd) (err error)

// MetadataFsm represents the finite state machine of a metadata partition
type MetadataFsm struct {
	s                     *Server
	store                 *raftstore.RocksDBStore
	rs                    *raft.RaftServer
	applied               uint64
	retainLogs            uint64
	leaderChangeHandler   raftLeaderChangeHandler
	peerChangeHandler     raftPeerChangeHandler
	snapshotHandler       raftApplySnapshotHandler
	cmdApplyHandler       raftCmdApplyHandler
	deleteCmdApplyHandler raftDeleteCmdApplyHandler
}

func newMetadataFsm(s *Server, store *raftstore.RocksDBStore, retainsLog uint64, rs *raft.RaftServer) (fsm *MetadataFsm) {
	fsm = new(MetadataFsm)
	fsm.s = s
	fsm.store = store
	fsm.rs = rs
	fsm.retainLogs = retainsLog
	return
}

// Corresponding to the LeaderChange interface in Raft library.
func (mf *MetadataFsm) registerLeaderChangeHandler(handler raftLeaderChangeHandler) {
	mf.leaderChangeHandler = handler
}

// Corresponding to the PeerChange interface in Raft library.
func (mf *MetadataFsm) registerPeerChangeHandler(handler raftPeerChangeHandler) {
	mf.peerChangeHandler = handler
}

// Corresponding to the ApplySnapshot interface in Raft library.
func (mf *MetadataFsm) registerApplySnapshotHandler(handler raftApplySnapshotHandler) {
	mf.snapshotHandler = handler
}

func (mf *MetadataFsm) registerCreateMpHandler(handler raftCmdApplyHandler) {
	mf.cmdApplyHandler = handler
}

func (mf *MetadataFsm) registerDeleteCmdHandler(handler raftDeleteCmdApplyHandler) {
	mf.deleteCmdApplyHandler = handler
}

func (mf *MetadataFsm) restore() {
	mf.restoreApplied()
}

func (mf *MetadataFsm) restoreApplied() {
	defer func() {
		log.LogInfof("action[restoreApplied],applyID[%v]", mf.applied)
	}()
	value, err := mf.store.Get(applied)
	if err != nil {
		panic(fmt.Sprintf("Failed to restore applied err:%v", err.Error()))
	}
	byteValues := value.([]byte)
	if len(byteValues) == 0 {
		mf.applied = 0
		return
	}
	applied, err := strconv.ParseUint(string(byteValues), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("Failed to restore applied,err:%v ", err.Error()))
	}
	mf.applied = applied
}

// Apply implements the interface of raft.StateMachine
func (mf *MetadataFsm) Apply(command []byte, index uint64) (resp interface{}, err error) {
	cmd := new(RaftCmd)
	if err = cmd.Unmarshal(command); err != nil {
		log.LogErrorf("action[fsmApply],unmarshal data:%v, err:%v", command, err.Error())
		panic(err)
	}
	log.LogInfof("action[fsmApply],index[%v],cmd.op[%v],cmd.K[%v],cmd.V[%v]", index, cmd.Op, cmd.K, string(cmd.V))
	cmdMap := make(map[string][]byte)
	createMpCmd := make(map[string]*RaftCmd)
	if cmd.Op != opSyncBatchPut {
		cmdMap[cmd.K] = cmd.V
		cmdMap[applied] = []byte(strconv.FormatUint(uint64(index), 10))
	} else {
		nestedCmdMap := make(map[string]*RaftCmd)
		if err = json.Unmarshal(cmd.V, &nestedCmdMap); err != nil {
			log.LogErrorf("action[fsmApply],unmarshal nested cmd data:%v, err:%v", command, err.Error())
			panic(err)
		}
		for cmdK, nestCmd := range nestedCmdMap {
			if nestCmd.Op == opSyncAddMetaPartition {
				createMpCmd[nestCmd.K] = nestCmd
			}
			log.LogInfof("action[fsmApply],cmd.op[%v],cmd.K[%v],cmd.V[%v]", nestCmd.Op, nestCmd.K, string(nestCmd.V))
			cmdMap[cmdK] = nestCmd.V
		}
		cmdMap[applied] = []byte(strconv.FormatUint(uint64(index), 10))
	}
	if len(createMpCmd) != 0 {
		// if this meta partition has common precursor meta partition with other meta partition, don't persist this mp to rocksdb
		if err = mf.cmdApplyHandler(createMpCmd); err == cProto.ErrHasCommonPreMetaPartition {
			if err = mf.putIndex(applied, index); err != nil {
				panic(err)
			}
			return nil, nil
		}
	}
	switch cmd.Op {
	case opSyncDeleteDataNode, opSyncDeleteMetaNode, opSyncDeleteVol, opSyncDeleteDataPartition, opSyncDeleteMetaPartition,
		OpSyncDelToken, opSyncDeleteUserInfo, opSyncDeleteAKUser, opSyncDeleteVolUser, OpSyncDelRegion, OpSyncDelIDC, opSyncDeleteEcNode, opSyncDeleteCodecNode, opSyncDelEcPartition, opSyncDeleteMigrateTask,
		opSyncDeleteFlashNode, opSyncDeleteFlashGroup:
		if err = mf.deleteCmdApplyHandler(cmd); err != nil {
			panic(err)
		}
		if err = mf.delKeyAndPutIndex(cmd.K, cmdMap); err != nil {
			panic(err)
		}
	default:
		if err = mf.store.BatchPut(cmdMap, true); err != nil {
			panic(err)
		}
	}

	if err = mf.ApplyFollowerMqProducerStateToMemory(cmd); err != nil {
		return nil, err
	}

	log.LogInfof("action[fsmApply] finished,index[%v],cmd.op[%v],cmd.K[%v],cmd.V[%v]", index, cmd.Op, cmd.K, string(cmd.V))
	mf.applied = index
	if mf.applied > 0 && (mf.applied%mf.retainLogs) == 0 {
		log.LogWarnf("action[Apply],truncate raft log,retainLogs[%v],index[%v]", mf.retainLogs, mf.applied)
		mf.rs.Truncate(GroupID, mf.applied)
	}

	//only master leader send message
	if mf.s.cluster.cfg.MqProducerState {
		if mf.s.mqProducer == nil {
			warnKey := fmt.Sprintf("%v_%v_mqProducer", mf.s.cluster.Name, ModuleName)
			warnMsg := fmt.Sprintf("mq producer not initilized when sending master command, opCode(%v), cmdKey(%v), index(%v)", cmd.Op, cmd.K, index)
			WarnBySpecialKey(warnKey, warnMsg)
			return
		}
		mf.s.mqProducer.AddMasterNodeCommand(command, cmd.Op, cmd.K, index, mf.s.clusterName, mf.s.ip, mf.s.cluster.IsLeader())
	}
	return
}

// ApplyMemberChange implements the interface of raft.StateMachine
func (mf *MetadataFsm) ApplyMemberChange(confChange *proto.ConfChange, index uint64) (interface{}, error) {
	var err error
	if mf.peerChangeHandler != nil {
		err = mf.peerChangeHandler(confChange)
	}
	return nil, err
}

// Snapshot implements the interface of raft.StateMachine
func (mf *MetadataFsm) Snapshot(recoverNode uint64, isRecorder bool) (proto.Snapshot, error) {
	snapshot := mf.store.RocksDBSnapshot()
	iterator := mf.store.Iterator(snapshot)
	iterator.SeekToFirst()
	return &MetadataSnapshot{
		applied:  mf.applied,
		snapshot: snapshot,
		fsm:      mf,
		iterator: iterator,
	}, nil
}

// ApplySnapshot implements the interface of raft.StateMachine
func (mf *MetadataFsm) ApplySnapshot(peers []proto.Peer, iterator proto.SnapIterator, snapV uint32) (err error) {
	log.LogInfof(fmt.Sprintf("action[ApplySnapshot] begin,applied[%v]", mf.applied))
	var data []byte
	for err == nil {
		if data, err = iterator.Next(); err != nil {
			break
		}
		cmd := &RaftCmd{}
		if err = json.Unmarshal(data, cmd); err != nil {
			goto errHandler
		}
		if _, err = mf.store.Put(cmd.K, cmd.V, true); err != nil {
			goto errHandler
		}
	}
	if err != nil && err != io.EOF {
		goto errHandler
	}
	mf.snapshotHandler()
	log.LogInfof(fmt.Sprintf("action[ApplySnapshot] success,applied[%v]", mf.applied))
	return nil
errHandler:
	log.LogError(fmt.Sprintf("action[ApplySnapshot] failed,err:%v", err.Error()))
	return err
}

// HandleFatalEvent implements the interface of raft.StateMachine
func (mf *MetadataFsm) HandleFatalEvent(err *raft.FatalError) {
	panic(err.Err)
}

// HandleLeaderChange implements the interface of raft.StateMachine
func (mf *MetadataFsm) HandleLeaderChange(leader uint64) {
	if mf.leaderChangeHandler != nil {
		mf.leaderChangeHandler(leader)
	}
}

func (mf *MetadataFsm) delKeyAndPutIndex(key string, cmdMap map[string][]byte) (err error) {
	return mf.store.DeleteKeyAndPutIndex(key, cmdMap, true)
}

func (mf *MetadataFsm) putIndex(key string, index uint64) (err error) {
	cmdMap := make(map[string][]byte)
	cmdMap[key] = []byte(strconv.FormatUint(uint64(index), 10))
	return mf.store.BatchPut(cmdMap, true)
}

func (mf *MetadataFsm) AskRollback(_ []byte, _ uint64) (rollback []byte, err error) {
	// Do nothing.
	return nil, nil
}

// ApplyFollowerMqProducerStateToMemory
// If op is setting mqProducerState, persist it to RocksDB first and then modify it in memory,
// because followers also need to send messages.
func (mf *MetadataFsm) ApplyFollowerMqProducerStateToMemory(cmd *RaftCmd) (err error) {
	if mf.s.partition.IsRaftLeader() {
		return nil
	}
	if cmd.Op == opSyncSetMqProducerState {
		cv := &clusterValue{}
		if err = json.Unmarshal(cmd.V, cv); err != nil {
			log.LogErrorf("action[ApplyFollowerMqProducerStateToMemory], unmarshal err:%v", err.Error())
			return err
		}
		mf.s.cluster.cfg.MqProducerState = cv.MqProducerState
	}
	return
}
