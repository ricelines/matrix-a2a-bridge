package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"maunium.net/go/mautrix/crypto/cryptohelper"
)

const (
	cryptoDBFilename  = "crypto.db"
	pickleKeyFilename = "crypto.db.pickle_key"
	pickleKeySize     = 32
)

func (b *Bridge) initializeCrypto(ctx context.Context) error {
	if b.crypto != nil {
		return nil
	}

	cryptoDBPath := filepath.Join(filepath.Dir(b.config.StatePath), cryptoDBFilename)
	pickleKey, err := loadOrCreatePickleKey(filepath.Join(filepath.Dir(cryptoDBPath), pickleKeyFilename))
	if err != nil {
		return fmt.Errorf("load matrix pickle key: %w", err)
	}

	helper, err := cryptohelper.NewCryptoHelper(b.client, pickleKey, cryptoDBPath)
	if err != nil {
		return fmt.Errorf("create matrix crypto helper: %w", err)
	}
	if err := helper.Init(ctx); err != nil {
		_ = helper.Close()
		return fmt.Errorf("initialize matrix crypto helper: %w", err)
	}

	b.client.Crypto = helper
	b.crypto = helper
	return nil
}

func loadOrCreatePickleKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return parsePickleKey(data, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read pickle key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create pickle key dir: %w", err)
	}

	key := make([]byte, pickleKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate pickle key: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create pickle key temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("chmod pickle key temp file: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write pickle key temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close pickle key temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, fmt.Errorf("persist pickle key: %w", err)
	}

	return key, nil
}

func parsePickleKey(data []byte, path string) ([]byte, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("pickle key file is empty: %s", path)
	}

	key, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode pickle key: %w", err)
	}
	if len(key) != pickleKeySize {
		return nil, fmt.Errorf("pickle key at %s has %d bytes, want %d", path, len(key), pickleKeySize)
	}
	return key, nil
}

type syncEventCollector struct {
	mu      sync.Mutex
	batches map[string]roomUpdateBatch
	order   []string
}

func newSyncEventCollector() *syncEventCollector {
	return &syncEventCollector{batches: make(map[string]roomUpdateBatch)}
}

func (c *syncEventCollector) add(batch roomUpdateBatch) {
	if c == nil || batch.RoomID == "" || len(batch.Updates) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	current, exists := c.batches[batch.RoomID]
	if !exists {
		c.order = append(c.order, batch.RoomID)
		c.batches[batch.RoomID] = batch
		return
	}

	for _, nextUpdate := range batch.Updates {
		merged := false
		for idx := range current.Updates {
			if current.Updates[idx].RoomSection != nextUpdate.RoomSection {
				continue
			}
			current.Updates[idx].State = append(current.Updates[idx].State, nextUpdate.State...)
			current.Updates[idx].Timeline = append(current.Updates[idx].Timeline, nextUpdate.Timeline...)
			merged = true
			break
		}
		if !merged {
			current.Updates = append(current.Updates, nextUpdate)
		}
	}

	c.batches[batch.RoomID] = current
}

func (c *syncEventCollector) flush() []roomUpdateBatch {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	batches := make([]roomUpdateBatch, 0, len(c.order))
	for _, roomID := range c.order {
		if batch, ok := c.batches[roomID]; ok {
			batches = append(batches, batch)
		}
	}
	return batches
}

func (b *Bridge) beginEventCollection() *syncEventCollector {
	collector := newSyncEventCollector()
	b.collectorMu.Lock()
	b.collector = collector
	b.collectorMu.Unlock()
	return collector
}

func (b *Bridge) finishEventCollection(collector *syncEventCollector) []roomUpdateBatch {
	b.collectorMu.Lock()
	if b.collector == collector {
		b.collector = nil
	}
	b.collectorMu.Unlock()
	return collector.flush()
}

func (b *Bridge) currentCollector() *syncEventCollector {
	b.collectorMu.Lock()
	defer b.collectorMu.Unlock()
	return b.collector
}
