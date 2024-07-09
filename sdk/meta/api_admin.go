package meta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

const (
	requestTimeout = 30 * time.Second
)

type request struct {
	method string
	path   string
	params map[string]string
	header map[string]string
	body   []byte
}

func newAPIRequest(method string, path string) *request {
	return &request{
		method: method,
		path:   path,
		params: make(map[string]string),
		header: make(map[string]string),
	}
}

func (r *request) addParam(key, value string) {
	r.params[key] = value
}

func (r *request) addHeader(key, value string) {
	r.header[key] = value
}

func (r *request) addBody(body []byte) {
	r.body = body
}

type MetaHttpClient struct {
	sync.RWMutex
	useSSL   bool
	host     string
	isDBBack bool
}

// NewMasterHelper returns a new MasterClient instance.
func NewMetaHttpClient(host string, useSSL bool) *MetaHttpClient {
	return &MetaHttpClient{host: host, useSSL: useSSL, isDBBack: false}
}

func NewDBBackMetaHttpClient(host string, useSSL bool) *MetaHttpClient {
	return &MetaHttpClient{host: host, useSSL: useSSL, isDBBack: true}
}

func isDbBackOldApi(path string) bool {
	if strings.Contains(path, "getExtents") || strings.Contains(path, "getAllPartitions") ||
		strings.Contains(path, "getPartitionById") || strings.Contains(path, "getInodeRange") ||
		strings.Contains(path, "getDentry") || strings.Contains(path, "getAllDentriesByParentIno") ||
		strings.Contains(path, "getStartFailedPartitions") {
		return true
	}
	return false
}

func (c *MetaHttpClient) serveRequest(r *request) (respData []byte, err error) {
	var resp *http.Response
	var schema string
	if c.useSSL {
		schema = "https"
	} else {
		schema = "http"
	}
	var url = fmt.Sprintf("%s://%s%s", schema, c.host, r.path)
	resp, err = c.httpRequest(r.method, url, r.params, r.header, r.body)

	if err != nil {
		log.LogErrorf("serveRequest: send http request fail: method(%v) url(%v) err(%v)", r.method, url, err)
		return
	}
	stateCode := resp.StatusCode
	respData, err = ioutil.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		log.LogErrorf("serveRequest: read http response body fail: err(%v)", err)
		return
	}
	switch stateCode {
	case http.StatusOK:
		if c.isDBBack && isDbBackOldApi(r.path) {
			return respData, nil
		}
		var body = &struct {
			Code int32           `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}{}

		if err := json.Unmarshal(respData, body); err != nil {
			return nil, fmt.Errorf("unmarshal response body err:%v", err)

		}
		// o represent proto.ErrCodeSuccess
		if body.Code != http.StatusOK && body.Code != http.StatusSeeOther {
			return nil, fmt.Errorf("%v:%s", proto.ParseErrorCode(body.Code), body.Msg)
		}
		return []byte(body.Data), nil
	default:
		if c.isDBBack && strings.Contains(r.path, "getExtents") && stateCode == http.StatusNotFound {
			return nil, nil
		}
		log.LogErrorf("serveRequest: unknown status: host(%v) uri(%v) status(%v) body(%s).",
			resp.Request.URL.String(), c.host, stateCode, strings.Replace(string(respData), "\n", "", -1))
		err = fmt.Errorf("unknown status: host(%v) url(%v) status(%v) body(%s)",
			resp.Request.URL.String(), c.host, stateCode, strings.Replace(string(respData), "\n", "", -1))
	}
	return
}

func (c *MetaHttpClient) httpRequest(method, url string, param, header map[string]string, reqData []byte) (resp *http.Response, err error) {
	client := http.DefaultClient
	reader := bytes.NewReader(reqData)
	client.Timeout = requestTimeout
	var req *http.Request
	fullUrl := c.mergeRequestUrl(url, param)
	log.LogDebugf("httpRequest: merge request url: method(%v) url(%v) bodyLength[%v].", method, fullUrl, len(reqData))
	if req, err = http.NewRequest(method, fullUrl, reader); err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "close")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err = client.Do(req)
	return
}

func (c *MetaHttpClient) mergeRequestUrl(url string, params map[string]string) string {
	if params != nil && len(params) > 0 {
		buff := bytes.NewBuffer([]byte(url))
		isFirstParam := true
		for k, v := range params {
			if isFirstParam {
				buff.WriteString("?")
				isFirstParam = false
			} else {
				buff.WriteString("&")
			}
			buff.WriteString(k)
			buff.WriteString("=")
			buff.WriteString(v)
		}
		return buff.String()
	}
	return url
}

type GetMPInfoResp struct {
	LeaderAddr         string          `json:"leaderAddr"`
	Peers              []proto.Peer    `json:"peers"`
	Learners           []proto.Learner `json:"learners"`
	NodeId             uint64          `json:"nodeId"`
	Cursor             uint64          `json:"cursor"`
	ApplyId            uint64          `json:"apply_id"`
	InodeCount         uint64          `json:"inode_count"`
	DentryCount        uint64          `json:"dentry_count"`
	MultipartCount     uint64          `json:"multipart_count"`
	ExtentCount        uint64          `json:"extent_count"`
	FreeListCount      int             `json:"free_list_count"`
	Leader             bool            `json:"leader"`
	DelInodeCount      uint64          `json:"deleted_inode_count"`
	DelDentryCount     uint64          `json:"deleted_dentry_count"`
	DelInodesTotalSize int64           `json:"del_inodes_total_size"`
	InodesTotalSize    int64           `json:"inodes_total_size"`
}

func (mc *MetaHttpClient) GetMetaPartition(pid uint64) (resp *GetMPInfoResp, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[GetMetaPartition],pid:%v,err:%v", pid, err)
		}
	}()
	req := newAPIRequest(http.MethodGet, "/getPartitionById")
	req.addParam("pid", fmt.Sprintf("%v", pid))
	respData, err := mc.serveRequest(req)
	log.LogInfof("err:%v,respData:%v\n", err, string(respData))
	if err != nil {
		return
	}
	resp = &GetMPInfoResp{}
	if err = json.Unmarshal(respData, resp); err != nil {
		return
	}
	return
}

func (mc *MetaHttpClient) GetAllDentry(pid uint64) (dentryMap map[string]*proto.MetaDentry, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[GetAllDentry],pid:%v,err:%v", pid, err)
		}
	}()
	dentryMap = make(map[string]*proto.MetaDentry, 0)
	req := newAPIRequest(http.MethodGet, "/getAllDentry")
	req.addParam("pid", fmt.Sprintf("%v", pid))
	respData, err := mc.serveRequest(req)
	log.LogInfof("err:%v,respData:%v\n", err, string(respData))
	if err != nil {
		return
	}

	dec := json.NewDecoder(bytes.NewBuffer(respData))
	dec.UseNumber()

	// It's the "items". We expect it to be an array
	if err = parseToken(dec, '['); err != nil {
		return
	}
	// Read items (large objects)
	for dec.More() {
		// Read next item (large object)
		lo := &proto.MetaDentry{}
		if err = dec.Decode(lo); err != nil {
			return
		}
		dentryMap[fmt.Sprintf("%v_%v", lo.ParentId, lo.Name)] = lo
	}
	// Array closing delimiter
	if err = parseToken(dec, ']'); err != nil {
		return
	}
	return
}

func parseToken(dec *json.Decoder, expectToken rune) (err error) {
	t, err := dec.Token()
	if err != nil {
		return
	}
	if delim, ok := t.(json.Delim); !ok || delim != json.Delim(expectToken) {
		err = fmt.Errorf("expected token[%v],delim[%v],ok[%v]", string(expectToken), delim, ok)
		return
	}
	return
}

func (mc *MetaHttpClient) GetAllInodes(pid uint64) (rstMap map[uint64]*proto.MetaInode, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[GetAllInodes],pid:%v,err:%v", pid, err)
		}
	}()

	inodeMap := make(map[uint64]*proto.MetaInode, 0)
	req := newAPIRequest(http.MethodGet, "/getAllInodes")
	req.addParam("pid", fmt.Sprintf("%v", pid))
	respData, err := mc.serveRequest(req)
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewBuffer(respData))
	dec.UseNumber()

	// It's the "items". We expect it to be an array
	if err = parseToken(dec, '['); err != nil {
		return
	}
	// Read items (large objects)
	for dec.More() {
		// Read next item (large object)
		in := &proto.MetaInode{}
		if err = dec.Decode(in); err != nil {
			return
		}
		inodeMap[in.Inode] = in
	}
	// Array closing delimiter
	if err = parseToken(dec, ']'); err != nil {
		return
	}

	return inodeMap, nil
}

func (mc *MetaHttpClient) ResetCursor(pid uint64, resetType string, newCursor uint64, force bool) (resp *proto.CursorResetResponse, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[GetAllInodes],pid:%v,err:%v", pid, err)
		}
	}()

	resp = &proto.CursorResetResponse{}
	req := newAPIRequest(http.MethodGet, "/cursorReset")
	req.addParam("pid", fmt.Sprintf("%v", pid))
	req.addParam("resetType", resetType)
	req.addParam("newCursor", fmt.Sprintf("%v", newCursor))
	if force {
		req.addParam("force", "1")
	}

	respData, err := mc.serveRequest(req)
	//fmt.Printf("err:%v,respData:%v\n", err, string(respData))
	if err != nil {
		return
	}

	err = json.Unmarshal(respData, resp)
	if err != nil {
		return
	}

	return
}

func (mc *MetaHttpClient) ListAllInodesId(pid uint64, mode uint32, stTime, endTime int64) (resp *proto.MpAllInodesId, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[GetAllInodes],pid:%v,err:%v", pid, err)
		}
	}()

	resp = &proto.MpAllInodesId{}
	req := newAPIRequest(http.MethodGet, "/getAllInodeId")
	req.addParam("pid", fmt.Sprintf("%v", pid))
	req.addParam("id", fmt.Sprintf("%v", pid))
	if mode != 0 {
		req.addParam("mode", fmt.Sprintf("%v", mode))
	}
	if stTime != 0 {
		req.addParam("start", fmt.Sprintf("%v", stTime))
	}
	if endTime != 0 {
		req.addParam("end", fmt.Sprintf("%v", endTime))
	}
	respData, err := mc.serveRequest(req)
	//fmt.Printf("err:%v,respData:%v\n", err, string(respData))
	if err != nil {
		return
	}

	err = json.Unmarshal(respData, resp)
	if err != nil {
		return
	}

	return
}

func (mc *MetaHttpClient) GetMetaNodeVersion() (versionInfo *proto.VersionValue, err error) {
	var (
		respData []byte
		url      string
		resp     *http.Response
		req      *request
	)
	req = newAPIRequest(http.MethodGet, proto.VersionPath)
	url = fmt.Sprintf("http://%s%s", mc.host, req.path)
	resp, err = mc.httpRequest(req.method, url, req.params, req.header, req.body)
	log.LogInfof("resp %v, url:%s, err %v", resp, url, err)
	if err != nil {
		log.LogErrorf("GetMetaNodeVersion: send http request fail: method(%v) url(%v) err(%v)", req.method, url, err)
		return
	}
	respData, err = ioutil.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		log.LogErrorf("serveRequest: read http response body fail: err(%v)", err)
		return
	}
	versionInfo = new(proto.VersionValue)
	if err = json.Unmarshal(respData, versionInfo); err != nil {
		return
	}
	return
}

func (mc *MetaHttpClient) GetExtentKeyByInodeId(metaPartitionId, inode uint64) (result *proto.GetExtentsResponse, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("GetExtentKeyByInodeId mp:%v, error:%v", metaPartitionId, err)
		}
	}()
	var data []byte
	var req *request
	if mc.isDBBack {
		req = newAPIRequest(http.MethodGet, "/getExtents")
	} else {
		req = newAPIRequest(http.MethodGet, "/getExtentsByInode")
	}
	req.params["pid"] = fmt.Sprintf("%v", metaPartitionId)
	req.params["ino"] = fmt.Sprintf("%v", inode)
	data, err = mc.serveRequest(req)
	if err != nil {
		err = errors.NewErrorf("get extent key by inode failed, error[%v]", err)
		return
	}

	//log.LogInfof("mp id:%v, inode id:%v, resp info:%s", metaPartitionId, inode, string(data))

	result = new(proto.GetExtentsResponse)
	if len(data) == 0 {
		result.Extents = make([]proto.ExtentKey, 0)
		log.LogInfof("[getExtentKeyByInodeId] inode:%v not exist", inode)
		return
	}

	if mc.isDBBack {
		getExtentsRespDBBck := new(proto.GetExtentsResponseDBBack)
		if err = json.Unmarshal(data, &getExtentsRespDBBck); err != nil {
			err = errors.NewErrorf("data:%s unmarshal extents key failed: %v", string(data), err)
			return
		}
		result.Extents = getExtentsRespDBBck.Extents
		return
	}

	if err = json.Unmarshal(data, &result); err != nil {
		err = errors.NewErrorf("data:%s unmarshal extents key failed: %v", string(data), err)
		return
	}
	return
}

func (mc *MetaHttpClient) GetInuseInodes(mpId uint64) (inodeInuseBitMap []uint64, err error) {
	req := newAPIRequest(http.MethodGet, "/getBitInuse")
	req.addParam("pid", fmt.Sprintf("%v", mpId))
	respData, err := mc.serveRequest(req)
	if err != nil {
		return
	}

	var respStruct = &struct {
		InoInuseBitMap []uint64 `json:"inodeInuseBitMap"`
	}{}
	if err = json.Unmarshal(respData, respStruct); err != nil {
		return
	}
	inodeInuseBitMap = respStruct.InoInuseBitMap
	return
}

func (mc *MetaHttpClient) GetInode(mpId uint64, inode uint64) (inodeInfoView *proto.InodeInfoView, err error) {
	req := newAPIRequest(http.MethodGet, "/getInode")
	req.addParam("pid", fmt.Sprintf("%v", mpId))
	req.addParam("ino", fmt.Sprintf("%v", inode))
	respData, err := mc.serveRequest(req)
	if err != nil {
		return
	}
	var inodeInfo map[string]interface{}
	if err = json.Unmarshal(respData, &inodeInfo); err != nil {
		return
	}
	dataInfo := inodeInfo["info"].(map[string]interface{})
	gid := float64(0)
	uid := float64(0)
	if !proto.IsDbBack {
		gid = dataInfo["gid"].(float64)
		uid = dataInfo["uid"].(float64)
	}
	inodeInfoView = &proto.InodeInfoView{
		Ino:         uint64(dataInfo["ino"].(float64)),
		PartitionID: mpId,
		At:          dataInfo["at"].(string),
		Ct:          dataInfo["ct"].(string),
		Mt:          dataInfo["mt"].(string),
		Nlink:       uint64(dataInfo["nlink"].(float64)),
		Size:        uint64(dataInfo["sz"].(float64)),
		Gen:         uint64(dataInfo["gen"].(float64)),
		Gid:         uint64(gid),
		Uid:         uint64(uid),
		Mode:        uint64(dataInfo["mode"].(float64)),
	}
	return
}

func (mc *MetaHttpClient) CreateInode(pid uint64, inode uint64, mode uint32) (err error) {
	defer func() {
		if err != nil {
			log.LogWarnf("action[CreateInode],mpId:%v,inode:%v,mode:%v,err:%v", pid, inode, mode, err)
		}
	}()

	req := newAPIRequest(http.MethodGet, "/createInodeForTest")
	req.addParam("pid", fmt.Sprintf("%v", pid))
	req.addParam("inoID", fmt.Sprintf("%v", inode))
	req.addParam("mode", fmt.Sprintf("%v", mode))

	if _, err = mc.serveRequest(req); err != nil {
		return
	}
	return
}

func (mc *MetaHttpClient) ListAllInodesIDAndDelInodesID(pid uint64, mode uint32, stTime, endTime int64) (resp *proto.MpAllInodesId, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[GetAllInodes],pid:%v,err:%v", pid, err)
		}
	}()

	resp = &proto.MpAllInodesId{}
	req := newAPIRequest(http.MethodGet, "/getAllInodeIdWithDeleted")
	req.addParam("pid", fmt.Sprintf("%v", pid))
	if mode != 0 {
		req.addParam("mode", fmt.Sprintf("%v", mode))
	}
	if stTime != 0 {
		req.addParam("start", fmt.Sprintf("%v", stTime))
	}
	if endTime != 0 {
		req.addParam("end", fmt.Sprintf("%v", endTime))
	}
	respData, err := mc.serveRequest(req)
	//fmt.Printf("err:%v,respData:%v\n", err, string(respData))
	if err != nil {
		return
	}

	err = json.Unmarshal(respData, resp)
	if err != nil {
		return
	}

	return
}

func (mc *MetaHttpClient) GetExtentKeyByDelInodeId(metaPartitionId, inode uint64) (result *proto.GetExtentsResponse, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("GetExtentKeyByDelInodeId mp:%v, error:%v", metaPartitionId, err)
		}
	}()
	var data []byte
	req := newAPIRequest(http.MethodGet, "/getExtentsByDelIno")
	req.params["pid"] = fmt.Sprintf("%v", metaPartitionId)
	req.params["ino"] = fmt.Sprintf("%v", inode)
	data, err = mc.serveRequest(req)
	if err != nil {
		err = errors.NewErrorf("get extent key by inode failed, error[%v]", err)
		return
	}

	//log.LogInfof("mp id:%v, inode id:%v, resp info:%s", metaPartitionId, inode, string(data))

	result = new(proto.GetExtentsResponse)
	if len(data) == 0 {
		result.Extents = make([]proto.ExtentKey, 0)
		log.LogInfof("[getExtentKeyByInodeId] inode:%v not exist", inode)
		return
	}

	result = new(proto.GetExtentsResponse)
	if err = json.Unmarshal(data, &result); err != nil {
		err = errors.NewErrorf("data:%s unmarshal extents key failed: %v", string(data), err)
		return
	}
	return
}

func (mc *MetaHttpClient) GetMetaDataCrcSum(pid uint64) (r *proto.MetaDataCRCSumInfo, err error) {
	req := newAPIRequest(http.MethodGet, "/getMetaDataCrcSum")
	req.params["pid"] = fmt.Sprintf("%v", pid)
	var data []byte
	data, err = mc.serveRequest(req)
	if err != nil {
		log.LogInfof("err:%v,respData:%v\n", err, string(data))
		return
	}
	r = new(proto.MetaDataCRCSumInfo)
	if err = json.Unmarshal(data, r); err != nil {
		err = fmt.Errorf("unmarshal failed:%v", err)
		return
	}
	return
}

func (mc *MetaHttpClient) GetInodesCrcSum(pid uint64) (result *proto.InodesCRCSumInfo, err error) {
	req := newAPIRequest(http.MethodGet, "/getInodesCrcSum")
	req.params["pid"] = fmt.Sprintf("%v", pid)
	var respData []byte
	respData, err = mc.serveRequest(req)
	if err != nil {
		log.LogErrorf("err:%v,respData:%v\n", err, string(respData))
		return
	}

	dec := json.NewDecoder(bytes.NewBuffer(respData))
	result = &proto.InodesCRCSumInfo{}
	if err = dec.Decode(result); err != nil {
		log.LogErrorf("decode failed:%v", err)
		return
	}
	return
}

func (mc *MetaHttpClient) GetDelInodesCrcSum(pid uint64) (result *proto.InodesCRCSumInfo, err error) {
	req := newAPIRequest(http.MethodGet, "/getDelInodesCrcSum")
	req.params["pid"] = fmt.Sprintf("%v", pid)
	var respData []byte
	respData, err = mc.serveRequest(req)
	if err != nil {
		log.LogErrorf("err:%v,respData:%v\n", err, string(respData))
		return
	}

	dec := json.NewDecoder(bytes.NewBuffer(respData))
	result = &proto.InodesCRCSumInfo{}
	if err = dec.Decode(result); err != nil {
		log.LogErrorf("decode failed:%v", err)
		return
	}
	return
}

func (mc *MetaHttpClient) GetInodeInfo(pid, ino uint64) (info *proto.InodeInfo, err error) {
	req := newAPIRequest(http.MethodGet, "/getInode")
	req.params["pid"] = fmt.Sprintf("%v", pid)
	req.params["ino"] = fmt.Sprintf("%v", ino)
	var respData []byte
	respData, err = mc.serveRequest(req)
	if err != nil {
		log.LogErrorf("err:%v,respData:%v\n", err, string(respData))
		return
	}

	dec := json.NewDecoder(bytes.NewBuffer(respData))
	r := &proto.InodeGetResponse{}
	if err = dec.Decode(r); err != nil {
		log.LogErrorf("decode failed:%v", err)
		return
	}
	info = r.Info
	return
}

func (mc *MetaHttpClient) GetDentry(pid uint64, name string) (den *proto.MetaDentry, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[GetDentry],pid:%v, name: %s, err:%v", pid, name, err)
		}
	}()

	var req *request
	if mc.isDBBack {
		req = newAPIRequest(http.MethodGet, "/getDentrySingle")
	} else {
		req = newAPIRequest(http.MethodGet, "/getDentry")
	}
	req.addParam("pid", fmt.Sprintf("%v", pid))
	req.addParam("name", name)
	respData, err := mc.serveRequest(req)
	log.LogInfof("err:%v,respData:%v\n", err, string(respData))
	if err != nil {
		return
	}

	den = new(proto.MetaDentry)
	if err = json.Unmarshal(respData, den); err != nil {
		return
	}
	return
}
