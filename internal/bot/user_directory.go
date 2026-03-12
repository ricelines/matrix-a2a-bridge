package bot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

const userBucketEventPrefix = "com.ricelines.onboarding.users"

type userRecord struct {
	ContextID   string     `json:"context_id,omitempty"`
	OnboardedAt *time.Time `json:"onboarded_at,omitempty"`
}

func (r userRecord) Onboarded() bool {
	return r.OnboardedAt != nil
}

type userBucket struct {
	Users map[string]userRecord `json:"users,omitempty"`
}

type userDirectory struct {
	client *mautrix.Client
	mu     sync.Mutex
	cache  map[string]userBucket
}

func newUserDirectory(client *mautrix.Client) *userDirectory {
	return &userDirectory{
		client: client,
		cache:  make(map[string]userBucket),
	}
}

func (d *userDirectory) Load(ctx context.Context, userID id.UserID) (userRecord, error) {
	eventType := bucketEventType(userID)

	d.mu.Lock()
	bucket, err := d.loadBucketLocked(ctx, eventType)
	d.mu.Unlock()
	if err != nil {
		return userRecord{}, err
	}

	record, ok := bucket.Users[userID.String()]
	if !ok {
		return userRecord{}, nil
	}
	return record, nil
}

func (d *userDirectory) Save(ctx context.Context, userID id.UserID, record userRecord) error {
	eventType := bucketEventType(userID)

	d.mu.Lock()
	defer d.mu.Unlock()

	bucket, err := d.loadBucketLocked(ctx, eventType)
	if err != nil {
		return err
	}
	if bucket.Users == nil {
		bucket.Users = make(map[string]userRecord)
	}
	bucket.Users[userID.String()] = record

	if err := d.client.SetAccountData(ctx, eventType, bucket); err != nil {
		return fmt.Errorf("set account data %s: %w", eventType, err)
	}

	d.cache[eventType] = cloneBucket(bucket)
	return nil
}

func (d *userDirectory) loadBucketLocked(ctx context.Context, eventType string) (userBucket, error) {
	if bucket, ok := d.cache[eventType]; ok {
		return cloneBucket(bucket), nil
	}

	var bucket userBucket
	if err := d.client.GetAccountData(ctx, eventType, &bucket); err != nil {
		if !errors.Is(err, mautrix.MNotFound) {
			return userBucket{}, fmt.Errorf("get account data %s: %w", eventType, err)
		}
		bucket = userBucket{}
	}
	if bucket.Users == nil {
		bucket.Users = make(map[string]userRecord)
	}

	d.cache[eventType] = cloneBucket(bucket)
	return cloneBucket(bucket), nil
}

func cloneBucket(bucket userBucket) userBucket {
	cloned := userBucket{Users: make(map[string]userRecord, len(bucket.Users))}
	for key, value := range bucket.Users {
		cloned.Users[key] = value
	}
	return cloned
}

func bucketEventType(userID id.UserID) string {
	sum := sha256.Sum256([]byte(userID))
	return fmt.Sprintf("%s.%02x", userBucketEventPrefix, sum[0])
}
