// Command server exposes the department over HTTP + WebSocket and serves the 3D front-end.
//
//	GET  /api/state            snapshot: agents + presence, requests, tasks, pending approvals, recent events
//	POST /api/requests         {"text": "..."} submit a request to Max
//	GET  /api/agents/{id}      agent detail: definition, presence, current task, recent log, memories
//	POST /api/approvals/{id}   {"approved": true|false}
//	GET  /ws                   live event stream (JSON per message)
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"aidept/internal/agents"
	"aidept/internal/app"
	"aidept/internal/events"
	"aidept/internal/store"
)

type server struct{ a *app.App }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	a, err := app.New(ctx)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	a.Orch.Start(ctx)
	s := &server{a: a}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/requests", s.submit)
	mux.HandleFunc("GET /api/agents/{id}", s.agent)
	mux.HandleFunc("POST /api/approvals/{id}", s.approve)
	mux.HandleFunc("GET /ws", s.ws)
	dist := filepath.Join(a.Root, "web", "dist")
	mux.Handle("/", http.FileServer(http.Dir(dist)))

	addr := os.Getenv("AIDEPT_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("AI IT department listening on http://%s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type agentView struct {
	*agents.Def
	Presence events.Presence `json:"presence"`
}

func (s *server) agentViews() []agentView {
	out := make([]agentView, 0, len(agents.Order))
	for _, id := range agents.Order {
		out = append(out, agentView{Def: s.a.Defs[id], Presence: s.a.Bus.PresenceOf(id)})
	}
	return out
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	reqs, _ := s.a.Store.ListRequests(20)
	var tasks []*store.Task
	for _, rq := range reqs { // tasks of the recent requests, newest request first
		ts, _ := s.a.Store.ListTasks(rq.ID)
		tasks = append(tasks, ts...)
	}
	approvals, _ := s.a.Store.PendingApprovals()
	evs, _ := s.a.Store.ListEvents("", 150)
	writeJSON(w, 200, map[string]any{
		"agents": s.agentViews(), "requests": reqs, "tasks": tasks, "approvals": approvals, "events": evs,
	})
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeJSON(w, 400, map[string]string{"error": "text is required"})
		return
	}
	req, err := s.a.Orch.Submit(strings.TrimSpace(body.Text))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, req)
}

func (s *server) agent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	def := s.a.Defs[id]
	if def == nil {
		writeJSON(w, 404, map[string]string{"error": "unknown agent"})
		return
	}
	p := s.a.Bus.PresenceOf(id)
	var current *store.Task
	if p.TaskID > 0 {
		current, _ = s.a.Store.GetTask(p.TaskID)
	}
	evs, _ := s.a.Store.ListEvents(id, 60)
	mems, _ := s.a.Store.ListMemories(id, 50)
	memFile, _ := os.ReadFile(filepath.Join(s.a.Root, "memory", id+".md"))
	writeJSON(w, 200, map[string]any{
		"agent": agentView{Def: def, Presence: p}, "current_task": current, "events": evs,
		"memories": mems, "memory_file": string(memFile),
	})
}

func (s *server) approve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
		return
	}
	var body struct {
		Approved bool `json:"approved"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.a.Approvals.Resolve(id, body.Approved); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "approved": body.Approved})
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		o := r.Header.Get("Origin")
		return o == "" || strings.Contains(o, "localhost") || strings.Contains(o, "127.0.0.1")
	},
}

func (s *server) ws(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	sub := s.a.Bus.Subscribe()
	defer s.a.Bus.Unsubscribe(sub)

	done := make(chan struct{})
	go func() { // reader: detect close
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-done:
			return
		case e := <-sub:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(e); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
