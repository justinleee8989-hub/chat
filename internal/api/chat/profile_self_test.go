package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	chatconstant "github.com/openimsdk/chat/pkg/common/constant"
	chatpb "github.com/openimsdk/chat/pkg/protocol/chat"
	"github.com/openimsdk/chat/pkg/protocol/common"
	"google.golang.org/grpc"
)

type selfProfileChatClient struct {
	chatpb.ChatClient
	req *chatpb.FindUserFullInfoReq
}

func (c *selfProfileChatClient) FindUserFullInfo(_ context.Context, req *chatpb.FindUserFullInfoReq, _ ...grpc.CallOption) (*chatpb.FindUserFullInfoResp, error) {
	c.req = req
	return &chatpb.FindUserFullInfoResp{Users: []*common.UserFullInfo{{UserID: "actor-1", Email: "self@example.test"}}}, nil
}

func TestFindSelfProfileUsesTokenActorAndReturnsOnlySelfEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &selfProfileChatClient{}
	api := &Api{chatClient: client}
	req := httptest.NewRequest(http.MethodPost, "/user/self", strings.NewReader(`{"userIDs":["other-2"]}`))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req
	ctx.Set(chatconstant.RpcOpUserID, "actor-1")
	ctx.Set(chatconstant.RpcOpUserType, []string{"1"})
	api.FindSelfProfile(ctx)

	if client.req == nil || len(client.req.UserIDs) != 1 || client.req.UserIDs[0] != "actor-1" {
		t.Fatalf("FindUserFullInfo request=%+v; response=%s; want token actor only", client.req, recorder.Body.String())
	}
	var response struct {
		ErrCode int `json:"errCode"`
		Data    struct {
			UserID string `json:"userID"`
			Email  string `json:"email"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if response.ErrCode != 0 || response.Data.UserID != "actor-1" || response.Data.Email != "self@example.test" {
		t.Fatalf("self profile response=%+v", response)
	}
	var data map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	dataObject, ok := data["data"].(map[string]any)
	if !ok || len(dataObject) != 2 {
		t.Fatalf("self endpoint returned unexpected fields: %#v", dataObject)
	}
}

func TestFindSelfProfileRejectsMissingTokenActor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &selfProfileChatClient{}
	api := &Api{chatClient: client}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/user/self", nil)
	api.FindSelfProfile(ctx)
	if client.req != nil {
		t.Fatalf("request sent without an authenticated actor: %+v", client.req)
	}
	var response struct {
		ErrCode int `json:"errCode"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if response.ErrCode == 0 {
		t.Fatalf("missing actor was accepted: %s", recorder.Body.String())
	}
}
