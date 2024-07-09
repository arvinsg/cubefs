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
	"net/url"
	"strconv"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	pb "github.com/gogo/protobuf/proto"
)

type Decoder func([]byte) ([]byte, error)

func (d Decoder) Decode(raw []byte) ([]byte, error) {
	return d(raw)
}

type ClientAPI struct {
	mc *MasterClient
}

func unmarshalVolView(data []byte, contentType string) (vv *proto.VolView, err error) {
	if contentType == proto.ProtobufType {
		vvPb := &proto.VolViewPb{}
		if err = pb.Unmarshal(data, vvPb); err != nil {
			return
		}
		vv = proto.ConvertVolViewPb(vvPb)
	} else {
		vv = &proto.VolView{}
		if err = json.Unmarshal(data, vv); err != nil {
			return
		}
	}
	return
}

func (api *ClientAPI) GetVolume(volName string, authKey string) (vv *proto.VolView, err error) {
	var request = newAPIRequest(http.MethodPost, proto.ClientVol)
	request.addParam("name", volName)
	request.addParam("authKey", authKey)
	request.addParam("baseVersion", proto.BaseVersion)
	request.addHeader(proto.AcceptFormat, proto.ProtobufType)
	var (
		data        []byte
		contentType string
	)
	if data, contentType, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return unmarshalVolView(data, contentType)
}

func (api *ClientAPI) GetVolumeWithoutAuthKey(volName string) (vv *proto.VolView, err error) {
	var request = newAPIRequest(http.MethodPost, proto.ClientVol)
	request.addParam("name", volName)
	request.addHeader(proto.SkipOwnerValidation, strconv.FormatBool(true))
	request.addHeader(proto.AcceptFormat, proto.ProtobufType)
	var (
		data        []byte
		contentType string
	)
	if data, contentType, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return unmarshalVolView(data, contentType)
}

func (api *ClientAPI) GetVolumeWithAuthnode(volName string, authKey string, token string, decoder Decoder) (vv *proto.VolView, err error) {
	var (
		body        []byte
		contentType string
	)
	var request = newAPIRequest(http.MethodPost, proto.ClientVol)
	request.addParam("name", volName)
	request.addParam("authKey", authKey)
	request.addParam(proto.ClientMessage, token)
	if body, contentType, err = api.mc.serveRequest(request); err != nil {
		return
	}
	if decoder != nil {
		if body, err = decoder.Decode(body); err != nil {
			return
		}
	}
	return unmarshalVolView(body, contentType)
}

func (api *ClientAPI) GetVolumeStat(volName string) (info *proto.VolStatInfo, err error) {
	var request = newAPIRequest(http.MethodGet, proto.ClientVolStat)
	request.addParam("name", volName)
	request.addParam("baseVersion", proto.BaseVersion)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	info = &proto.VolStatInfo{}
	if err = json.Unmarshal(data, info); err != nil {
		return
	}
	return
}

func (api *ClientAPI) GetToken(volName, tokenKey string) (token *proto.Token, err error) {
	var request = newAPIRequest(http.MethodGet, proto.TokenGetURI)
	request.addParam("name", volName)
	request.addParam("token", url.QueryEscape(tokenKey))
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	token = &proto.Token{}
	if err = json.Unmarshal(data, token); err != nil {
		return
	}
	return
}

func (api *ClientAPI) GetMetaPartition(partitionID uint64, volName string) (partition *proto.MetaPartitionInfo, err error) {
	var request = newAPIRequest(http.MethodGet, proto.ClientMetaPartition)
	request.addParam("id", strconv.FormatUint(partitionID, 10))
	request.addParam("name", volName)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	partition = &proto.MetaPartitionInfo{}
	if err = json.Unmarshal(data, partition); err != nil {
		return
	}
	if proto.IsDbBack {
		partition.Hosts = partition.PersistenceHosts
	}
	return
}

func (api *ClientAPI) GetMetaPartitions(volName string) ([]*proto.MetaPartitionView, error) {
	if proto.IsDbBack {
		if vv, err := api.GetVolumeWithoutAuthKey(volName); err != nil {
			return nil, err
		} else {
			return vv.MetaPartitions, nil
		}
	} else {
		return api.getMetaPartitions(volName)
	}
}

func (api *ClientAPI) getMetaPartitions(volName string) (views []*proto.MetaPartitionView, err error) {
	var request = newAPIRequest(http.MethodGet, proto.ClientMetaPartitions)
	request.addParam("name", volName)
	request.addHeader(proto.AcceptFormat, proto.ProtobufType)
	var (
		data        []byte
		contentType string
	)
	if data, contentType, err = api.mc.serveRequest(request); err != nil {
		return
	}
	if contentType == proto.ProtobufType {
		viewsPb := &proto.MetaPartitionViewsPb{}
		if err = pb.Unmarshal(data, viewsPb); err != nil {
			return
		}
		views = proto.ConvertMetaPartitionViewsPb(viewsPb)
	} else {
		err = json.Unmarshal(data, &views)
	}
	return
}

// dpIDs为空时获取vol全量的dp信息
func (api *ClientAPI) GetDataPartitions(volName string, dpIDs []uint64) (view *proto.DataPartitionsView, err error) {
	path := proto.ClientDataPartitions
	if proto.IsDbBack || api.mc.IsDbBack {
		path = proto.ClientDataPartitionsDbBack
	}
	var request = newAPIRequest(http.MethodGet, path)
	request.addParam("name", volName)
	if len(dpIDs) != 0 {
		var dpIDsStr string
		for index, id := range dpIDs {
			dpIDsStr += fmt.Sprintf("%v", id)
			if index != len(dpIDs)-1 {
				dpIDsStr += ","
			}
		}
		request.addParam(proto.IDsKey, dpIDsStr)
	}
	request.addHeader(proto.AcceptFormat, proto.ProtobufType)
	var (
		data        []byte
		contentType string
	)
	if data, contentType, err = api.mc.serveRequest(request); err != nil {
		return
	}
	if contentType == proto.ProtobufType {
		viewPb := &proto.DataPartitionsViewPb{}
		if err = pb.Unmarshal(data, viewPb); err != nil {
			log.LogErrorf("unmarshal data partitions view pb data(%v) failed: %v", data, err)
			return
		}
		view = proto.ConvertDataPartitionsViewPb(viewPb)

	} else {
		view = &proto.DataPartitionsView{}
		if err = json.Unmarshal(data, view); err != nil {
			log.LogErrorf("unmarshal data partitions view json data(%s) failed: %v", string(data), err)
		}
	}

	return
}

func (api *ClientAPI) GetEcPartitions(volName string) (view *proto.EcPartitionsView, err error) {
	path := proto.ClientEcPartitions
	var request = newAPIRequest(http.MethodGet, path)
	request.addParam("name", volName)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	view = &proto.EcPartitionsView{}
	if err = json.Unmarshal(data, view); err != nil {
		return
	}
	return
}

func (api *ClientAPI) ApplyVolMutex(app, volName, addr string) (err error) {
	var request = newAPIRequest(http.MethodGet, proto.AdminApplyVolMutex)
	request.addParam("app", app)
	request.addParam("name", volName)
	request.addParam("addr", addr)
	_, _, err = api.mc.serveRequest(request)
	return
}

func (api *ClientAPI) ReleaseVolMutex(app, volName, addr string) (err error) {
	var request = newAPIRequest(http.MethodGet, proto.AdminReleaseVolMutex)
	request.addParam("app", app)
	request.addParam("name", volName)
	request.addParam("addr", addr)
	_, _, err = api.mc.serveRequest(request)
	return
}

func (api *ClientAPI) GetVolMutex(app, volName string) (*proto.VolWriteMutexInfo, error) {
	var request = newAPIRequest(http.MethodGet, proto.AdminGetVolMutex)
	request.addParam("app", app)
	request.addParam("name", volName)
	data, _, err := api.mc.serveRequest(request)
	if err != nil {
		return nil, err
	}
	volWriteMutexInfo := &proto.VolWriteMutexInfo{}
	if err = json.Unmarshal(data, volWriteMutexInfo); err != nil {
		return nil, err
	}
	return volWriteMutexInfo, nil
}

func (api *ClientAPI) SetClientPkgAddr(addr string) (err error) {
	var request = newAPIRequest(http.MethodGet, proto.AdminSetClientPkgAddr)
	request.addParam("addr", addr)
	if _, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	return
}

func (api *ClientAPI) GetClientPkgAddr() (addr string, err error) {
	var request = newAPIRequest(http.MethodGet, proto.AdminGetClientPkgAddr)
	var data []byte
	if data, _, err = api.mc.serveRequest(request); err != nil {
		return
	}
	if err = json.Unmarshal(data, &addr); err != nil {
		return
	}
	return
}
