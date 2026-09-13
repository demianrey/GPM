package server

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// UserStore es la fuente de verdad de qué UUIDs pueden autenticarse y con
// qué nombre (para logs/medición). La implementación de esta noche es un
// archivo JSON local (modo manual). El modo "panel" (sincronizar contra
// v2board, como hace v2node) implementará la MISMA interfaz -- el resto del
// servidor no necesita saber de dónde salen los usuarios.
type UserStore interface {
	// Lookup devuelve el nombre asociado al uuid y si existe.
	Lookup(uuid string) (name string, ok bool)
}

// FileUserStore es un UserStore respaldado por un archivo JSON en disco,
// con recarga en caliente (cada Lookup relee si el archivo cambió) para que
// `gpm adduser`/`gpm deluser` surtan efecto sin reiniciar el servidor.
type FileUserStore struct {
	path string

	mu      sync.RWMutex
	modTime int64
	users   map[string]string
}

func NewFileUserStore(path string) (*FileUserStore, error) {
	s := &FileUserStore{path: path}
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileUserStore) Lookup(uuid string) (string, bool) {
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	name, ok := s.users[uuid]
	return name, ok
}

func (s *FileUserStore) maybeReload() {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}
	mt := info.ModTime().UnixNano()
	s.mu.RLock()
	changed := mt != s.modTime
	s.mu.RUnlock()
	if !changed {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
}

func (s *FileUserStore) reloadLocked() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.users = map[string]string{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("leer %s: %w", s.path, err)
	}
	var users map[string]string
	if err := json.Unmarshal(data, &users); err != nil {
		return fmt.Errorf("parsear %s: %w", s.path, err)
	}
	if users == nil {
		users = map[string]string{}
	}
	s.users = users
	if info, err := os.Stat(s.path); err == nil {
		s.modTime = info.ModTime().UnixNano()
	}
	return nil
}

// AddUser / DelUser / ListUsers son helpers de archivo, usados por los
// subcomandos `gpm adduser`/`gpm deluser`/`gpm listusers` -- no dependen de
// un FileUserStore en memoria (pueden correr mientras el servidor sigue
// vivo en otro proceso, ya que FileUserStore recarga solo).

func LoadUsersFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var users map[string]string
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("parsear %s: %w", path, err)
	}
	if users == nil {
		users = map[string]string{}
	}
	return users, nil
}

func SaveUsersFile(path string, users map[string]string) error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
