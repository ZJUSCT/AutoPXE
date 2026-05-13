package node

import (
	"net"
	"strings"
	"sync"
)

type Store struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	macIndex map[string]string
}

func NewStore() *Store {
	return &Store{
		nodes:    make(map[string]*Node),
		macIndex: make(map[string]string),
	}
}

func canonMAC(s string) string {
	if hw, err := net.ParseMAC(s); err == nil {
		return strings.ToLower(hw.String())
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func (s *Store) GetByUUID(uuid string) *Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[uuid]
}

// FindByMACs returns the first existing node that matches any of the given
// MACs. Returns nil if no MAC is currently indexed.
func (s *Store) FindByMACs(macs []string) *Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range macs {
		if uuid, ok := s.macIndex[canonMAC(m)]; ok {
			return s.nodes[uuid]
		}
	}
	return nil
}

// GetOrCreate finds a node matching any of the given MACs, otherwise creates
// one indexed by all of them. The boot NIC convention is that macs[0] is
// preferred for new node UUIDs but every MAC is indexed so future lookups via
// any NIC find the same node.
func (s *Store) GetOrCreate(macs []string) (*Node, bool) {
	canon := make([]string, 0, len(macs))
	for _, m := range macs {
		c := canonMAC(m)
		if c != "" {
			canon = append(canon, c)
		}
	}
	if len(canon) == 0 {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range canon {
		if uuid, ok := s.macIndex[m]; ok {
			return s.nodes[uuid], false
		}
	}
	n := New(canon)
	s.nodes[n.UUID] = n
	for _, m := range canon {
		s.macIndex[m] = n.UUID
	}
	return n, true
}

func (s *Store) Each(fn func(*Node)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.nodes {
		fn(n)
	}
}
