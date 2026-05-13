package node

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type State string

const (
	StateNew       State = "new"
	StateDeploying State = "deploying"
	StateRunImage  State = "run_image"
	StateDone      State = "done"
	StateFailed    State = "failed"
)

type Node struct {
	UUID       string
	MACs       []string
	AgentToken string
	Hostname   string

	mu            sync.Mutex
	state         State
	callbackURL   string
	commandID     string
	lastHeartbeat time.Time
	driverStarted bool
}

type Snapshot struct {
	State         State
	CallbackURL   string
	CommandID     string
	LastHeartbeat time.Time
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("rand: %w", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

func newAgentToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("rand: %w", err))
	}
	return hex.EncodeToString(b)
}

func New(macs []string, hostname string) *Node {
	return &Node{
		UUID:       newUUID(),
		MACs:       macs,
		AgentToken: newAgentToken(),
		Hostname:   hostname,
		state:      StateNew,
	}
}

func (n *Node) State() State {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state
}

func (n *Node) Get() Snapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return Snapshot{
		State:         n.state,
		CallbackURL:   n.callbackURL,
		CommandID:     n.commandID,
		LastHeartbeat: n.lastHeartbeat,
	}
}

// RecordHeartbeat updates the callback URL + heartbeat timestamp and reports
// whether the caller should start a deploy driver goroutine. It returns true
// at most once per node lifetime — subsequent calls observe driverStarted.
func (n *Node) RecordHeartbeat(callbackURL string) (startDriver bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.callbackURL = callbackURL
	n.lastHeartbeat = time.Now()
	if !n.driverStarted && n.state == StateNew {
		n.driverStarted = true
		return true
	}
	return false
}

func (n *Node) SetCommandID(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commandID = id
}

// Transition is a CAS-style state move; returns false if current state != from.
func (n *Node) Transition(from, to State) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != from {
		return false
	}
	n.state = to
	return true
}

func (n *Node) ForceState(s State) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.state = s
}
