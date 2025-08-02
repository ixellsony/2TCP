package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Constantes pour le serveur
const (
	DefaultServerPort = 8080
	CleanupTimeout    = 30 * time.Second
	StatsInterval     = 30 * time.Second
)

// --- DEBUT DU CODE COMMUN ---

type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type ConnectionData struct {
	ID string `json:"id"`
}

type HelloData struct {
	Port int `json:"port"`
}

type Logger struct {
	*log.Logger
	level string
}

func NewLogger(level string) *Logger {
	flags := log.LstdFlags
	if level == "debug" {
		flags |= log.Lshortfile
	}
	return &Logger{
		Logger: log.New(os.Stdout, "", flags),
		level:  level,
	}
}

func (l *Logger) log(prefix, msg string, keysAndValues ...interface{}) {
	var kvs string
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			if kvs != "" {
				kvs += " "
			}
			kvs += fmt.Sprintf("%v=%v", keysAndValues[i], keysAndValues[i+1])
		}
	}
	l.Printf("[%s] %s %s", prefix, msg, kvs)
}

func (l *Logger) Debug(msg string, keysAndValues ...interface{}) {
	if l.level == "debug" {
		l.log("DEBUG", msg, keysAndValues...)
	}
}

func (l *Logger) Info(msg string, keysAndValues ...interface{}) {
	if l.level == "debug" || l.level == "info" {
		l.log("INFO", msg, keysAndValues...)
	}
}

func (l *Logger) Warn(msg string, keysAndValues ...interface{}) {
	if l.level != "error" {
		l.log("WARN", msg, keysAndValues...)
	}
}

func (l *Logger) Error(msg string, keysAndValues ...interface{}) {
	l.log("ERROR", msg, keysAndValues...)
}

func (l *Logger) Connection(msg string, keysAndValues ...interface{}) {
	if l.level == "debug" {
		l.log("CONN", msg, keysAndValues...)
	}
}

type Stream struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	mu   sync.Mutex
}

func NewStream(conn net.Conn) *Stream {
	return &Stream{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(conn),
	}
}

func NewStreamWithReader(conn net.Conn, reader io.Reader) *Stream {
	return &Stream{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(reader),
	}
}

func (s *Stream) Send(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(msg)
}

func (s *Stream) Recv() (Message, error) {
	var msg Message
	err := s.dec.Decode(&msg)
	return msg, err
}

func (s *Stream) Close() error {
	return s.conn.Close()
}

func copyBidirectional(conn1, conn2 net.Conn) {
	var wg sync.WaitGroup
	var once sync.Once
	closeConnections := func() {
		conn1.Close()
		conn2.Close()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(conn1, conn2)
		once.Do(closeConnections)
	}()
	go func() {
		defer wg.Done()
		io.Copy(conn2, conn1)
		once.Do(closeConnections)
	}()
	wg.Wait()
}

func marshalMessage(data interface{}) ([]byte, error) {
	return json.Marshal(data)
}

// --- FIN DU CODE COMMUN ---

// --- DEBUT DU CODE SPÉCIFIQUE AU SERVEUR ---

type ConnectionManager struct {
	conns      sync.Map
	timers     sync.Map
	activeConn int64
	totalConn  int64
	mu         sync.RWMutex
	logger     *Logger
}

func NewConnectionManager(logger *Logger) *ConnectionManager {
	return &ConnectionManager{logger: logger}
}

func (cm *ConnectionManager) Store(id string, conn net.Conn) {
	cm.conns.Store(id, conn)
	cm.mu.Lock()
	cm.activeConn++
	cm.totalConn++
	active, total := cm.activeConn, cm.totalConn
	cm.mu.Unlock()
	cm.logger.Connection("Connection stored", "id", id, "active", active, "total", total)
	timer := time.AfterFunc(CleanupTimeout, func() {
		if conn, exists := cm.conns.LoadAndDelete(id); exists {
			conn.(net.Conn).Close()
			cm.decrementActive()
			cm.logger.Debug("Cleaned up stale connection", "id", id)
		}
		cm.timers.Delete(id)
	})
	cm.timers.Store(id, timer)
}

func (cm *ConnectionManager) LoadAndDelete(id string) (net.Conn, bool) {
	if timer, exists := cm.timers.LoadAndDelete(id); exists {
		timer.(*time.Timer).Stop()
	}
	if conn, exists := cm.conns.LoadAndDelete(id); exists {
		cm.decrementActive()
		return conn.(net.Conn), true
	}
	return nil, false
}

func (cm *ConnectionManager) decrementActive() {
	cm.mu.Lock()
	cm.activeConn--
	cm.mu.Unlock()
}

func (cm *ConnectionManager) Stats() (int64, int64) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.activeConn, cm.totalConn
}

type Server struct {
	servicePort   int
	connMgr       *ConnectionManager
	logger        *Logger
	controlStream *Stream
	controlMu     sync.RWMutex
}

func NewServer(servicePort int, logger *Logger) *Server {
	return &Server{
		servicePort: servicePort,
		connMgr:     NewConnectionManager(logger),
		logger:      logger,
	}
}

func (s *Server) setControlStream(stream *Stream) {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.controlStream = stream
}

func (s *Server) getControlStream() *Stream {
	s.controlMu.RLock()
	defer s.controlMu.RUnlock()
	return s.controlStream
}

func (s *Server) listenForService(ctx context.Context) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.servicePort))
	if err != nil {
		return fmt.Errorf("failed to listen on service port %d: %v", s.servicePort, err)
	}
	defer listener.Close()
	s.logger.Info("Service listener running", "port", s.servicePort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				s.logger.Error("Service accept error", "error", err)
				continue
			}
		}
		go s.forwardServiceConnection(conn)
	}
}

func (s *Server) forwardServiceConnection(incomingConn net.Conn) {
	control := s.getControlStream()
	if control == nil {
		s.logger.Warn("No client control channel active. Dropping incoming connection.")
		incomingConn.Close()
		return
	}

	id := uuid.New().String()
	s.logger.Connection("New tunnel connection", "id", id[:8])
	s.connMgr.Store(id, incomingConn)

	connData, err := marshalMessage(ConnectionData{ID: id})
	if err != nil {
		s.logger.Error("Failed to marshal connection data", "error", err)
		incomingConn.Close()
		return
	}

	connMsg := Message{Type: "connection", Data: connData}
	if err := control.Send(connMsg); err != nil {
		incomingConn.Close()
		s.logger.Error("Failed to send connection message to client", "error", err)
	}
}

func (s *Server) listenForClients(ctx context.Context) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", DefaultServerPort))
	if err != nil {
		return fmt.Errorf("failed to listen on control port %d: %v", DefaultServerPort, err)
	}
	defer listener.Close()
	s.logger.Info("Control server listening", "port", DefaultServerPort)

	if s.logger.level == "debug" {
		go s.printStats(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn, err := listener.Accept()
		if err != nil {
			s.logger.Error("Accept error", "error", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

func (s *Server) printStats(ctx context.Context) {
	ticker := time.NewTicker(StatsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			active, total := s.connMgr.Stats()
			if total > 0 {
				s.logger.Debug("Connection stats", "active", active, "total", total)
			}
		}
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	s.logger.Connection("New potential connection", "remote_addr", conn.RemoteAddr().String())
	reader := bufio.NewReader(conn)
	peekedBytes, err := reader.Peek(4)
	if err == nil && string(peekedBytes) == "GET " {
		s.logger.Debug("Detected HTTP GET request, likely a health check. Responding with 200 OK.")
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\n\r\n")
		return
	}

	stream := NewStreamWithReader(conn, reader)
	msg, err := stream.Recv()
	if err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			s.logger.Error("Error receiving initial message", "error", err)
		}
		return
	}

	switch msg.Type {
	case "hello":
		s.handleClientControl(stream, msg)
	case "accept":
		s.handleAccept(stream, msg)
	default:
		s.logger.Warn("Unknown message type", "type", msg.Type)
	}
}

func (s *Server) handleClientControl(stream *Stream, msg Message) {
	defer s.setControlStream(nil)
	var helloData HelloData
	if err := json.Unmarshal(msg.Data, &helloData); err != nil {
		s.logger.Error("Invalid hello data", "error", err)
		return
	}

	s.logger.Info("Client control channel established", "client_port", helloData.Port)
	s.setControlStream(stream)

	responseData, err := marshalMessage(HelloData{Port: s.servicePort})
	if err != nil {
		s.logger.Error("Failed to marshal response data", "error", err)
		return
	}

	response := Message{Type: "hello", Data: responseData}
	if err := stream.Send(response); err != nil {
		s.logger.Error("Failed to send hello response", "error", err)
		return
	}

	for {
		if _, err := stream.Recv(); err != nil {
			if err != io.EOF {
				s.logger.Info("Client control channel disconnected", "error", err)
			} else {
				s.logger.Info("Client control channel disconnected gracefully")
			}
			return
		}
	}
}

func (s *Server) handleAccept(stream *Stream, msg Message) {
	var acceptData ConnectionData
	if err := json.Unmarshal(msg.Data, &acceptData); err != nil {
		s.logger.Error("Invalid accept data", "error", err)
		return
	}
	s.logger.Connection("Forwarding connection", "id", acceptData.ID[:8])
	if incomingConn, exists := s.connMgr.LoadAndDelete(acceptData.ID); exists {
		defer incomingConn.Close()
		copyBidirectional(stream.conn, incomingConn)
	} else {
		s.logger.Warn("Connection not found", "id", acceptData.ID[:8])
	}
}

// --- FIN DU CODE SPÉCIFIQUE AU SERVEUR ---

func main() {
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	servicePort := flag.Int("service-port", 0, "The port of the local service to expose")
	flag.Parse()

	if *servicePort == 0 {
		fmt.Fprintln(os.Stderr, "Error: -service-port is required")
		flag.Usage()
		os.Exit(1)
	}

	logger := NewLogger(*logLevel)
	server := NewServer(*servicePort, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := server.listenForService(ctx); err != nil && err != context.Canceled {
			logger.Error("Service listener failed", "error", err)
			cancel()
		}
	}()

	if err := server.listenForClients(ctx); err != nil {
		logger.Error("Client listener failed", "error", err)
		os.Exit(1)
	}
}
