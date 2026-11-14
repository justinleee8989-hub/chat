// Copyright © 2023 OpenIM open source community. All rights reserved.
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

package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/openimsdk/tools/db/mongoutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/openimsdk/chat/pkg/common/db/table/chat"
	"github.com/openimsdk/tools/errs"
)

type mongoVerifyCode struct {
	ID         primitive.ObjectID `bson:"_id"`
	Account    string             `bson:"account"`
	Platform   string             `bson:"platform"`
	Code       string             `bson:"code"`
	Duration   uint               `bson:"duration"`
	Count      int                `bson:"count"`
	Used       bool               `bson:"used"`
	CreateTime time.Time          `bson:"create_time"`
}

func NewVerifyCode(db *mongo.Database) (chat.VerifyCodeInterface, error) {
	coll := db.Collection("verify_code")
	_, err := coll.Indexes().CreateMany(context.Background(), []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "account", Value: 1},
			},
		},
	})
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return &VerifyCode{
		coll: coll,
	}, nil
}

type VerifyCode struct {
	coll *mongo.Collection
}

func (o *VerifyCode) parseID(s string) (primitive.ObjectID, error) {
	objID, err := primitive.ObjectIDFromHex(s)
	if err != nil {
		var zero primitive.ObjectID
		return zero, errs.Wrap(err)
	}
	return objID, nil
}

func (o *VerifyCode) Add(ctx context.Context, ms []*chat.VerifyCode) error {
	tmp := make([]mongoVerifyCode, 0, len(ms))
	for i, m := range ms {
		var objID primitive.ObjectID
		if m.ID == "" {
			objID = primitive.NewObjectID()
			ms[i].ID = objID.Hex()
		} else {
			var err error
			objID, err = o.parseID(m.ID)
			if err != nil {
				return err
			}
		}
		tmp = append(tmp, mongoVerifyCode{
			ID:         objID,
			Account:    m.Account,
			Platform:   m.Platform,
			Code:       m.Code,
			Duration:   m.Duration,
			Count:      m.Count,
			Used:       m.Used,
			CreateTime: m.CreateTime,
		})
	}
	return mongoutil.InsertMany(ctx, o.coll, tmp)
}

func (o *VerifyCode) RangeNum(ctx context.Context, account string, start time.Time, end time.Time) (int64, error) {
	filter := bson.M{
		"account": account,
		"create_time": bson.M{
			"$gte": start,
			"$lte": end,
		},
	}
	return mongoutil.Count(ctx, o.coll, filter)
}

func (o *VerifyCode) TakeLast(ctx context.Context, account string) (*chat.VerifyCode, error) {
	filter := bson.M{
		"account": account,
	}
	opt := options.FindOne().SetSort(bson.M{"_id": -1})
	last, err := mongoutil.FindOne[*mongoVerifyCode](ctx, o.coll, filter, opt)
	if err != nil {
		return nil, err
	}
	return &chat.VerifyCode{
		ID:         last.ID.Hex(),
		Account:    last.Account,
		Platform:   last.Platform,
		Code:       last.Code,
		Duration:   last.Duration,
		Count:      last.Count,
		Used:       last.Used,
		CreateTime: last.CreateTime,
	}, nil
}

func (o *VerifyCode) Incr(ctx context.Context, id string) error {
	objID, err := o.parseID(id)
	if err != nil {
		return err
	}
	return mongoutil.UpdateOne(ctx, o.coll, bson.M{"_id": objID}, bson.M{"$inc": bson.M{"count": 1}}, false)
}

func (o *VerifyCode) TryIncrementAttempt(ctx context.Context, id string, limit int) (bool, error) {
	objID, err := o.parseID(id)
	if err != nil {
		return false, err
	}
	result, err := o.coll.UpdateOne(ctx, bson.M{"_id": objID, "used": bson.M{"$ne": true}, "count": bson.M{"$lt": limit}}, bson.M{"$inc": bson.M{"count": 1}})
	if err != nil {
		return false, errs.Wrap(err)
	}
	return result.ModifiedCount == 1, nil
}

func (o *VerifyCode) TryAcceptCode(ctx context.Context, id string, code string, limit int, nonce string) (bool, error) {
	objID, err := o.parseID(id)
	if err != nil {
		return false, err
	}
	result, err := o.coll.UpdateOne(ctx, bson.M{"_id": objID, "code": code, "used": bson.M{"$ne": true}, "count": bson.M{"$lt": limit}}, bson.M{"$set": bson.M{"last_verified_nonce": nonce}})
	if err != nil {
		return false, errs.Wrap(err)
	}
	return result.MatchedCount == 1, nil
}

func (o *VerifyCode) Delete(ctx context.Context, id string) error {
	objID, err := o.parseID(id)
	if err != nil {
		return err
	}
	return mongoutil.DeleteOne(ctx, o.coll, bson.M{"_id": objID})
}

const maxVerifyCodeSendLimit = 1000

type verifyCodeSendReservation struct {
	ID        string    `bson:"id"`
	ExpiresAt time.Time `bson:"expires_at"`
}

type VerifyCodeSendLimiter struct{ coll *mongo.Collection }

func NewVerifyCodeSendLimiter(db *mongo.Database) (chat.VerifyCodeSendLimiterInterface, error) {
	coll := db.Collection("verify_code_send_limits")
	_, err := coll.Indexes().CreateOne(context.Background(), mongo.IndexModel{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0)})
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return &VerifyCodeSendLimiter{coll: coll}, nil
}

func verifyCodeAccountKey(account string) string {
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:])
}

func (o *VerifyCodeSendLimiter) Reserve(ctx context.Context, account string, now time.Time, window time.Duration, max int, reservationID string) (bool, error) {
	if account == "" || reservationID == "" || window <= 0 || max <= 0 || max > maxVerifyCodeSendLimit {
		return false, fmt.Errorf("invalid verification-code send limit parameters")
	}
	key := verifyCodeAccountKey(account)
	expiresAt := now.Add(window)
	active := bson.M{"$filter": bson.M{"input": bson.M{"$ifNull": bson.A{"$reservations", bson.A{}}}, "as": "entry", "cond": bson.M{"$gt": bson.A{"$$entry.expires_at", now}}}}
	filter := bson.M{"_id": key, "$expr": bson.M{"$lt": bson.A{bson.M{"$size": active}, max}}}
	pipeline := mongo.Pipeline{{{Key: "$set", Value: bson.M{
		"reservations": bson.M{"$concatArrays": bson.A{active, bson.A{bson.M{"id": reservationID, "expires_at": expiresAt}}}},
		"expires_at":   bson.M{"$max": bson.A{bson.M{"$ifNull": bson.A{"$expires_at", expiresAt}}, expiresAt}},
	}}}}
	for attempt := 0; attempt < 4; attempt++ {
		result, err := o.coll.UpdateOne(ctx, filter, pipeline)
		if err != nil {
			return false, errs.Wrap(err)
		}
		if result.MatchedCount == 1 {
			return true, nil
		}
		count, err := o.coll.CountDocuments(ctx, bson.M{"_id": key})
		if err != nil {
			return false, errs.Wrap(err)
		}
		if count != 0 {
			return false, nil
		}
		_, err = o.coll.InsertOne(ctx, bson.M{"_id": key, "reservations": bson.A{bson.M{"id": reservationID, "expires_at": expiresAt}}, "expires_at": expiresAt})
		if err == nil {
			return true, nil
		}
		if !mongo.IsDuplicateKeyError(err) {
			return false, errs.Wrap(err)
		}
	}
	return false, fmt.Errorf("verification-code reservation contention exceeded retries")
}

func (o *VerifyCodeSendLimiter) Release(ctx context.Context, account string, reservationID string) error {
	_, err := o.coll.UpdateOne(ctx, bson.M{"_id": verifyCodeAccountKey(account)}, bson.M{"$pull": bson.M{"reservations": bson.M{"id": reservationID}}})
	return errs.Wrap(err)
}
