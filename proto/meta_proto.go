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
	"fmt"
	"time"
)

type PeerType uint8

const (
	PeerNormal		PeerType = iota
	PeerArbiter
	PeerRecorder
)

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

// CreateNameSpaceRequest defines the request to create a name space.
type CreateNameSpaceRequest struct {
	Name string
}

// CreateNameSpaceResponse defines the response to the request of creating a name space.
type CreateNameSpaceResponse struct {
	Status int
	Result string
}

// Peer defines the peer of the node id and address.
type Peer struct {
	ID   uint64 	`json:"id"`
	Addr string 	`json:"addr"`
	Type PeerType	`json:"type"`
}

func (p *Peer) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("ID(%v)Addr(%v)Type(%v)", p.ID, p.Addr, p.Type)
}

func (p *Peer) IsNormal() bool {
	return p.Type == PeerNormal
}

func (p *Peer) IsRecorder() bool {
	return p.Type == PeerRecorder
}

func (p *Peer) IsEqual(comparePeer Peer) bool {
	return p.ID == comparePeer.ID && p.Addr == comparePeer.Addr && p.Type == comparePeer.Type
}

// Learner defines the learner of the node id and address.
type Learner struct {
	ID       uint64         `json:"id"`
	Addr     string         `json:"addr"`
	PmConfig *PromoteConfig `json:"promote_config"`
}

type PromoteConfig struct {
	AutoProm      bool  `json:"auto_prom"`
	PromThreshold uint8 `json:"prom_threshold"`
}

// CreateMetaPartitionRequest defines the request to create a meta partition.
type CreateMetaPartitionRequest struct {
	MetaId       string
	VolName      string
	Start        uint64
	End          uint64
	PartitionID  uint64
	Members      []Peer
	Learners     []Learner
	Recorders	 []string
	StoreMode    StoreMode
	TrashDays    uint32
	CreationType int
}

// CreateMetaPartitionResponse defines the response to the request of creating a meta partition.
type CreateMetaPartitionResponse struct {
	VolName     string
	PartitionID uint64
	Status      uint8
	Result      string
}

type MNMetaPartitionInfo struct {
	LeaderAddr string    `json:"leaderAddr"`
	Peers      []Peer    `json:"peers"`
	Learners   []Learner `json:"learners"`
	NodeId     uint64    `json:"nodeId"`
	Cursor     uint64    `json:"cursor"`
	RaftStatus *Status	 `json:"raft_status"`
}

type MNMetaRecorderInfo struct {
	Peers      []Peer    `json:"peers"`
	Learners   []Learner `json:"learners"`
	NodeId     uint64    `json:"nodeId"`
	RaftStatus *Status	 `json:"raft_status"`
}

type MetaDataCRCSumInfo struct {
	PartitionID uint64   `json:"pid"`
	ApplyID     uint64   `json:"applyID"`
	CntSet      []uint64 `json:"cntSet"`
	CRCSumSet   []uint32 `json:"crcSumSet"`
}

type InodesCRCSumInfo struct {
	PartitionID     uint64   `json:"pid"`
	ApplyID         uint64   `json:"applyID"`
	AllInodesCRCSum uint32   `json:"allInodesCRCSum"`
	InodesID        []uint64 `json:"inodeIDSet"`
	CRCSumSet       []uint32 `json:"crcSumSet"`
}

type InodeInfoWithEK struct {
	Inode      uint64      `json:"ino"`
	Mode       uint32      `json:"mode"`
	Nlink      uint32      `json:"nlink"`
	Size       uint64      `json:"sz"`
	Uid        uint32      `json:"uid"`
	Gid        uint32      `json:"gid"`
	Generation uint64      `json:"gen"`
	ModifyTime time.Time   `json:"mt"`
	CreateTime time.Time   `json:"ct"`
	AccessTime time.Time   `json:"at"`
	Target     []byte      `json:"tgt"`
	Flag       int32       `json:"flag"`
	Extents    []ExtentKey `json:"eks"`
	Timestamp  int64       `json:"ts"`
	IsExpired  bool        `json:"isExpired"`
}

type CorrectMPInodesAndDelInodesTotalSizeReq struct {
	NeedCorrectHosts        []string `json:"needCorrectHosts"`
}