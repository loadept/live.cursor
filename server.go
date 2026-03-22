package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
)

type Client struct {
	Addr   string
	ConnID string
	Conn   *ws.Conn
}

type Event struct {
	Type   string `json:"type"`
	ConnID string `json:"conn_id"`
}

type Message struct {
	ConnID  string `json:"conn_id"`
	Payload struct {
		Label     string `json:"label"`
		Timestamp string `json:"timestamp"`
		Color     string `json:"color"`
		X         int    `json:"x"`
		Y         int    `json:"y"`
	} `json:"payload"`
}

type Server struct {
	upgrader  *ws.Upgrader
	mutex     sync.Mutex
	wg        sync.WaitGroup
	clients   map[*Client]bool
	events    chan *Event
	broadcast chan *Message
}

//go:embed web/template/* web/static/*
var static embed.FS
var tmpl = template.Must(template.ParseFS(static, "web/template/index.tmpl"))

func main() {
	log.SetFlags(0)
	mux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:         getEnv("ADDR", ":8080"),
		Handler:      mux,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	upgrader := &ws.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   512,
		WriteBufferSize:  512,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == getEnv("ORIGIN", "http://localhost:8080")
		},
	}
	wsServer := &Server{
		upgrader:  upgrader,
		clients:   make(map[*Client]bool),
		events:    make(chan *Event),
		broadcast: make(chan *Message),
	}
	defer func() {
		wsServer.wg.Wait()
		close(wsServer.events)
		close(wsServer.broadcast)
	}()

	sub, err := fs.Sub(static, "web/static")
	fatalIfErr(err)

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"title": "live.cursor",
		}
		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /ws", wsServer.wsHandler)
	go wsServer.msgsHandler()

	go func() {
		log.Println("server listen on", httpServer.Addr)
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	log.Println("shutdown server")
	fatalIfErr(httpServer.Shutdown(shutCtx))
}

func getEnv(key, def string) string {
	env, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	return env
}

func fatalIfErr(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

var (
	logMu sync.Mutex
	enc   = json.NewEncoder(os.Stdout)
)

func writeLog(le *logEntry) {
	logMu.Lock()
	defer logMu.Unlock()
	enc.Encode(le)
}

type logEntry struct {
	Timestamp string `json:"timestamp,omitempty"`
	Addr      string `json:"addr,omitempty"`
	ConnID    string `json:"conn_id,omitempty"`
	Event     string `json:"event,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	s.wg.Add(1)
	defer s.wg.Done()

	ip := r.Header.Get("CF-Connecting-IP")
	if ip == "" {
		ip = r.RemoteAddr
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	id := uuid.New().String()
	client := &Client{Addr: ip, ConnID: id, Conn: conn}

	writeLog(&logEntry{
		Timestamp: time.Now().Format(time.RFC3339),
		Addr:      client.Addr,
		ConnID:    client.ConnID,
		Event:     "a user has connected",
	})

	s.mutex.Lock()
	s.clients[client] = true
	s.mutex.Unlock()

	var le *logEntry
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if ws.IsCloseError(err, ws.CloseNormalClosure, ws.CloseGoingAway) {
				le = &logEntry{Event: "user disconnected cleanly"}
			} else {
				le = &logEntry{Error: fmt.Sprintf("message reading failed: %v", err)}
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			le = &logEntry{Error: fmt.Sprintf("message decoding failed: %v", err)}
			client.Conn.WriteMessage(ws.TextMessage, []byte(`{"detail": "malformed data, connection closed"}`))
			break
		}

		msg.ConnID = client.ConnID
		s.broadcast <- &msg
	}

	le.Timestamp = time.Now().Format(time.RFC3339)
	le.Addr = client.Addr
	le.ConnID = client.ConnID
	writeLog(le)

	s.mutex.Lock()
	delete(s.clients, client)
	s.mutex.Unlock()

	s.events <- &Event{Type: "disconnected", ConnID: client.ConnID}
}

func (s *Server) msgsHandler() {
	for {
		select {
		case message, ok := <-s.broadcast:
			if !ok {
				return
			}
			s.broadcastAll(message.ConnID, message)
		case event, ok := <-s.events:
			if !ok {
				return
			}
			s.broadcastAll(event.ConnID, event)
		}
	}
}

func (s *Server) broadcastAll(senderID string, data any) {
	raw, _ := json.Marshal(data)
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var toRemove []*Client
	for client := range s.clients {
		if client.ConnID == senderID {
			continue
		}

		if err := client.Conn.WriteMessage(ws.TextMessage, raw); err != nil {
			writeLog(&logEntry{
				Timestamp: time.Now().Format(time.RFC3339),
				Addr:      client.Addr,
				ConnID:    client.ConnID,
				Error:     fmt.Sprintf("write message failed: %v", err),
			})

			client.Conn.Close()
			toRemove = append(toRemove, client)
		}
	}

	for _, client := range toRemove {
		delete(s.clients, client)
	}
}
