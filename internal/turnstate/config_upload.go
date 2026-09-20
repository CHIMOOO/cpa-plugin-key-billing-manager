package turnstate

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"time"

	"cpa-key-billing/internal/messages"
)

const (
	MaxProxyPoolEntries = 20000
	MaxConfigBytes      = 16 << 20
	ConfigChunkBytes    = 12 << 10
	configUploadTTL     = 15 * time.Minute
	maxConfigUploads    = 2
)

type configUpload struct {
	size     int
	data     []byte
	revision uint64
	expires  time.Time
}

// ConfigUploadStatus contains no configuration or proxy credentials.
type ConfigUploadStatus struct {
	ID         string `json:"id"`
	Received   int    `json:"received"`
	ChunkBytes int    `json:"chunk_bytes"`
}

// BeginConfigUpload reserves a bounded in-memory staging area. No active or
// persisted setting changes before CommitConfigUpload validates all bytes.
func (m *Manager) BeginConfigUpload(size int) (ConfigUploadStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneConfigUploadsLocked()
	if size < 2 || size > MaxConfigBytes {
		return ConfigUploadStatus{}, messages.Errorf("Turn-state settings must not exceed 16 MiB")
	}
	if len(m.uploads) >= maxConfigUploads {
		return ConfigUploadStatus{}, messages.Errorf("Too many settings uploads are active; finish or cancel another upload")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ConfigUploadStatus{}, messages.Errorf("Cannot create a settings upload")
	}
	id := hex.EncodeToString(random[:])
	if m.uploads == nil {
		m.uploads = map[string]*configUpload{}
	}
	m.uploads[id] = &configUpload{size: size, revision: m.configRevision, expires: m.now().Add(configUploadTTL)}
	return ConfigUploadStatus{ID: id, ChunkBytes: ConfigChunkBytes}, nil
}

func (m *Manager) pruneConfigUploadsLocked() {
	now := m.now()
	for id, upload := range m.uploads {
		if !now.Before(upload.expires) {
			delete(m.uploads, id)
		}
	}
}

// AppendConfigUpload accepts only sequential chunks, allowing exact retries.
// Data is base64 in the management JSON so UTF-8 boundaries and escaping cannot
// inflate a chunk unpredictably or change the original configuration bytes.
func (m *Manager) AppendConfigUpload(id string, offset int, data []byte) (ConfigUploadStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneConfigUploadsLocked()
	upload := m.uploads[id]
	if upload == nil {
		return ConfigUploadStatus{}, messages.Errorf("The settings upload expired or does not exist; save again")
	}
	if len(data) == 0 || len(data) > ConfigChunkBytes || offset < 0 || offset > len(upload.data) || len(data) > upload.size-offset {
		return ConfigUploadStatus{}, messages.Errorf("Invalid settings upload chunk")
	}
	if offset < len(upload.data) {
		if offset+len(data) > len(upload.data) || !bytes.Equal(upload.data[offset:offset+len(data)], data) {
			return ConfigUploadStatus{}, messages.Errorf("Invalid settings upload chunk")
		}
	} else {
		upload.data = append(upload.data, data...)
	}
	upload.expires = m.now().Add(configUploadTTL)
	return ConfigUploadStatus{ID: id, Received: len(upload.data), ChunkBytes: ConfigChunkBytes}, nil
}

func (m *Manager) CommitConfigUpload(id string) error {
	// Match Update's ordering so a probe cannot write across a config commit.
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	m.writerMu.Lock()
	defer m.writerMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneConfigUploadsLocked()
	upload := m.uploads[id]
	if upload == nil {
		return messages.Errorf("The settings upload expired or does not exist; save again")
	}
	if upload.revision != m.configRevision {
		return messages.Errorf("Settings changed during upload; refresh and save again")
	}
	if len(upload.data) != upload.size {
		return messages.Errorf("The settings upload is incomplete; save again")
	}
	if err := m.updateLocked(upload.data); err != nil {
		return err
	}
	delete(m.uploads, id)
	return nil
}

func (m *Manager) CancelConfigUpload(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneConfigUploadsLocked()
	delete(m.uploads, id)
}
