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
	"net/http"
	"strconv"
	"strings"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	pb "github.com/gogo/protobuf/proto"
)

const (
	AuthFileName = ".clusterAuth_"
)

type NodeAPI struct {
	mc *MasterClient
}

type RegNodeInfoReq struct {
	Role     string
	ZoneName string
	Version  string
	ProfPort string
	SrvPort  string
}

func getReqPathByRole(role string) string {
	switch role {
	case proto.RoleData:
		return proto.AddDataNode
	case proto.RoleMeta:
		return proto.AddMetaNode
	case proto.RoleCodec:
		return proto.AddCodecNode
	case proto.RoleEc:
		return proto.AddEcNode
	case proto.RoleFlash:
		return proto.AddFlashNode
	default:
		return ""
	}
	return ""
}

func (api *NodeAPI) addRegParam(regInfo *RegNodeInfoReq, authKey, addr string, req *request) {
	req.addParam("module", regInfo.Role)
	req.addParam("addr", addr+":"+regInfo.SrvPort)
	if regInfo.ZoneName != "" {
		req.addParam("zoneName", regInfo.ZoneName)
	}

	if regInfo.Version != "" {
		req.addParam("version", regInfo.Version)
	}

	if regInfo.ProfPort != "" {
		req.addParam("httpPort", regInfo.ProfPort)
	}

	if authKey != "" {
		req.addParam("authenticate", authKey)
	}

}

func (api *NodeAPI) buildNewRegReq(regInfo *RegNodeInfoReq, authKey, addr string) (req *request, err error) {
	reqPath := getReqPathByRole(regInfo.Role)
	if reqPath == "" {
		err = fmt.Errorf("invalid para, role[%s] invalid", regInfo.Role)
		return
	}

	req = newAPIRequest(http.MethodGet, proto.RegNode)
	api.addRegParam(regInfo, authKey, addr, req)
	return
}

func (api *NodeAPI) buildOldRegReq(regInfo *RegNodeInfoReq, authKey, addr string) (req *request, err error) {
	reqPath := getReqPathByRole(regInfo.Role)
	if reqPath == "" {
		err = fmt.Errorf("invalid para, role[%s] invalid", regInfo.Role)
		return
	}

	req = newAPIRequest(http.MethodGet, reqPath)
	api.addRegParam(regInfo, authKey, addr, req)
	return
}

func (api *NodeAPI) AddDataNode(serverAddr, zoneName, version string) (id uint64, err error) {
	var request = newAPIRequest(http.MethodGet, proto.AddDataNode)
	request.addParam("addr", serverAddr)
	request.addParam("zoneName", zoneName)
	request.addParam("version", version)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	id, err = strconv.ParseUint(string(data), 10, 64)
	return
}

func (api *NodeAPI) AddMetaNode(serverAddr, zoneName, version string) (id uint64, err error) {
	var request = newAPIRequest(http.MethodGet, proto.AddMetaNode)
	request.addParam("addr", serverAddr)
	request.addParam("zoneName", zoneName)
	request.addParam("version", version)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	id, err = strconv.ParseUint(string(data), 10, 64)
	return
}

func (api *NodeAPI) AddCodecNode(serverAddr, version string) (id uint64, err error) {
	var request = newAPIRequest(http.MethodGet, proto.AddCodecNode)
	request.addParam("addr", serverAddr)
	request.addParam("version", version)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	id, err = strconv.ParseUint(string(data), 10, 64)
	return
}

func (api *NodeAPI) CodEcNodeDecommission(nodeAddr string) (err error) {
	var request = newAPIRequest(http.MethodGet, proto.DecommissionCodecNode)
	request.addParam("addr", nodeAddr)
	request.addHeader("isTimeOut", "false")
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) AddEcNode(serverAddr, httpPort, zoneName, version string) (id uint64, err error) {
	var request = newAPIRequest(http.MethodGet, proto.AddEcNode)
	request.addParam("addr", serverAddr)
	request.addParam("httpPort", httpPort)
	request.addParam("zoneName", zoneName)
	request.addParam("version", version)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	id, err = strconv.ParseUint(string(data), 10, 64)
	return
}

func (api *NodeAPI) GetEcScrubInfo() (scrubInfo proto.UpdateEcScrubInfoRequest, err error) {
	var respData []byte
	var respInfo proto.UpdateEcScrubInfoRequest
	var request = newAPIRequest(http.MethodGet, proto.AdminClusterGetScrub)
	request.addHeader("isTimeOut", "false")

	if respData, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	if err = json.Unmarshal(respData, &respInfo); err != nil {
		return
	}
	return respInfo, err
}

func (api *NodeAPI) EcNodeDecommission(nodeAddr string) (data []byte, err error) {
	var request = newAPIRequest(http.MethodGet, proto.DecommissionEcNode)
	request.addParam("addr", nodeAddr)
	request.addHeader("isTimeOut", "false")

	data, _, err = api.mc.serveRequest(request)
	return
}

func (api *NodeAPI) EcNodeDiskDecommission(nodeAddr, diskID string) (data []byte, err error) {
	var request = newAPIRequest(http.MethodGet, proto.DecommissionEcDisk)
	request.addParam("addr", nodeAddr)
	request.addParam("disk", diskID)
	request.addHeader("isTimeOut", "false")

	data, _, err = api.mc.serveRequest(request)
	return
}

func (api *NodeAPI) EcNodegetTaskStatus() (taskView []*proto.MigrateTaskView, err error) {
	var data []byte
	var request = newAPIRequest(http.MethodGet, proto.AdminGetAllTaskStatus)
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	taskView = make([]*proto.MigrateTaskView, 0)
	err = json.Unmarshal(data, &taskView)
	return
}

func (api *NodeAPI) GetDataNode(serverHost string) (node *proto.DataNodeInfo, err error) {
	var buf []byte
	var request = newAPIRequest(http.MethodGet, proto.GetDataNode)
	request.addParam("addr", serverHost)
	if buf, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	node = &proto.DataNodeInfo{}
	if err = json.Unmarshal(buf, &node); err != nil {
		return
	}
	return
}

func (api *NodeAPI) GetMetaNode(serverHost string) (node *proto.MetaNodeInfo, err error) {
	var buf []byte
	var request = newAPIRequest(http.MethodGet, proto.GetMetaNode)
	request.addParam("addr", serverHost)
	if buf, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	node = &proto.MetaNodeInfo{}
	if err = json.Unmarshal(buf, &node); err != nil {
		return
	}
	return
}

func (api *NodeAPI) ResponseMetaNodeTask(task *proto.AdminTask) (err error) {
	var encoded []byte
	if encoded, err = json.Marshal(task); err != nil {
		return
	}
	var request = newAPIRequest(http.MethodPost, proto.GetMetaNodeTaskResponse)
	request.addBody(encoded)
	if _, _, err = api.mc.serveRequest(request); err != nil {
		log.LogErrorf("serveRequest: %v", err.Error())
		return
	}
	return
}

func (api *NodeAPI) ResponseDataNodeTask(task *proto.AdminTask) (err error) {

	var encoded []byte
	if encoded, err = json.Marshal(task); err != nil {
		return
	}
	var request = newAPIRequest(http.MethodPost, proto.GetDataNodeTaskResponse)
	request.addBody(encoded)
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) ResponseCodecNodeTask(task *proto.AdminTask) (err error) {
	var encoded []byte
	if encoded, err = json.Marshal(task); err != nil {
		return
	}
	var request = newAPIRequest(http.MethodPost, proto.GetCodecNodeTaskResponse)
	request.addBody(encoded)
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) ResponseEcNodeTask(task *proto.AdminTask) (err error) {
	var encoded []byte
	if encoded, err = json.Marshal(task); err != nil {
		return
	}
	var request = newAPIRequest(http.MethodPost, proto.GetEcNodeTaskResponse)
	request.addBody(encoded)
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) DataNodeDecommission(nodeAddr string) (err error) {
	var request = newAPIRequest(http.MethodGet, proto.DecommissionDataNode)
	request.addParam("addr", nodeAddr)
	request.addHeader("isTimeOut", "false")
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) DataNodeDiskDecommission(nodeAddr, diskID string, force bool) (err error) {
	var request = newAPIRequest(http.MethodGet, proto.DecommissionDisk)
	request.addParam("addr", nodeAddr)
	request.addParam("disk", diskID)
	request.addParam("force", strconv.FormatBool(force))
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) MetaNodeDecommission(nodeAddr string) (err error) {
	var request = newAPIRequest(http.MethodGet, proto.DecommissionMetaNode)
	request.addParam("addr", nodeAddr)
	request.addHeader("isTimeOut", "false")
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) DataNodeGetPartition(addr string, id uint64) (node *proto.DNDataPartitionInfo, err error) {
	var request = newAPIRequest(http.MethodGet, "/partition")
	var buf []byte
	nodeClient := NewNodeClient(fmt.Sprintf("%v:%v", addr, api.mc.DataNodeProfPort), false, DATANODE)
	nodeClient.DataNodeProfPort = api.mc.DataNodeProfPort
	request.addParam("id", strconv.FormatUint(id, 10))
	request.addHeader("isTimeOut", "false")
	if buf, _, err = nodeClient.serveRequest(request); err != nil {
		return
	}
	node = new(proto.DNDataPartitionInfo)
	pInfoOld := new(proto.DNDataPartitionInfoOldVersion)
	if err = json.Unmarshal(buf, pInfoOld); err != nil {
		err = json.Unmarshal(buf, node)
		return
	}
	for _, ext := range pInfoOld.Files {
		extent := proto.ExtentInfoBlock{
			ext.FileID,
			ext.Size,
			uint64(ext.Crc),
			uint64(ext.ModifyTime),
		}
		node.Files = append(node.Files, extent)
	}
	node.RaftStatus = pInfoOld.RaftStatus
	node.Path = pInfoOld.Path
	node.VolName = pInfoOld.VolName
	node.Replicas = pInfoOld.Replicas
	node.Size = pInfoOld.Size
	node.ID = pInfoOld.ID
	node.Status = pInfoOld.Status
	node.FileCount = pInfoOld.FileCount
	node.Peers = pInfoOld.Peers
	node.Learners = pInfoOld.Learners
	node.Used = pInfoOld.Used
	return
}

func (api *NodeAPI) MetaNodeGetPartition(addr string, id uint64) (node *proto.MNMetaPartitionInfo, err error) {
	var request = newAPIRequest(http.MethodGet, "/getPartitionById")
	var buf []byte
	nodeClient := NewNodeClient(fmt.Sprintf("%v:%v", addr, api.mc.MetaNodeProfPort), false, METANODE)
	nodeClient.MetaNodeProfPort = api.mc.MetaNodeProfPort
	request.addParam("pid", strconv.FormatUint(id, 10))
	request.addHeader("isTimeOut", "false")
	if buf, _, err = nodeClient.serveRequest(request); err != nil {
		return
	}
	node = &proto.MNMetaPartitionInfo{}
	if err = json.Unmarshal(buf, &node); err != nil {
		return
	}
	return
}

func (api *NodeAPI) DataNodeValidateCRCReport(dpCrcInfo *proto.DataPartitionExtentCrcInfo) (err error) {
	var encoded []byte
	if encoded, err = json.Marshal(dpCrcInfo); err != nil {
		return
	}
	var request = newAPIRequest(http.MethodPost, proto.DataNodeValidateCRCReport)
	request.addBody(encoded)
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *NodeAPI) DataNodeGetTinyExtentHolesAndAvali(addr string, partitionID, extentID uint64) (info *proto.DNTinyExtentInfo, err error) {
	var request = newAPIRequest(http.MethodGet, "/tinyExtentHoleInfo")
	var buf []byte
	nodeClient := NewNodeClient(fmt.Sprintf("%v:%v", addr, api.mc.DataNodeProfPort), false, DATANODE)
	nodeClient.DataNodeProfPort = api.mc.DataNodeProfPort
	request.addParam("partitionID", strconv.FormatUint(partitionID, 10))
	request.addParam("extentID", strconv.FormatUint(extentID, 10))
	request.addHeader("isTimeOut", "false")
	if buf, _, err = nodeClient.serveRequest(request); err != nil {
		return
	}
	info = &proto.DNTinyExtentInfo{}
	if err = json.Unmarshal(buf, &info); err != nil {
		return
	}
	return
}

func (api *NodeAPI) GetCodecNode(serverHost string) (node *proto.CodecNodeInfo, err error) {
	var buf []byte
	var request = newAPIRequest(http.MethodGet, proto.GetCodecNode)
	request.addParam("addr", serverHost)
	if buf, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	node = &proto.CodecNodeInfo{}
	if err = json.Unmarshal(buf, &node); err != nil {
		return
	}
	return
}

func (api *NodeAPI) GetEcNode(serverHost string) (node *proto.EcNodeInfo, err error) {
	var buf []byte
	var request = newAPIRequest(http.MethodGet, proto.GetEcNode)
	request.addParam("addr", serverHost)
	if buf, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	node = &proto.EcNodeInfo{}
	if err = json.Unmarshal(buf, &node); err != nil {
		return
	}
	return
}

func (api *NodeAPI) EcNodeGetTaskStatus() (taskView []*proto.MigrateTaskView, err error) {
	var data []byte
	var request = newAPIRequest(http.MethodGet, proto.AdminGetAllTaskStatus)
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	taskView = make([]*proto.MigrateTaskView, 0)
	err = json.Unmarshal(data, &taskView)
	return
}

func (api *NodeAPI) DataNodeGetExtentCrc(addr string, partitionId, extentId uint64) (crc uint32, err error) {
	var request = newAPIRequest(http.MethodGet, "/extentCrc")
	var buf []byte
	nodeClient := NewNodeClient(fmt.Sprintf("%v:%v", addr, api.mc.DataNodeProfPort), false, DATANODE)
	nodeClient.DataNodeProfPort = api.mc.DataNodeProfPort
	request.addParam("partitionId", strconv.FormatUint(partitionId, 10))
	request.addParam("extentId", strconv.FormatUint(extentId, 10))
	request.addHeader("isTimeOut", "false")
	if buf, _, err = nodeClient.serveRequest(request); err != nil {
		return
	}
	resp := &struct {
		CRC uint32
	}{}
	if err = json.Unmarshal(buf, resp); err != nil {
		return
	}
	crc = resp.CRC
	return
}

func (api *NodeAPI) EcNodeGetExtentCrc(addr string, partitionId, extentId, stripeCount uint64, crc uint32) (resp *proto.ExtentCrcResponse, err error) {
	var request = newAPIRequest(http.MethodGet, "/extentCrc")
	var buf []byte
	nodeClient := NewNodeClient(fmt.Sprintf("%v:%v", addr, api.mc.EcNodeProfPort), false, ECNODE)
	nodeClient.EcNodeProfPort = api.mc.EcNodeProfPort
	request.addParam("partitionId", strconv.FormatUint(partitionId, 10))
	request.addParam("extentId", strconv.FormatUint(extentId, 10))
	request.addParam("stripeCount", strconv.FormatUint(stripeCount, 10))
	request.addParam("crc", strconv.FormatUint(uint64(crc), 10))
	request.addHeader("isTimeOut", "false")
	if buf, _, err = nodeClient.serveRequest(request); err != nil {
		return
	}
	resp = &proto.ExtentCrcResponse{}
	if err = json.Unmarshal(buf, resp); err != nil {
		return
	}

	return
}

func (api *NodeAPI) StopMigratingByDataNode(datanode string) string {
	var request = newAPIRequest(http.MethodGet, proto.AdminDNStopMigrating)
	request.addParam("addr", datanode)
	data, _, err := api.mc.serveRequest(request)
	if err != nil {
		return fmt.Sprintf("StopMigratingByDataNode fail:%v\n", err)
	}
	return string(data)
}

func (api *NodeAPI) AddFlashNode(serverAddr, zoneName, version string) (id uint64, err error) {
	var request = newAPIRequest(http.MethodGet, proto.AddFlashNode)
	request.addParam("addr", serverAddr)
	request.addParam("zoneName", zoneName)
	request.addParam("version", version)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	id, err = strconv.ParseUint(string(data), 10, 64)
	return
}

func (api *NodeAPI) FlashNodeDecommission(nodeAddr string) (result string, err error) {
	var data []byte
	var request = newAPIRequest(http.MethodGet, proto.DecommissionFlashNode)
	request.addParam("addr", nodeAddr)
	request.addHeader("isTimeOut", "false")
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return string(data), nil
}

func (api *NodeAPI) GetFlashNode(serverHost string) (node *proto.FlashNodeViewInfo, err error) {
	var buf []byte
	var request = newAPIRequest(http.MethodGet, proto.GetFlashNode)
	request.addParam("addr", serverHost)
	if buf, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	node = &proto.FlashNodeViewInfo{}
	if err = json.Unmarshal(buf, &node); err != nil {
		return
	}
	return
}

func (api *NodeAPI) SetFlashNodeState(addr, state string) (err error) {
	var buf []byte
	var request = newAPIRequest(http.MethodGet, proto.AdminSetFlashNode)
	request.addParam("addr", addr)
	request.addParam("state", state)
	if buf, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	var msg string
	if err = json.Unmarshal(buf, &msg); err != nil {
		return
	}

	if !strings.Contains(msg, "success") {
		err = fmt.Errorf("set flashNodeState failed: %v", msg)
	}
	return
}

func (api *NodeAPI) ResponseHeartBeatTaskPb(taskPb *proto.HeartbeatAdminTaskPb) (err error) {
	var encoded []byte
	if encoded, err = pb.Marshal(taskPb); err != nil {
		return
	}
	var request = newAPIRequest(http.MethodPost, proto.GetHeartbeatPbResponse)
	request.addBody(encoded)
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}
