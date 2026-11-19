package chat

import (
	"context"
	"testing"

	"github.com/openimsdk/chat/pkg/common/constant"
	"github.com/openimsdk/chat/pkg/common/db/database"
	chatdb "github.com/openimsdk/chat/pkg/common/db/table/chat"
	chatpb "github.com/openimsdk/chat/pkg/protocol/chat"
	"go.mongodb.org/mongo-driver/mongo"
)

// contactDatabase holds credentials by account and one attribute per user.
type contactDatabase struct {
	database.ChatDatabaseInterface
	credentials map[string]*chatdb.Credential
}

func (d *contactDatabase) TakeCredentialByAccount(_ context.Context, account string) (*chatdb.Credential, error) {
	if credential, ok := d.credentials[account]; ok {
		return credential, nil
	}
	return nil, mongo.ErrNoDocuments
}

func (d *contactDatabase) FindAttribute(_ context.Context, userIDs []string) ([]*chatdb.Attribute, error) {
	attributes := make([]*chatdb.Attribute, 0, len(userIDs))
	for _, userID := range userIDs {
		attributes = append(attributes, &chatdb.Attribute{UserID: userID, Nickname: userID, Email: userID + "@example.test"})
	}
	return attributes, nil
}

func contactActor() context.Context {
	ctx := context.WithValue(context.Background(), constant.RpcOpUserID, "self")
	return context.WithValue(ctx, constant.RpcOpUserType, []string{"1"})
}

func TestFindUserByContactRequiresActorContext(t *testing.T) {
	service := &chatSvr{}
	_, err := service.FindUserByContact(context.Background(), &chatpb.FindUserByContactReq{Email: "other@example.test"})
	if err == nil {
		t.Fatal("contact lookup without an actor context was accepted")
	}
}

func TestFindUserByContactMatchesOnlyThatKindOfCredential(t *testing.T) {
	service := &chatSvr{Database: &contactDatabase{credentials: map[string]*chatdb.Credential{
		"bob@example.test": {UserID: "bob", Account: "bob@example.test", Type: constant.CredentialEmail},
		"+86 13800138000":  {UserID: "carol", Account: "+86 13800138000", Type: constant.CredentialPhone},
		"dave":             {UserID: "dave", Account: "dave", Type: constant.CredentialAccount},
	}}}
	cases := []struct {
		req  *chatpb.FindUserByContactReq
		want string
	}{
		{&chatpb.FindUserByContactReq{Email: "bob@example.test"}, "bob"},
		{&chatpb.FindUserByContactReq{AreaCode: "+86", PhoneNumber: "13800138000"}, "carol"},
		{&chatpb.FindUserByContactReq{Email: "nobody@example.test"}, ""},
		// An account name sent as an email is not an email match.
		{&chatpb.FindUserByContactReq{Email: "dave"}, ""},
	}
	for _, c := range cases {
		resp, err := service.FindUserByContact(contactActor(), c.req)
		if err != nil {
			t.Fatalf("%+v: %v", c.req, err)
		}
		got := ""
		if resp.User != nil {
			got = resp.User.UserID
			if resp.User.Email != "" {
				t.Fatalf("%+v: another user's email was returned", c.req)
			}
		}
		if got != c.want {
			t.Fatalf("%+v: found %q, want %q", c.req, got, c.want)
		}
	}
}

func TestFindUserByContactReqTakesExactlyOneContact(t *testing.T) {
	valid := []*chatpb.FindUserByContactReq{
		{Email: "other@example.test"},
		{AreaCode: "+86", PhoneNumber: "13800138000"},
	}
	for _, req := range valid {
		if err := req.Check(); err != nil {
			t.Fatalf("valid request %+v was refused: %v", req, err)
		}
	}
	invalid := []*chatpb.FindUserByContactReq{
		{},
		{Email: "other@example.test", AreaCode: "+86", PhoneNumber: "13800138000"},
		{PhoneNumber: "13800138000"},
	}
	for _, req := range invalid {
		if err := req.Check(); err == nil {
			t.Fatalf("invalid request %+v was accepted", req)
		}
	}
}
