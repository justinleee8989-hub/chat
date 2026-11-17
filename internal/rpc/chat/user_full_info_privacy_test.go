package chat

import (
	"context"
	"testing"

	chatpb "github.com/openimsdk/chat/pkg/protocol/chat"

	"github.com/openimsdk/chat/pkg/common/constant"
	"github.com/openimsdk/chat/pkg/protocol/common"
)

func TestRedactFullInfoEmailsForNonSelfUser(t *testing.T) {
	users := []*common.UserFullInfo{
		{UserID: "actor-1", Email: "self@example.test"},
		{UserID: "other-2", Email: "other@example.test"},
	}
	redactFullInfoEmails("actor-1", constant.NormalUser, users)
	if users[0].Email != "self@example.test" || users[1].Email != "" {
		t.Fatalf("non-admin email visibility: self=%q other=%q", users[0].Email, users[1].Email)
	}
}

func TestRedactFullInfoEmailsKeepsAdminResults(t *testing.T) {
	users := []*common.UserFullInfo{{UserID: "other-2", Email: "other@example.test"}}
	redactFullInfoEmails("admin-1", constant.AdminUser, users)
	if users[0].Email != "other@example.test" {
		t.Fatalf("admin email was redacted: %q", users[0].Email)
	}
}

func TestRedactPublicInfoEmailsForNonSelfUser(t *testing.T) {
	users := []*common.UserPublicInfo{
		{UserID: "actor-1", Email: "self@example.test"},
		{UserID: "other-2", Email: "other@example.test"},
	}
	redactPublicInfoEmails("actor-1", constant.NormalUser, users)
	if users[0].Email != "self@example.test" || users[1].Email != "" {
		t.Fatalf("non-admin public email visibility: self=%q other=%q", users[0].Email, users[1].Email)
	}
}

func TestRedactPublicInfoEmailsKeepsAdminResults(t *testing.T) {
	users := []*common.UserPublicInfo{{UserID: "other-2", Email: "other@example.test"}}
	redactPublicInfoEmails("admin-1", constant.AdminUser, users)
	if users[0].Email != "other@example.test" {
		t.Fatalf("admin public email was redacted: %q", users[0].Email)
	}
}

func TestFindUserPublicInfoRequiresActorContext(t *testing.T) {
	service := &chatSvr{}
	_, err := service.FindUserPublicInfo(context.Background(), &chatpb.FindUserPublicInfoReq{UserIDs: []string{"other-2"}})
	if err == nil {
		t.Fatal("public info query without an actor context was accepted")
	}
}
