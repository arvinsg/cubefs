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

package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/tiglabs/raft"

	"github.com/tiglabs/raft/proto"
)

func TestMember(t *testing.T) {
	tests := []RaftTestConfig{
		{
			name:    	"memberWithNoLease_default",
			isLease: 	false,
			mode:    	StandardMode,
			peers:		peers,
		},
		{
			name:    	"memberWithLease_default",
			isLease: 	true,
			mode:    	StandardMode,
			peers:		peers,
		},
		{
			name:    	"memberWithNoLease_strict",
			isLease: 	false,
			mode:    	StrictMode,
			peers:		peers,
		},
		{
			name:    	"memberWithLease_strict",
			isLease: 	true,
			mode:    	StrictMode,
			peers:		peers,
		},
		{
			name:    	"memberWithNoLease_mix",
			isLease: 	false,
			mode:    	MixMode,
			peers:		peers,
		},
		{
			name:    	"memberWithLease_mix",
			isLease: 	true,
			mode:    	MixMode,
			peers:		peers,
		},
		{
			name:    	"memberWithNoLease_recorder",
			isLease: 	false,
			mode:    	StandardMode,
			peers:		recorderPeers,
		},
		{
			name:    	"memberWithLease_recorder",
			isLease: 	true,
			mode:    	StandardMode,
			peers:		recorderPeers,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			member(t, tt.name, tt.isLease, tt.mode, tt.peers)
		})
	}
}

func member(t *testing.T, testName string, isLease bool, mode RaftMode, peers []proto.Peer) {
	servers := initTestServer(peers, isLease, true, 1, mode)
	f, w := getLogFile("", testName+".log")
	defer func() {
		w.Flush()
		f.Close()
		for _, s := range servers {
			s.raft.Stop()
		}
		time.Sleep(100 * time.Millisecond)
	}()

	output("waiting electing leader....")
	leadServer := waitElect(servers, 1, w)
	printStatus(servers, w)

	// test add node
	addNodeID := uint64(6)
	w.WriteString(fmt.Sprintf("[%s] Add new node [%v]\r\n", time.Now().Format(format_time), addNodeID))
	leader, term := leadServer.raft.LeaderTerm(1)
	raftConfig := &raft.RaftConfig{Peers: peers, Leader: leader, Term: term, Mode: getConsistencyMode(mode, addNodeID)}
	newServer := createRaftServer(addNodeID, isLease, true, 1, raftConfig)
	// add node
	resolver.addNode(addNodeID, 0)
	output("starting add node")
	leadServer.sm[1].AddNode(proto.Peer{ID: addNodeID})
	output("added node")
	time.Sleep(time.Second)
	servers = append(servers, newServer)
	printStatus(servers, w)

	output("starting put data")
	if err := leadServer.sm[1].Put("test2", "test2_val", NoCheckLinear, NoCheckLinear); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if vget, err := newServer.sm[1].Get("test2"); err != nil || vget != "test2_val" {
		t.Fatal("new add node not get the data")
	}
	output("success put data")

	// test remove node
	w.WriteString(fmt.Sprintf("[%s] Remove node \r\n", time.Now().Format(format_time)))
	output("starting remove node")
	leadServer.sm[1].RemoveNode(proto.Peer{ID: addNodeID})
	output("removed node")
	output("starting put data")
	if err := leadServer.sm[1].Put("test3", "test3_val", NoCheckLinear, NoCheckLinear); err != nil {
		t.Fatal(err)
	}
	output("success put data")
	newServers := make([]*testServer, 0)
	for _, s := range servers {
		if s.nodeID == newServer.nodeID {
			s.raft.Stop()
		} else {
			newServers = append(newServers, s)
		}
	}
	servers = newServers
	time.Sleep(100 * time.Millisecond)
	waitElect(servers, 1, w)
	raftConfig = &raft.RaftConfig{Peers: append(peers, proto.Peer{ID: addNodeID}), Leader: 0, Term: 10, Mode: getConsistencyMode(mode, addNodeID)}
	newServer = createRaftServer(addNodeID, isLease, false, 1, raftConfig)
	servers = append(servers, newServer)
	printStatus(servers, w)
	resolver.delNode(addNodeID)

}
