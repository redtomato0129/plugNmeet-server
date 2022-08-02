package models

import (
	"context"
	"errors"
	"github.com/go-redis/redis/v8"
	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/mynaparrot/plugnmeet-server/pkg/config"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
)

type EtherpadModel struct {
	SharedNotePad config.SharedNotePad
	NodeId        string
	Host          string
	ApiKey        string
	context       context.Context
	rc            *redis.Client
	rs            *RoomService
}

type EtherpadHttpRes struct {
	Code    int64             `json:"code"`
	Message string            `json:"message"`
	Data    EtherpadDataTypes `json:"data"`
}

type EtherpadDataTypes struct {
	AuthorID        string `json:"authorID"`
	GroupID         string `json:"groupID"`
	SessionID       string `json:"sessionID"`
	PadID           string `json:"padID"`
	ReadOnlyID      string `json:"readOnlyID"`
	TotalPads       int64  `json:"totalPads"`
	TotalSessions   int64  `json:"totalSessions"`
	TotalActivePads int64  `json:"totalActivePads"`
}

const (
	APIVersion  = "1.2.15"
	EtherpadKey = "pnm:etherpad:"
)

func NewEtherpadModel() *EtherpadModel {
	return &EtherpadModel{
		rc:            config.AppCnf.RDS,
		context:       context.Background(),
		SharedNotePad: config.AppCnf.SharedNotePad,
		rs:            NewRoomService(),
	}
}

type CreateSessionRes struct {
	PadId         string
	ReadOnlyPadId string
}

// CreateSession will create group, pad, session
// return padId, readonlyPadId
func (m *EtherpadModel) CreateSession(roomId string) (*CreateSessionRes, error) {
	if len(m.SharedNotePad.EtherpadHosts) < 1 {
		return nil, errors.New("need at least one etherpad host")
	}
	err := m.selectHost()
	if err != nil {
		return nil, err
	}

	res := new(CreateSessionRes)
	res.PadId = uuid.NewString()

	// step 1: create pad using session id
	r, err := m.createPad(res.PadId)
	if err != nil {
		return nil, err
	}
	if r.Code > 0 {
		return nil, errors.New(r.Message)
	}

	// step 2: create readonly pad
	r, err = m.createReadonlyPad(res.PadId)
	if err != nil {
		return nil, err
	}
	if r.Code > 0 {
		return nil, errors.New(r.Message)
	}
	res.ReadOnlyPadId = r.Data.ReadOnlyID

	// add roomId to redis for this node
	m.rc.SAdd(m.context, EtherpadKey+m.NodeId, roomId)

	// finally, update to room
	err = m.addPadToRoomMetadata(roomId, res)
	if err != nil {
		log.Errorln(err)
	}

	return res, nil
}

func (m *EtherpadModel) addPadToRoomMetadata(roomId string, c *CreateSessionRes) error {
	_, meta, err := m.rs.LoadRoomWithMetadata(roomId)
	if err != nil {
		return err
	}

	f := SharedNotePadFeatures{
		AllowedSharedNotePad: meta.Features.SharedNotePadFeatures.AllowedSharedNotePad,
		IsActive:             true,
		NodeId:               m.NodeId,
		Host:                 m.Host,
		NotePadId:            c.PadId,
		ReadOnlyPadId:        c.ReadOnlyPadId,
	}
	meta.Features.SharedNotePadFeatures = f

	_, err = m.rs.UpdateRoomMetadataByStruct(roomId, meta)
	if err != nil {
		log.Errorln(err)
	}

	return err
}

type CleanPadReq struct {
	RoomId string `json:"room_id" validate:"required"`
	NodeId string `json:"node_id" validate:"required"`
	PadId  string `json:"pad_id" validate:"required"`
}

// CleanPad will delete group, session & pad
func (m *EtherpadModel) CleanPad(roomId, nodeId, padId string) error {
	for _, h := range m.SharedNotePad.EtherpadHosts {
		if h.Id == nodeId {
			m.Host = h.Host
			m.ApiKey = h.ApiKey
		}
	}
	if m.Host == "" {
		return errors.New("no host found")
	}

	// step 1: delete pad
	vals := url.Values{}
	vals.Add("padID", padId)
	_, err := m.postToEtherpad("deletePad", vals)
	if err != nil {
		log.Errorln(err)
	}

	// add roomId to redis for this node
	_ = m.rc.SRem(m.context, EtherpadKey+nodeId, roomId)

	return nil
}

func (m *EtherpadModel) CleanAfterRoomEnd(roomId, metadata string) error {
	roomMeta := new(RoomMetadata)
	_ = json.Unmarshal([]byte(metadata), roomMeta)

	np := roomMeta.Features.SharedNotePadFeatures
	if !np.AllowedSharedNotePad {
		return nil
	}

	err := m.CleanPad(roomId, np.NodeId, np.NotePadId)
	if err != nil {
		log.Errorln(err)
	}

	return err
}

type ChangeEtherpadStatusReq struct {
	RoomId   string `json:"room_id" validate:"required"`
	IsActive bool   `json:"is_active"`
}

func (m *EtherpadModel) ChangeEtherpadStatus(r *ChangeEtherpadStatusReq) error {
	_, meta, err := m.rs.LoadRoomWithMetadata(r.RoomId)
	if err != nil {
		return err
	}

	meta.Features.SharedNotePadFeatures.IsActive = r.IsActive

	_, err = m.rs.UpdateRoomMetadataByStruct(r.RoomId, meta)
	if err != nil {
		log.Errorln(err)
	}

	return err
}

func (m *EtherpadModel) createPad(sessionId string) (*EtherpadHttpRes, error) {
	vals := url.Values{}
	vals.Add("padID", sessionId)

	res, err := m.postToEtherpad("createPad", vals)
	if err != nil {
		log.Errorln(err)
	}
	return res, err
}

func (m *EtherpadModel) createReadonlyPad(sessionId string) (*EtherpadHttpRes, error) {
	vals := url.Values{}
	vals.Add("padID", sessionId)

	res, err := m.postToEtherpad("getReadOnlyID", vals)
	if err != nil {
		log.Errorln(err)
	}
	return res, err
}

// selectHost will choose server based on simple active number
func (m *EtherpadModel) selectHost() error {
	type host struct {
		i      int
		id     string
		active int64
	}
	var hosts []host

	for i, h := range m.SharedNotePad.EtherpadHosts {
		ok := m.checkStatus(h)
		if ok {
			c := m.rc.SCard(m.context, EtherpadKey+h.Id)
			hosts = append(hosts, host{
				i:      i,
				id:     h.Id,
				active: c.Val(),
			})
		}
	}
	if len(hosts) == 0 {
		return errors.New("no active etherpad host found")
	}

	sort.Slice(hosts, func(i, j int) bool {
		return hosts[i].active < hosts[j].active
	})

	selectedHost := m.SharedNotePad.EtherpadHosts[hosts[0].i]
	m.NodeId = selectedHost.Id
	m.Host = selectedHost.Host
	m.ApiKey = selectedHost.ApiKey

	return nil
}

func (m *EtherpadModel) checkStatus(h config.EtherpadInfo) bool {
	m.Host = h.Host
	m.ApiKey = h.ApiKey

	vals := url.Values{}
	_, err := m.postToEtherpad("getStats", vals)
	if err != nil {
		log.Errorln(err)
		return false
	}

	return true
}

func (m *EtherpadModel) postToEtherpad(method string, vals url.Values) (*EtherpadHttpRes, error) {
	endPoint := m.Host + "/api/" + APIVersion + "/" + method
	vals.Add("apikey", m.ApiKey)

	en := vals.Encode()
	resp, err := http.Get(endPoint + "?" + en)
	if err != nil {
		return nil, errors.New("can't connect to host")
	}

	if resp.Status != "200 OK" {
		return nil, errors.New("error code: " + resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Errorln(err)
		return nil, err
	}

	mar := new(EtherpadHttpRes)
	err = json.Unmarshal(body, mar)
	if err != nil {
		log.Errorln(err)
		return nil, err
	}

	return mar, nil
}
