package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/webhook"
)

// ── Config ────────────────────────────────────────────────────────────────────

const (
	livekitHost   = "ws://localhost:7880"
	livekitAPIKey = "devkey"
	livekitSecret = "secret"
	orchAddr      = ":8080"
	agentScript   = "../agent/agent.py"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type AgentState string

const (
	StateStarting  AgentState = "starting"
	StateListening AgentState = "listening"
	StateSpeaking  AgentState = "speaking"
	StateIdle      AgentState = "idle"
	StateStopped   AgentState = "stopped"
)

type Session struct {
	RoomID    string
	AgentPID  int
	State     AgentState
	StartedAt time.Time
	UpdatedAt time.Time
}

// AgentEvent is posted by the Python agent → orchestrator
type AgentEvent struct {
	Room       string  `json:"room"`
	Type       string  `json:"type"`       // state_change | transcript | intent
	State      string  `json:"state"`      // listening | speaking | idle
	Transcript string  `json:"transcript"` // filled on transcript events
	Intent     string  `json:"intent"`     // filled on intent events
	Confidence float64 `json:"confidence"` // STT confidence
}

// AgentCommand is sent orchestrator → agent
type AgentCommand struct {
	Room   string `json:"room"`
	Action string `json:"action"` // say | interrupt | stop | transfer
	Text   string `json:"text,omitempty"`
	Target string `json:"target,omitempty"` // for transfer
}

// ── Orchestrator ──────────────────────────────────────────────────────────────

type Orchestrator struct {
	mu       sync.RWMutex
	sessions map[string]*Session // key: room ID
	provider auth.KeyProvider
}

func NewOrchestrator() *Orchestrator {
	return &Orchestrator{
		sessions: make(map[string]*Session),
		provider: auth.NewSimpleKeyProvider(livekitAPIKey, livekitSecret),
	}
}

func (o *Orchestrator) Run() {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", o.handleWebhook)        // LiveKit → orchestrator
	mux.HandleFunc("/agent/event", o.handleAgentEvent) // agent → orchestrator
	mux.HandleFunc("/health", o.handleHealth)

	log.Printf("[orch] listening on %s", orchAddr)
	if err := http.ListenAndServe(orchAddr, mux); err != nil {
		log.Fatal(err)
	}
}

// ── Webhook handler ───────────────────────────────────────────────────────────

func (o *Orchestrator) handleWebhook(w http.ResponseWriter, r *http.Request) {
	evt, err := webhook.ReceiveWebhookEvent(r, o.provider)
	if err != nil {
		log.Printf("[orch] webhook auth failed: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	room := ""
	if evt.Room != nil {
		room = evt.Room.Name
	}
	participant := ""
	if evt.Participant != nil {
		participant = evt.Participant.Identity
	}

	log.Printf("[orch] webhook event=%s room=%s participant=%s", evt.Event, room, participant)

	switch evt.Event {

	case webhook.EventRoomStarted:
		o.ensureSession(room)

	case webhook.EventParticipantJoined:
		// Only react to real callers joining, not our own agent
		if participant != "" && participant != "valjean-agent" {
			o.ensureSession(room)
			go o.startAgent(room)
		}

	case webhook.EventParticipantLeft:
		// If the caller left, stop the agent
		if participant != "" && participant != "valjean-agent" {
			go o.stopAgent(room)
		}

	case webhook.EventRoomFinished:
		go o.stopAgent(room)
		o.removeSession(room)

	case webhook.EventTrackPublished:
		o.touchSession(room)

	}

	w.WriteHeader(http.StatusOK)
}

// ── Agent event handler ───────────────────────────────────────────────────────

func (o *Orchestrator) handleAgentEvent(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()

	var ev AgentEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	log.Printf("[orch] agent event room=%s type=%s state=%s transcript=%q",
		ev.Room, ev.Type, ev.State, ev.Transcript)

	switch ev.Type {

	case "state_change":
		o.updateSessionState(ev.Room, AgentState(ev.State))

	case "transcript":
		// Demo workflow: detect intent and respond
		o.handleTranscript(ev)

	case "intent":
		log.Printf("[orch] intent detected room=%s intent=%s", ev.Room, ev.Intent)
	}

	w.WriteHeader(http.StatusOK)
}

// handleTranscript runs simple intent detection and sends commands back to agent
func (o *Orchestrator) handleTranscript(ev AgentEvent) {
	text := ev.Transcript

	switch {
	case contains(text, "cancel", "stop", "never mind"):
		log.Printf("[orch] cancel intent detected room=%s", ev.Room)
		_ = o.sendCommand(AgentCommand{
			Room:   ev.Room,
			Action: "interrupt",
		})
		_ = o.sendCommand(AgentCommand{
			Room:   ev.Room,
			Action: "say",
			Text:   "Sure, I've cancelled that for you.",
		})

	case contains(text, "transfer", "speak to human", "real person"):
		log.Printf("[orch] transfer intent detected room=%s", ev.Room)
		_ = o.sendCommand(AgentCommand{
			Room:   ev.Room,
			Action: "say",
			Text:   "Let me transfer you now. Please hold.",
		})
		_ = o.sendCommand(AgentCommand{
			Room:   ev.Room,
			Action: "transfer",
			Target: "sip:support@example.com",
		})

	case contains(text, "goodbye", "bye", "hang up"):
		log.Printf("[orch] hangup intent detected room=%s", ev.Room)
		_ = o.sendCommand(AgentCommand{
			Room:   ev.Room,
			Action: "say",
			Text:   "Goodbye! Have a great day.",
		})
	}
}

func (o *Orchestrator) handleHealth(w http.ResponseWriter, r *http.Request) {
	o.mu.RLock()
	count := len(o.sessions)
	o.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"ok","active_sessions":%d}`, count)
}

// ── Agent lifecycle ───────────────────────────────────────────────────────────

func (o *Orchestrator) startAgent(room string) {
	o.mu.RLock()
	s := o.sessions[room]
	if s != nil && s.AgentPID != 0 {
		o.mu.RUnlock()
		log.Printf("[orch] agent already running for room=%s", room)
		return
	}
	o.mu.RUnlock()

	log.Printf("[orch] starting agent for room=%s", room)

	// Generate a LiveKit token for the agent participant
	token, err := generateAgentToken(room)
	if err != nil {
		log.Printf("[orch] failed to generate token: %v", err)
		return
	}

	python := pythonBin()
	cmd := exec.Command(python, agentScript,
		"--room", room,
		"--token", token,
		"--livekit-url", livekitHost,
		"--orchestrator", "http://localhost"+orchAddr,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Printf("[orch] failed to start agent: %v", err)
		return
	}

	o.mu.Lock()
	if s, ok := o.sessions[room]; ok {
		s.AgentPID = cmd.Process.Pid
		s.State = StateStarting
		s.UpdatedAt = time.Now()
	}
	o.mu.Unlock()

	log.Printf("[orch] agent started pid=%d room=%s", cmd.Process.Pid, room)

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("[orch] agent exited with error pid=%d room=%s err=%v",
				cmd.Process.Pid, room, err)
		} else {
			log.Printf("[orch] agent exited cleanly pid=%d room=%s", cmd.Process.Pid, room)
		}
		// Clear PID so we can restart if needed
		o.mu.Lock()
		if s, ok := o.sessions[room]; ok {
			s.AgentPID = 0
			s.State = StateStopped
		}
		o.mu.Unlock()
	}()
}

func (o *Orchestrator) stopAgent(room string) {
	o.mu.Lock()
	s, ok := o.sessions[room]
	pid := 0
	if ok && s != nil {
		pid = s.AgentPID
		s.AgentPID = 0
		s.State = StateStopped
	}
	o.mu.Unlock()

	if pid == 0 {
		return
	}

	log.Printf("[orch] stopping agent pid=%d room=%s", pid, room)

	// Ask agent to stop gracefully first
	_ = o.sendCommand(AgentCommand{Room: room, Action: "stop"})
	time.Sleep(2 * time.Second)

	// Force kill if still running
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(os.Interrupt)
	}
}

// sendCommand POSTs a command to the agent's local HTTP server
func (o *Orchestrator) sendCommand(cmd AgentCommand) error {
	// Agent exposes :7010/cmd — in production use service discovery
	url := "http://localhost:7010/cmd"
	return postJSON(url, cmd)
}

// ── Session helpers ───────────────────────────────────────────────────────────

func (o *Orchestrator) ensureSession(room string) {
	if room == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.sessions[room]; !ok {
		o.sessions[room] = &Session{
			RoomID:    room,
			State:     StateIdle,
			StartedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		log.Printf("[orch] session created room=%s", room)
	}
}

func (o *Orchestrator) touchSession(room string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s, ok := o.sessions[room]; ok {
		s.UpdatedAt = time.Now()
	}
}

func (o *Orchestrator) updateSessionState(room string, state AgentState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s, ok := o.sessions[room]; ok {
		s.State = state
		s.UpdatedAt = time.Now()
		log.Printf("[orch] session state room=%s state=%s", room, state)
	}
}

func (o *Orchestrator) removeSession(room string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.sessions, room)
	log.Printf("[orch] session removed room=%s", room)
}

// ── Token generation ──────────────────────────────────────────────────────────

func generateAgentToken(room string) (string, error) {
	at := auth.NewAccessToken(livekitAPIKey, livekitSecret)
	grant := &auth.VideoGrant{
		RoomJoin:     true,
		Room:         room,
		CanPublish:   func() *bool { b := true; return &b }(),
		CanSubscribe: func() *bool { b := true; return &b }(),
	}
	at.SetVideoGrant(grant).
		SetIdentity("valjean-agent").
		SetName("Valjean Agent").
		SetValidFor(2 * time.Hour)

	return at.ToJWT()
}

// ── Utilities ─────────────────────────────────────────────────────────────────

func contains(s string, words ...string) bool {
	lower := []byte(s)
	for i, c := range lower {
		if c >= 'A' && c <= 'Z' {
			lower[i] = c + 32
		}
	}
	for _, w := range words {
		if indexBytes(lower, []byte(w)) >= 0 {
			return true
		}
	}
	return false
}

func indexBytes(s, sub []byte) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if string(s[i:i+len(sub)]) == string(sub) {
			return i
		}
	}
	return -1
}

func pythonBin() string {
	if _, err := exec.LookPath("python3"); err == nil {
		return "python3"
	}
	return "python"
}

func postJSON(url string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json",
		newBytesReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

type bytesReader struct {
	b []byte
	i int
}

func newBytesReader(b []byte) *bytesReader { return &bytesReader{b: b} }
func (r *bytesReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	o := NewOrchestrator()
	o.Run()
}
