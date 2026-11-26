package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"
)

var ErrAdminExists = errors.New("admin already exists")

// AuthStore 单管理员账号 + 会话。
type AuthStore interface {
	HasAdmin(ctx context.Context) (bool, error)
	CreateAdmin(ctx context.Context, username, password string) error
	VerifyAdmin(ctx context.Context, username, password string) (bool, error)
	// UpdateAdmin 修改用户名和密码，并吊销所有会话。
	UpdateAdmin(ctx context.Context, username, password string) error
	AdminUsername(ctx context.Context) (string, error)
	CreateSession(ctx context.Context, ttl time.Duration) (string, error)
	ValidSession(ctx context.Context, token string) (bool, error)
	DeleteSession(ctx context.Context, token string) error
}

func validateCredentials(username, password string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(password) < 6 {
		return "", fmt.Errorf("username required and password min 6 chars")
	}
	return username, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// 会话库只存 token 的哈希，库泄露也拿不到可用 token。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

const adminDocID = "admin"

type MongoAuth struct {
	Users    *mongo.Collection
	Sessions *mongo.Collection
}

type adminDoc struct {
	ID           string    `bson:"_id"`
	Username     string    `bson:"username"`
	PasswordHash string    `bson:"password_hash"`
	CreatedAt    time.Time `bson:"created_at"`
}

// EnsureIndexes 会话按 expires_at 自动过期。
func (m MongoAuth) EnsureIndexes(ctx context.Context) error {
	_, err := m.Sessions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	})
	return err
}

func (m MongoAuth) HasAdmin(ctx context.Context) (bool, error) {
	n, err := m.Users.CountDocuments(ctx, bson.M{"_id": adminDocID})
	return n > 0, err
}

func (m MongoAuth) CreateAdmin(ctx context.Context, username, password string) error {
	username, err := validateCredentials(username, password)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = m.Users.InsertOne(ctx, adminDoc{
		ID: adminDocID, Username: username, PasswordHash: string(hash), CreatedAt: time.Now(),
	})
	if mongo.IsDuplicateKeyError(err) {
		return ErrAdminExists
	}
	return err
}

func (m MongoAuth) VerifyAdmin(ctx context.Context, username, password string) (bool, error) {
	var doc adminDoc
	err := m.Users.FindOne(ctx, bson.M{"_id": adminDocID, "username": strings.TrimSpace(username)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bcrypt.CompareHashAndPassword([]byte(doc.PasswordHash), []byte(password)) == nil, nil
}

func (m MongoAuth) UpdateAdmin(ctx context.Context, username, password string) error {
	username, err := validateCredentials(username, password)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := m.Users.UpdateOne(ctx, bson.M{"_id": adminDocID}, bson.M{"$set": bson.M{
		"username": username, "password_hash": string(hash),
	}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return errors.New("admin not found")
	}
	_, err = m.Sessions.DeleteMany(ctx, bson.M{})
	return err
}

func (m MongoAuth) AdminUsername(ctx context.Context) (string, error) {
	var doc adminDoc
	if err := m.Users.FindOne(ctx, bson.M{"_id": adminDocID}).Decode(&doc); err != nil {
		return "", err
	}
	return doc.Username, nil
}

func (m MongoAuth) CreateSession(ctx context.Context, ttl time.Duration) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	_, err = m.Sessions.InsertOne(ctx, bson.M{"_id": hashToken(tok), "expires_at": time.Now().Add(ttl)})
	if err != nil {
		return "", err
	}
	return tok, nil
}

func (m MongoAuth) ValidSession(ctx context.Context, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, nil
	}
	n, err := m.Sessions.CountDocuments(ctx, bson.M{"_id": hashToken(token), "expires_at": bson.M{"$gt": time.Now()}})
	return n > 0, err
}

func (m MongoAuth) DeleteSession(ctx context.Context, token string) error {
	_, err := m.Sessions.DeleteOne(ctx, bson.M{"_id": hashToken(token)})
	return err
}
