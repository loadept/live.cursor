package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Client struct {
	ConnID string
	Conn   *websocket.Conn
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
	upgrader  *websocket.Upgrader
	mutex     sync.Mutex
	clients   map[*Client]bool
	events    chan *Event
	broadcast chan *Message
}

//go:embed template/index.tmpl
var index embed.FS
var tmpl = template.Must(template.ParseFS(index, "template/index.tmpl"))

func main() {
	log.SetFlags(0)
	mux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:         getEnv("ADDR", ":8080"),
		Handler:      mux,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	upgrader := &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == "https://live.loadept.com" || origin == "https://loadept.com"
			// return true
		},
	}
	wsServer := &Server{
		upgrader:  upgrader,
		clients:   make(map[*Client]bool),
		events:    make(chan *Event),
		broadcast: make(chan *Message),
	}
	defer func() {
		close(wsServer.events)
		close(wsServer.broadcast)
	}()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"title": "cursor live",
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
	if err := httpServer.Shutdown(shutCtx); err != nil {
		log.Fatal(err)
	}
}

func getEnv(key, def string) string {
	env, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	return env
}

func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	id := uuid.New().String()
	client := &Client{Conn: conn, ConnID: id}

	s.mutex.Lock()
	s.clients[client] = true
	s.mutex.Unlock()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			s.mutex.Lock()
			delete(s.clients, client)
			s.mutex.Unlock()

			client.Conn.Close()
			s.events <- &Event{Type: "disconnected", ConnID: client.ConnID}
			break
		}

		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			errMsg := []byte(`{"detail": "malformed data, connection closed"}`)
			client.Conn.WriteMessage(websocket.TextMessage, errMsg)

			s.mutex.Lock()
			delete(s.clients, client)
			s.mutex.Unlock()

			client.Conn.Close()
			s.events <- &Event{Type: "disconnected", ConnID: client.ConnID}
			break
		}
		msg.ConnID = client.ConnID

		s.broadcast <- &msg
	}
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

	for client := range s.clients {
		if client.ConnID == senderID {
			continue
		}

		if err := client.Conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			client.Conn.Close()
			delete(s.clients, client)
		}
	}
}
