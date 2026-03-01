package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/webhook"
)

const (
	apiKey    = "devkey"
	apiSecret = "secret"
)

var provider = auth.NewSimpleKeyProvider(apiKey, apiSecret)

// --- DEMO in-memory state ---
type CallSession struct {
	Room        string
	AgentPID    int
	StartedAt   time.Time
	LastEventAt time.Time
}

var (
	mu       sync.Mutex
	sessions = map[string]*CallSession{} // key: room name
)

// --- Agent event coming back from python worker ---
type AgentEvent struct {
	Room       string `json:"room"`
	Type       string `json:"type"` // transcript|intent|state
	Transcript string `json:"transcript,omitempty"`
	Intent     string `json:"intent,omitempty"`
}

// --- Command to agent (demo) ---
type AgentCommand struct {
	Room   string `json:"room"`
	Action string `json:"action"` // say|interrupt
	Text   string `json:"text,omitempty"`
}

func main() {
	http.HandleFunc("/livekit/webhook", livekitWebhookHandler)
	http.HandleFunc("/agent/event", agentEventHandler)     // agent -> orchestration
	http.HandleFunc("/agent/command", agentCommandHandler) // demo: you -> orchestration -> agent

	log.Println("valjean-orchestration listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func livekitWebhookHandler(w http.ResponseWriter, r *http.Request) {
	// Single call handles body reading + signature verification + unmarshalling
	evt, err := webhook.ReceiveWebhookEvent(r, provider)
	if err != nil {
		log.Printf("[WEBHOOK] verification failed: %v", err)
		http.Error(w, "invalid signature", 401)
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

	log.Printf("[WEBHOOK] event=%s room=%s participant=%s\n", evt.Event, room, participant)

	switch evt.Event {
	case webhook.EventRoomStarted:
		ensureSession(room)

	case webhook.EventParticipantJoined:
		ensureSession(room)
		if participant != "" && participant != "agent" {
			startAgentIfNeeded(room)
		}

	case webhook.EventParticipantLeft:
		stopAgentIfRunning(room)

	case webhook.EventParticipantConnectionAborted:
		stopAgentIfRunning(room)

	case webhook.EventRoomFinished:
		stopAgentIfRunning(room)
		removeSession(room)

	case webhook.EventTrackPublished, webhook.EventTrackUnpublished:
		touchSession(room)

	case webhook.EventEgressStarted, webhook.EventEgressUpdated, webhook.EventEgressEnded:
		touchSession(room)

	case webhook.EventIngressStarted, webhook.EventIngressEnded:
		touchSession(room)

	default:
		ensureSession(room)
		touchSession(room)
	}

	w.WriteHeader(200)
}

func agentEventHandler(w http.ResponseWriter, r *http.Request) {
	var ev AgentEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad json", 400)
		return
	}

	log.Printf("[AGENT->ORCH] room=%s type=%s transcript=%q intent=%q\n",
		ev.Room, ev.Type, ev.Transcript, ev.Intent,
	)

	// Demo workflow: if the transcript contains "cancel" -> interrupt + say a line
	if ev.Type == "transcript" && containsCancel(ev.Transcript) {
		_ = sendCommandToAgent(AgentCommand{
			Room:   ev.Room,
			Action: "interrupt",
		})
		_ = sendCommandToAgent(AgentCommand{
			Room:   ev.Room,
			Action: "say",
			Text:   "Okay, cancelling now.",
		})
	}

	w.WriteHeader(200)
}

func agentCommandHandler(w http.ResponseWriter, r *http.Request) {
	// This endpoint is just for demo manual testing (curl)
	var cmd AgentCommand
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if err := sendCommandToAgent(cmd); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(200)
}

// ------------------- helpers -------------------

func ensureSession(room string) {
	if room == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := sessions[room]; !ok {
		sessions[room] = &CallSession{
			Room:        room,
			StartedAt:   time.Now(),
			LastEventAt: time.Now(),
		}
	}
}

func touchSession(room string) {
	mu.Lock()
	defer mu.Unlock()
	if s, ok := sessions[room]; ok {
		s.LastEventAt = time.Now()
	}
}

func removeSession(room string) {
	mu.Lock()
	defer mu.Unlock()
	delete(sessions, room)
}

func startAgentIfNeeded(room string) {
	if room == "" {
		return
	}

	mu.Lock()
	s := sessions[room]
	alreadyRunning := s != nil && s.AgentPID != 0
	mu.Unlock()

	if alreadyRunning {
		return
	}

	// Demo: spawn python agent process
	// Pass room + orchestration callback URL
	cmd := exec.Command(
		"python",
		"agent.py",
		"--room", room,
		"--orchestrator", "http://localhost:8080",
	)

	if err := cmd.Start(); err != nil {
		log.Println("failed to start agent:", err)
		return
	}

	mu.Lock()
	if sessions[room] != nil {
		sessions[room].AgentPID = cmd.Process.Pid
		sessions[room].LastEventAt = time.Now()
	}
	mu.Unlock()

	log.Printf("[ORCH] started agent room=%s pid=%d\n", room, cmd.Process.Pid)

	// Do not Wait() in demo main thread; let it run.
	go func() {
		_ = cmd.Wait()
		log.Printf("[ORCH] agent exited room=%s pid=%d\n", room, cmd.Process.Pid)
	}()
}

func stopAgentIfRunning(room string) {
	mu.Lock()
	s := sessions[room]
	pid := 0
	if s != nil {
		pid = s.AgentPID
		s.AgentPID = 0
	}
	mu.Unlock()

	if pid == 0 {
		return
	}

	// Demo: best-effort kill by PID (simple)
	log.Printf("[ORCH] stopping agent room=%s pid=%d\n", room, pid)
	// On mac/linux:
	_ = exec.Command("kill", "-TERM", fmt.Sprintf("%d", pid)).Run()
}

func sendCommandToAgent(cmd AgentCommand) error {
	// Demo assumes the agent exposes local control endpoint, e.g. http://localhost:7010/cmd
	// In production you’d have stable service discovery / per-room routing.
	url := "http://localhost:7010/cmd"

	b, _ := json.Marshal(cmd)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent cmd failed: %s", string(body))
	}
	return nil
}

func containsCancel(s string) bool {
	// tiny demo helper
	return len(s) > 0 && (bytes.Contains(bytes.ToLower([]byte(s)), []byte("cancel")))
}
