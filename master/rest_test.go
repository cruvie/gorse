// Copyright 2021 gorse Project Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emicklei/go-restful/v3"
	"github.com/go-viper/mapstructure/v2"
	"github.com/juju/errors"
	"github.com/samber/lo"
	"github.com/steinfletcher/apitest"
	"github.com/stretchr/testify/assert"
	"github.com/zhenghaoz/gorse/config"
	"github.com/zhenghaoz/gorse/model/click"
	"github.com/zhenghaoz/gorse/model/ranking"
	"github.com/zhenghaoz/gorse/protocol"
	"github.com/zhenghaoz/gorse/server"
	"github.com/zhenghaoz/gorse/storage/cache"
	"github.com/zhenghaoz/gorse/storage/data"
	"github.com/zhenghaoz/gorse/storage/meta"
	"google.golang.org/protobuf/proto"
)

const (
	mockMasterUsername = "admin"
	mockMasterPassword = "pass"
)

type mockServer struct {
	handler *restful.Container
	Master
}

func newMockServer(t *testing.T) (*mockServer, string) {
	s := new(mockServer)
	// open database
	var err error
	s.Settings = config.NewSettings()
	s.metaStore, err = meta.Open(fmt.Sprintf("sqlite://%s/meta.db", t.TempDir()), s.Config.Master.MetaTimeout)
	assert.NoError(t, err)
	s.DataClient, err = data.Open(fmt.Sprintf("sqlite://%s/data.db", t.TempDir()), "")
	assert.NoError(t, err)
	s.CacheClient, err = cache.Open(fmt.Sprintf("sqlite://%s/cache.db", t.TempDir()), "")
	assert.NoError(t, err)
	// init database
	err = s.metaStore.Init()
	assert.NoError(t, err)
	err = s.DataClient.Init()
	assert.NoError(t, err)
	err = s.CacheClient.Init()
	assert.NoError(t, err)
	// create server
	s.Config = config.GetDefaultConfig()
	s.Config.Master.DashboardUserName = mockMasterUsername
	s.Config.Master.DashboardPassword = mockMasterPassword
	s.WebService = new(restful.WebService)
	s.CreateWebService()
	s.RestServer.CreateWebService()
	// create handler
	s.handler = restful.NewContainer()
	s.handler.Add(s.WebService)
	// login
	req, err := http.NewRequest("POST", "/login",
		strings.NewReader(fmt.Sprintf("user_name=%s&password=%s", mockMasterUsername, mockMasterPassword)))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := httptest.NewRecorder()
	s.login(resp, req)
	assert.Equal(t, http.StatusFound, resp.Code)
	return s, resp.Header().Get("Set-Cookie")
}

func (s *mockServer) Close(t *testing.T) {
	err := s.metaStore.Close()
	assert.NoError(t, err)
	err = s.DataClient.Close()
	assert.NoError(t, err)
	err = s.CacheClient.Close()
	assert.NoError(t, err)
}

func marshal(t *testing.T, v interface{}) string {
	s, err := json.Marshal(v)
	assert.NoError(t, err)
	return string(s)
}

func marshalJSONLines[T any](t *testing.T, v []T) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, item := range v {
		err := encoder.Encode(item)
		assert.NoError(t, err)
	}
	return buf.String()
}

func convertToMapStructure(t *testing.T, v interface{}) map[string]interface{} {
	var m map[string]interface{}
	err := mapstructure.Decode(v, &m)
	assert.NoError(t, err)
	return m
}

func TestMaster_ExportUsers(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// insert users
	users := []data.User{
		{UserId: "1", Labels: map[string]any{"gender": "male", "job": "engineer"}},
		{UserId: "2", Labels: map[string]any{"gender": "male", "job": "lawyer"}},
		{UserId: "3", Labels: map[string]any{"gender": "female", "job": "teacher"}},
	}
	err := s.DataClient.BatchInsertUsers(ctx, users)
	assert.NoError(t, err)
	// send request
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	s.importExportUsers(w, req)
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Equal(t, "application/jsonl", w.Header().Get("Content-Type"))
	assert.Equal(t, "attachment;filename=users.jsonl", w.Header().Get("Content-Disposition"))
	assert.Equal(t, marshalJSONLines(t, users), w.Body.String())
}

func TestMaster_ExportItems(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// insert items
	items := []data.Item{
		{
			ItemId:     "1",
			IsHidden:   false,
			Categories: []string{"x"},
			Timestamp:  time.Date(2020, 1, 1, 1, 1, 1, 1, time.UTC),
			Labels:     map[string]any{"genre": []string{"comedy", "sci-fi"}},
			Comment:    "o,n,e",
		},
		{
			ItemId:     "2",
			IsHidden:   false,
			Categories: []string{"x", "y"},
			Timestamp:  time.Date(2021, 1, 1, 1, 1, 1, 1, time.UTC),
			Labels:     map[string]any{"genre": []string{"documentary", "sci-fi"}},
			Comment:    "t\r\nw\r\no",
		},
		{
			ItemId:     "3",
			IsHidden:   true,
			Categories: nil,
			Timestamp:  time.Date(2022, 1, 1, 1, 1, 1, 1, time.UTC),
			Labels:     nil,
			Comment:    "\"three\"",
		},
	}
	err := s.DataClient.BatchInsertItems(ctx, items)
	assert.NoError(t, err)
	// send request
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	s.importExportItems(w, req)
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Equal(t, "application/jsonl", w.Header().Get("Content-Type"))
	assert.Equal(t, "attachment;filename=items.jsonl", w.Header().Get("Content-Disposition"))
	assert.Equal(t, marshalJSONLines(t, items), w.Body.String())
}

func TestMaster_ExportFeedback(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)

	ctx := context.Background()
	// insert feedback
	feedbacks := []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "2"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "read", UserId: "2", ItemId: "6"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "share", UserId: "1", ItemId: "4"}},
	}
	err := s.DataClient.BatchInsertFeedback(ctx, feedbacks, true, true, true)
	assert.NoError(t, err)
	// send request
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	s.importExportFeedback(w, req)
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Equal(t, "application/jsonl", w.Header().Get("Content-Type"))
	assert.Equal(t, "attachment;filename=feedback.jsonl", w.Header().Get("Content-Disposition"))
	assert.Equal(t, marshalJSONLines(t, feedbacks), w.Body.String())
}

func TestMaster_ImportUsers(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// send request
	buf := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(buf)
	file, err := writer.CreateFormFile("file", "users.jsonl")
	assert.NoError(t, err)
	_, err = file.Write([]byte(`{"UserId":"1","Labels":{"性别":"男","职业":"工程师"}}
{"UserId":"2","Labels":{"性别":"男","职业":"律师"}}
{"UserId":"3","Labels":{"性别":"女","职业":"教师"}}`))
	assert.NoError(t, err)
	err = writer.Close()
	assert.NoError(t, err)
	req := httptest.NewRequest("POST", "https://example.com/", buf)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	s.importExportUsers(w, req)
	// check
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.JSONEq(t, marshal(t, server.Success{RowAffected: 3}), w.Body.String())
	_, items, err := s.DataClient.GetUsers(ctx, "", 100)
	assert.NoError(t, err)
	assert.Equal(t, []data.User{
		{UserId: "1", Labels: map[string]any{"性别": "男", "职业": "工程师"}},
		{UserId: "2", Labels: map[string]any{"性别": "男", "职业": "律师"}},
		{UserId: "3", Labels: map[string]any{"性别": "女", "职业": "教师"}},
	}, items)
}

func TestMaster_ImportItems(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// send request
	buf := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(buf)
	file, err := writer.CreateFormFile("file", "items.jsonl")
	assert.NoError(t, err)
	_, err = file.Write([]byte(`{"ItemId":"1","IsHidden":false,"Categories":["x"],"Timestamp":"2020-01-01 01:01:01.000000001 +0000 UTC","Labels":{"类型":["喜剧","科幻"]},"Comment":"one"}
{"ItemId":"2","IsHidden":false,"Categories":["x","y"],"Timestamp":"2021-01-01 01:01:01.000000001 +0000 UTC","Labels":{"类型":["卡通","科幻"]},"Comment":"two"}
{"ItemId":"3","IsHidden":true,"Timestamp":"2022-01-01 01:01:01.000000001 +0000 UTC","Comment":"three"}`))
	assert.NoError(t, err)
	err = writer.Close()
	assert.NoError(t, err)
	req := httptest.NewRequest("POST", "https://example.com/", buf)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	s.importExportItems(w, req)
	// check
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.JSONEq(t, marshal(t, server.Success{RowAffected: 3}), w.Body.String())
	_, items, err := s.DataClient.GetItems(ctx, "", 100, nil)
	assert.NoError(t, err)
	assert.Equal(t, []data.Item{
		{
			ItemId:     "1",
			IsHidden:   false,
			Categories: []string{"x"},
			Timestamp:  time.Date(2020, 1, 1, 1, 1, 1, 1, time.UTC),
			Labels:     map[string]any{"类型": []any{"喜剧", "科幻"}},
			Comment:    "one"},
		{
			ItemId:     "2",
			IsHidden:   false,
			Categories: []string{"x", "y"},
			Timestamp:  time.Date(2021, 1, 1, 1, 1, 1, 1, time.UTC),
			Labels:     map[string]any{"类型": []any{"卡通", "科幻"}},
			Comment:    "two",
		},
		{
			ItemId:     "3",
			IsHidden:   true,
			Categories: nil,
			Timestamp:  time.Date(2022, 1, 1, 1, 1, 1, 1, time.UTC),
			Labels:     nil,
			Comment:    "three",
		},
	}, items)
}

func TestMaster_ImportFeedback(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	// send request
	ctx := context.Background()
	buf := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(buf)
	file, err := writer.CreateFormFile("file", "feedback.jsonl")
	assert.NoError(t, err)
	_, err = file.Write([]byte(`{"FeedbackType":"click","UserId":"0","ItemId":"2","Timestamp":"0001-01-01 00:00:00 +0000 UTC"}
{"FeedbackType":"read","UserId":"2","ItemId":"6","Timestamp":"0001-01-01 00:00:00 +0000 UTC"}
{"FeedbackType":"share","UserId":"1","ItemId":"4","Timestamp":"0001-01-01 00:00:00 +0000 UTC"}`))
	assert.NoError(t, err)
	err = writer.Close()
	assert.NoError(t, err)
	req := httptest.NewRequest("POST", "https://example.com/", buf)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	s.importExportFeedback(w, req)
	// check
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.JSONEq(t, marshal(t, server.Success{RowAffected: 3}), w.Body.String())
	_, feedback, err := s.DataClient.GetFeedback(ctx, "", 100, nil, lo.ToPtr(time.Now()))
	assert.NoError(t, err)
	assert.Equal(t, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "2"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "read", UserId: "2", ItemId: "6"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "share", UserId: "1", ItemId: "4"}},
	}, feedback)
}

func TestMaster_GetCluster(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	// add nodes
	serverNode := &meta.Node{
		UUID:       "alan turnin",
		Hostname:   "192.168.1.100",
		Type:       protocol.NodeType_Server.String(),
		Version:    "server_version",
		UpdateTime: time.Now().UTC(),
	}
	workerNode := &meta.Node{
		UUID:       "dennis ritchie",
		Hostname:   "192.168.1.101",
		Type:       protocol.NodeType_Worker.String(),
		Version:    "worker_version",
		UpdateTime: time.Now().UTC(),
	}
	err := s.metaStore.UpdateNode(serverNode)
	assert.NoError(t, err)
	err = s.metaStore.UpdateNode(workerNode)
	assert.NoError(t, err)
	// get nodes
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/cluster").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, []*meta.Node{serverNode, workerNode})).
		End()
}

func TestMaster_GetStats(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)

	ctx := context.Background()
	// set stats
	s.rankingScore = ranking.Score{Precision: 0.1}
	s.clickScore = click.Score{Precision: 0.2}
	err := s.CacheClient.Set(ctx, cache.Integer(cache.Key(cache.GlobalMeta, cache.NumUsers), 123))
	assert.NoError(t, err)
	err = s.CacheClient.Set(ctx, cache.Integer(cache.Key(cache.GlobalMeta, cache.NumItems), 234))
	assert.NoError(t, err)
	err = s.CacheClient.Set(ctx, cache.Integer(cache.Key(cache.GlobalMeta, cache.NumValidPosFeedbacks), 345))
	assert.NoError(t, err)
	err = s.CacheClient.Set(ctx, cache.Integer(cache.Key(cache.GlobalMeta, cache.NumValidNegFeedbacks), 456))
	assert.NoError(t, err)
	// get stats
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/stats").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, Status{
			NumUsers:            123,
			NumItems:            234,
			NumValidPosFeedback: 345,
			NumValidNegFeedback: 456,
			MatchingModelScore:  ranking.Score{Precision: 0.1},
			RankingModelScore:   click.Score{Precision: 0.2},
			BinaryVersion:       "unknown-version",
		})).
		End()
}

func TestMaster_GetRates(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)

	ctx := context.Background()
	// write rates
	s.Config.Recommend.DataSource.PositiveFeedbackTypes = []string{"a", "b"}
	// This first measurement should be overwritten.
	baseTimestamp := time.Now()
	err := s.CacheClient.AddTimeSeriesPoints(ctx, []cache.TimeSeriesPoint{
		{Name: cache.Key(PositiveFeedbackRate, "a"), Value: 100.0, Timestamp: baseTimestamp.Add(-2 * 24 * time.Hour)},
		{Name: cache.Key(PositiveFeedbackRate, "a"), Value: 2.0, Timestamp: baseTimestamp.Add(-2 * 24 * time.Hour)},
		{Name: cache.Key(PositiveFeedbackRate, "a"), Value: 2.0, Timestamp: baseTimestamp.Add(-1 * 24 * time.Hour)},
		{Name: cache.Key(PositiveFeedbackRate, "a"), Value: 3.0, Timestamp: baseTimestamp.Add(-0 * 24 * time.Hour)},
		{Name: cache.Key(PositiveFeedbackRate, "b"), Value: 20.0, Timestamp: baseTimestamp.Add(-2 * 24 * time.Hour)},
		{Name: cache.Key(PositiveFeedbackRate, "b"), Value: 20.0, Timestamp: baseTimestamp.Add(-1 * 24 * time.Hour)},
		{Name: cache.Key(PositiveFeedbackRate, "b"), Value: 30.0, Timestamp: baseTimestamp.Add(-0 * 24 * time.Hour)},
	})
	assert.NoError(t, err)

	// get rates
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/rates").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, map[string][]cache.TimeSeriesPoint{
			"a": {
				{Name: cache.Key(PositiveFeedbackRate, "a"), Value: 2.0, Timestamp: baseTimestamp.Add(-2 * 24 * time.Hour)},
				{Name: cache.Key(PositiveFeedbackRate, "a"), Value: 2.0, Timestamp: baseTimestamp.Add(-1 * 24 * time.Hour)},
				{Name: cache.Key(PositiveFeedbackRate, "a"), Value: 3.0, Timestamp: baseTimestamp.Add(-0 * 24 * time.Hour)},
			},
			"b": {
				{Name: cache.Key(PositiveFeedbackRate, "b"), Value: 20.0, Timestamp: baseTimestamp.Add(-2 * 24 * time.Hour)},
				{Name: cache.Key(PositiveFeedbackRate, "b"), Value: 20.0, Timestamp: baseTimestamp.Add(-1 * 24 * time.Hour)},
				{Name: cache.Key(PositiveFeedbackRate, "b"), Value: 30.0, Timestamp: baseTimestamp.Add(-0 * 24 * time.Hour)},
			},
		})).
		End()
}

func TestMaster_GetCategories(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// insert categories
	err := s.CacheClient.SetSet(ctx, cache.ItemCategories, "a", "b", "c")
	assert.NoError(t, err)
	// get categories
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/categories").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, []string{"a", "b", "c"})).
		End()
}

func TestMaster_GetUsers(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// add users
	users := []User{
		{data.User{UserId: "0"}, time.Date(2000, 1, 1, 1, 1, 1, 1, time.UTC), time.Date(2020, 1, 1, 1, 1, 1, 1, time.UTC)},
		{data.User{UserId: "1"}, time.Date(2001, 1, 1, 1, 1, 1, 1, time.UTC), time.Date(2021, 1, 1, 1, 1, 1, 1, time.UTC)},
		{data.User{UserId: "2"}, time.Date(2002, 1, 1, 1, 1, 1, 1, time.UTC), time.Date(2022, 1, 1, 1, 1, 1, 1, time.UTC)},
	}
	for _, user := range users {
		err := s.DataClient.BatchInsertUsers(ctx, []data.User{user.User})
		assert.NoError(t, err)
		err = s.CacheClient.Set(ctx, cache.Time(cache.Key(cache.LastModifyUserTime, user.UserId), user.LastActiveTime))
		assert.NoError(t, err)
		err = s.CacheClient.Set(ctx, cache.Time(cache.Key(cache.LastUpdateUserRecommendTime, user.UserId), user.LastUpdateTime))
		assert.NoError(t, err)
	}
	// get users
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/users").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, UserIterator{
			Cursor: "",
			Users:  users,
		})).
		End()
	// get a user
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/user/1").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, users[1])).
		End()
}

func TestServer_SearchDocumentsOfItems(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	type ListOperator struct {
		Name       string
		Collection string
		Subset     string
		Category   string
		Get        string
	}
	ctx := context.Background()
	operators := []ListOperator{
		{"ItemToItem", cache.ItemToItem, cache.Key(cache.Neighbors, "0"), "", "/api/dashboard/item-to-item/neighbors/0"},
		{"ItemToItemCategory", cache.ItemToItem, cache.Key(cache.Neighbors, "0"), "*", "/api/dashboard/item-to-item/neighbors/0"},
		{"LatestItems", cache.NonPersonalized, cache.Latest, "", "/api/dashboard/non-personalized/latest/"},
		{"PopularItems", cache.NonPersonalized, cache.Popular, "", "/api/dashboard/non-personalized/popular/"},
		{"LatestItemsCategory", cache.NonPersonalized, cache.Latest, "*", "/api/dashboard/non-personalized/latest/"},
		{"PopularItemsCategory", cache.NonPersonalized, cache.Popular, "*", "/api/dashboard/non-personalized/popular/"},
	}
	for i, operator := range operators {
		t.Run(operator.Name, func(t *testing.T) {
			// Put scores
			scores := []cache.Score{
				{Id: strconv.Itoa(i) + "0", Score: 100, Categories: []string{operator.Category}},
				{Id: strconv.Itoa(i) + "1", Score: 99, Categories: []string{operator.Category}},
				{Id: strconv.Itoa(i) + "2", Score: 98, Categories: []string{operator.Category}},
				{Id: strconv.Itoa(i) + "3", Score: 97, Categories: []string{operator.Category}},
				{Id: strconv.Itoa(i) + "4", Score: 96, Categories: []string{operator.Category}},
			}
			err := s.CacheClient.AddScores(ctx, operator.Collection, operator.Subset, scores)
			assert.NoError(t, err)
			items := make([]ScoredItem, 0)
			for _, score := range scores {
				items = append(items, ScoredItem{Item: data.Item{ItemId: score.Id}, Score: score.Score})
				err = s.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: score.Id}})
				assert.NoError(t, err)
			}
			// hide item
			apitest.New().
				Handler(s.handler).
				Patch("/api/item/"+strconv.Itoa(i)+"3").
				Header("Cookie", cookie).
				JSON(data.ItemPatch{IsHidden: proto.Bool(true)}).
				Expect(t).
				Status(http.StatusOK).
				End()
			apitest.New().
				Handler(s.handler).
				Get(operator.Get).
				Header("Cookie", cookie).
				Query("category", operator.Category).
				Expect(t).
				Status(http.StatusOK).
				Body(marshal(t, []ScoredItem{items[0], items[1], items[2], items[4]})).
				End()
		})
	}
}

func TestServer_SearchDocumentsOfUsers(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	type ListOperator struct {
		Prefix string
		Label  string
		Get    string
	}
	ctx := context.Background()
	operators := []ListOperator{
		{cache.UserToUser, cache.Key(cache.Neighbors, "0"), "/api/dashboard/user-to-user/neighbors/0/"},
	}
	for _, operator := range operators {
		t.Logf("test RESTful API: %v", operator.Get)
		// Put scores
		scores := []cache.Score{
			{Id: "0", Score: 100, Categories: []string{""}},
			{Id: "1", Score: 99, Categories: []string{""}},
			{Id: "2", Score: 98, Categories: []string{""}},
			{Id: "3", Score: 97, Categories: []string{""}},
			{Id: "4", Score: 96, Categories: []string{""}},
		}
		err := s.CacheClient.AddScores(ctx, operator.Prefix, operator.Label, scores)
		assert.NoError(t, err)
		users := make([]ScoreUser, 0)
		for _, score := range scores {
			users = append(users, ScoreUser{User: data.User{UserId: score.Id}, Score: score.Score})
			err = s.DataClient.BatchInsertUsers(ctx, []data.User{{UserId: score.Id}})
			assert.NoError(t, err)
		}
		apitest.New().
			Handler(s.handler).
			Get(operator.Get).
			Header("Cookie", cookie).
			Expect(t).
			Status(http.StatusOK).
			Body(marshal(t, users)).
			End()
	}
}

func TestServer_Feedback(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// insert feedback
	feedback := []Feedback{
		{FeedbackType: "click", UserId: "0", Item: data.Item{ItemId: "0"}},
		{FeedbackType: "click", UserId: "0", Item: data.Item{ItemId: "2"}},
		{FeedbackType: "click", UserId: "0", Item: data.Item{ItemId: "4"}},
		{FeedbackType: "click", UserId: "0", Item: data.Item{ItemId: "6"}},
		{FeedbackType: "click", UserId: "0", Item: data.Item{ItemId: "8"}},
	}
	for _, v := range feedback {
		err := s.DataClient.BatchInsertFeedback(ctx, []data.Feedback{{
			FeedbackKey: data.FeedbackKey{FeedbackType: v.FeedbackType, UserId: v.UserId, ItemId: v.Item.ItemId},
		}}, true, true, true)
		assert.NoError(t, err)
	}
	// get feedback
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/user/0/feedback/click").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, feedback)).
		End()
}

func TestServer_GetRecommends(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	// inset recommendation
	itemIds := []cache.Score{
		{Id: "1", Score: 99, Categories: []string{""}},
		{Id: "2", Score: 98, Categories: []string{""}},
		{Id: "3", Score: 97, Categories: []string{""}},
		{Id: "4", Score: 96, Categories: []string{""}},
		{Id: "5", Score: 95, Categories: []string{""}},
		{Id: "6", Score: 94, Categories: []string{""}},
		{Id: "7", Score: 93, Categories: []string{""}},
		{Id: "8", Score: 92, Categories: []string{""}},
	}
	ctx := context.Background()
	err := s.CacheClient.AddScores(ctx, cache.OfflineRecommend, "0", itemIds)
	assert.NoError(t, err)
	// insert feedback
	feedback := []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "0", ItemId: "2"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "0", ItemId: "4"}},
	}
	err = s.DataClient.BatchInsertFeedback(ctx, feedback, true, true, true)
	assert.NoError(t, err)
	// insert items
	for _, item := range itemIds {
		err = s.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: item.Id}})
		assert.NoError(t, err)
	}
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/recommend/0/offline").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, []data.Item{
			{ItemId: "1"}, {ItemId: "3"}, {ItemId: "5"}, {ItemId: "6"}, {ItemId: "7"}, {ItemId: "8"},
		})).
		End()

	s.Config.Recommend.Online.FallbackRecommend = []string{"collaborative", "item_based", "user_based", "latest", "popular"}
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/recommend/0/_").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, []data.Item{
			{ItemId: "1"}, {ItemId: "3"}, {ItemId: "5"}, {ItemId: "6"}, {ItemId: "7"}, {ItemId: "8"},
		})).
		End()
}

func TestMaster_Purge(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)

	ctx := context.Background()
	// insert data
	err := s.CacheClient.Set(ctx, cache.String("key", "value"))
	assert.NoError(t, err)
	ret, err := s.CacheClient.Get(ctx, "key").String()
	assert.NoError(t, err)
	assert.Equal(t, "value", ret)

	err = s.CacheClient.AddSet(ctx, "set", "a", "b", "c")
	assert.NoError(t, err)
	set, err := s.CacheClient.GetSet(ctx, "set")
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, set)

	err = s.CacheClient.AddScores(ctx, "sorted", "", []cache.Score{
		{Id: "a", Score: 1, Categories: []string{""}},
		{Id: "b", Score: 2, Categories: []string{""}},
		{Id: "c", Score: 3, Categories: []string{""}}})
	assert.NoError(t, err)
	z, err := s.CacheClient.SearchScores(ctx, "sorted", "", []string{""}, 0, -1)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []cache.Score{
		{Id: "a", Score: 1, Categories: []string{""}},
		{Id: "b", Score: 2, Categories: []string{""}},
		{Id: "c", Score: 3, Categories: []string{""}}}, z)

	err = s.DataClient.BatchInsertFeedback(ctx, lo.Map(lo.Range(100), func(t int, i int) data.Feedback {
		return data.Feedback{FeedbackKey: data.FeedbackKey{
			FeedbackType: "click",
			UserId:       strconv.Itoa(t),
			ItemId:       strconv.Itoa(t),
		}}
	}), true, true, true)
	assert.NoError(t, err)
	_, users, err := s.DataClient.GetUsers(ctx, "", 100)
	assert.NoError(t, err)
	assert.Equal(t, 100, len(users))
	_, items, err := s.DataClient.GetItems(ctx, "", 100, nil)
	assert.NoError(t, err)
	assert.Equal(t, 100, len(items))
	_, feedbacks, err := s.DataClient.GetFeedback(ctx, "", 100, nil, lo.ToPtr(time.Now()))
	assert.NoError(t, err)
	assert.Equal(t, 100, len(feedbacks))

	// purge data
	req := httptest.NewRequest("POST", "https://example.com/",
		strings.NewReader("check_list=delete_users,delete_items,delete_feedback,delete_cache"))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.purge(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	_, err = s.CacheClient.Get(ctx, "key").String()
	assert.ErrorIs(t, err, errors.NotFound)
	set, err = s.CacheClient.GetSet(ctx, "set")
	assert.NoError(t, err)
	assert.Empty(t, set)
	z, err = s.CacheClient.SearchScores(ctx, "sorted", "", []string{""}, 0, -1)
	assert.NoError(t, err)
	assert.Empty(t, z)

	_, users, err = s.DataClient.GetUsers(ctx, "", 100)
	assert.NoError(t, err)
	assert.Empty(t, users)
	_, items, err = s.DataClient.GetItems(ctx, "", 100, nil)
	assert.NoError(t, err)
	assert.Empty(t, items)
	_, feedbacks, err = s.DataClient.GetFeedback(ctx, "", 100, nil, lo.ToPtr(time.Now()))
	assert.NoError(t, err)
	assert.Empty(t, feedbacks)
}

func TestMaster_GetConfig(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)

	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/config").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, formatConfig(convertToMapStructure(t, s.Config)))).
		End()

	s.Config.Master.DashboardRedacted = true
	redactedConfig := formatConfig(convertToMapStructure(t, s.Config))
	delete(redactedConfig, "database")
	apitest.New().
		Handler(s.handler).
		Get("/api/dashboard/config").
		Header("Cookie", cookie).
		Expect(t).
		Status(http.StatusOK).
		Body(marshal(t, redactedConfig)).
		End()
}

func TestDumpAndRestore(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// insert users
	users := make([]data.User, batchSize+1)
	for i := range users {
		users[i] = data.User{
			UserId: fmt.Sprintf("%05d", i),
			Labels: map[string]any{"a": fmt.Sprintf("%d", 2*i+1), "b": fmt.Sprintf("%d", 2*i+2)},
		}
	}
	err := s.DataClient.BatchInsertUsers(ctx, users)
	assert.NoError(t, err)
	// insert items
	items := make([]data.Item, batchSize+1)
	for i := range items {
		items[i] = data.Item{
			ItemId: fmt.Sprintf("%05d", i),
			Labels: map[string]any{"a": fmt.Sprintf("%d", 2*i+1), "b": fmt.Sprintf("%d", 2*i+2)},
		}
	}
	err = s.DataClient.BatchInsertItems(ctx, items)
	assert.NoError(t, err)
	// insert feedback
	feedback := make([]data.Feedback, batchSize+1)
	for i := range feedback {
		feedback[i] = data.Feedback{
			FeedbackKey: data.FeedbackKey{
				FeedbackType: "click",
				UserId:       fmt.Sprintf("%05d", i),
				ItemId:       fmt.Sprintf("%05d", i),
			},
		}
	}
	err = s.DataClient.BatchInsertFeedback(ctx, feedback, true, true, true)
	assert.NoError(t, err)

	// dump data
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	s.dump(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// restore data
	err = s.DataClient.Purge()
	assert.NoError(t, err)
	req = httptest.NewRequest("POST", "https://example.com/", bytes.NewReader(w.Body.Bytes()))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/octet-stream")
	w = httptest.NewRecorder()
	s.restore(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// check data
	_, returnUsers, err := s.DataClient.GetUsers(ctx, "", len(users))
	assert.NoError(t, err)
	if assert.Equal(t, len(users), len(returnUsers)) {
		assert.Equal(t, users, returnUsers)
	}
	_, returnItems, err := s.DataClient.GetItems(ctx, "", len(items), nil)
	assert.NoError(t, err)
	if assert.Equal(t, len(items), len(returnItems)) {
		assert.Equal(t, items, returnItems)
	}
	_, returnFeedback, err := s.DataClient.GetFeedback(ctx, "", len(feedback), nil, lo.ToPtr(time.Now()))
	assert.NoError(t, err)
	if assert.Equal(t, len(feedback), len(returnFeedback)) {
		assert.Equal(t, feedback, returnFeedback)
	}
}

func TestExportAndImport(t *testing.T) {
	s, cookie := newMockServer(t)
	defer s.Close(t)
	ctx := context.Background()
	// insert users
	users := make([]data.User, batchSize+1)
	for i := range users {
		users[i] = data.User{
			UserId: fmt.Sprintf("%05d", i),
			Labels: map[string]any{"a": fmt.Sprintf("%d", 2*i+1), "b": fmt.Sprintf("%d", 2*i+2)},
		}
	}
	err := s.DataClient.BatchInsertUsers(ctx, users)
	assert.NoError(t, err)
	// insert items
	items := make([]data.Item, batchSize+1)
	for i := range items {
		items[i] = data.Item{
			ItemId: fmt.Sprintf("%05d", i),
			Labels: map[string]any{"a": fmt.Sprintf("%d", 2*i+1), "b": fmt.Sprintf("%d", 2*i+2)},
		}
	}
	err = s.DataClient.BatchInsertItems(ctx, items)
	assert.NoError(t, err)
	// insert feedback
	feedback := make([]data.Feedback, batchSize+1)
	for i := range feedback {
		feedback[i] = data.Feedback{
			FeedbackKey: data.FeedbackKey{
				FeedbackType: "click",
				UserId:       fmt.Sprintf("%05d", i),
				ItemId:       fmt.Sprintf("%05d", i),
			},
		}
	}
	err = s.DataClient.BatchInsertFeedback(ctx, feedback, true, true, true)
	assert.NoError(t, err)

	// export users
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	s.importExportUsers(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	usersData := w.Body.Bytes()
	// export items
	req = httptest.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Cookie", cookie)
	w = httptest.NewRecorder()
	s.importExportItems(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	itemsData := w.Body.Bytes()
	// export feedback
	req = httptest.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Cookie", cookie)
	w = httptest.NewRecorder()
	s.importExportFeedback(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	feedbackData := w.Body.Bytes()

	err = s.DataClient.Purge()
	assert.NoError(t, err)
	// import users
	buf := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(buf)
	file, err := writer.CreateFormFile("file", "users.jsonl")
	assert.NoError(t, err)
	_, err = file.Write(usersData)
	assert.NoError(t, err)
	err = writer.Close()
	assert.NoError(t, err)
	req = httptest.NewRequest("POST", "https://example.com/", buf)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w = httptest.NewRecorder()
	s.importExportUsers(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	// import items
	buf = bytes.NewBuffer(nil)
	writer = multipart.NewWriter(buf)
	file, err = writer.CreateFormFile("file", "items.jsonl")
	assert.NoError(t, err)
	_, err = file.Write(itemsData)
	assert.NoError(t, err)
	err = writer.Close()
	assert.NoError(t, err)
	req = httptest.NewRequest("POST", "https://example.com/", buf)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w = httptest.NewRecorder()
	s.importExportItems(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	// import feedback
	buf = bytes.NewBuffer(nil)
	writer = multipart.NewWriter(buf)
	file, err = writer.CreateFormFile("file", "feedback.jsonl")
	assert.NoError(t, err)
	_, err = file.Write(feedbackData)
	assert.NoError(t, err)
	err = writer.Close()
	assert.NoError(t, err)
	req = httptest.NewRequest("POST", "https://example.com/", buf)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w = httptest.NewRecorder()
	s.importExportFeedback(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// check data
	_, returnUsers, err := s.DataClient.GetUsers(ctx, "", len(users))
	assert.NoError(t, err)
	if assert.Equal(t, len(users), len(returnUsers)) {
		assert.Equal(t, users, returnUsers)
	}
	_, returnItems, err := s.DataClient.GetItems(ctx, "", len(items), nil)
	assert.NoError(t, err)
	if assert.Equal(t, len(items), len(returnItems)) {
		assert.Equal(t, items, returnItems)
	}
	_, returnFeedback, err := s.DataClient.GetFeedback(ctx, "", len(feedback), nil, lo.ToPtr(time.Now()))
	assert.NoError(t, err)
	if assert.Equal(t, len(feedback), len(returnFeedback)) {
		assert.Equal(t, feedback, returnFeedback)
	}
}
