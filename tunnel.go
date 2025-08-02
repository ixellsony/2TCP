package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const (
	NetworkTimeout    = 5 * time.Second
	DefaultServerPort = 8080
	CleanupTimeout    = 30 * time.Second
	StatsInterval     = 30 * time.Second
)

// Message structures
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

// Optimized logger
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
	if len(keysAndValues) == 0 {
		l.Printf("[%s] %s", prefix, msg)
		return
	}

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

// JSON stream handler
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

// Constructeur de Stream modifié pour accepter un io.Reader personnalisé pour le décodeur
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

// Connection manager with atomic operations
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

// Utility function for bidirectional copy
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

// Marshal with error handling
func marshalMessage(data interface{}) ([]byte, error) {
	return json.Marshal(data)
}

// Server implementation
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

// --- DEBUT DE LA MODIFICATION PRINCIPALE ---

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	s.logger.Connection("New potential connection", "remote_addr", conn.RemoteAddr().String())
	
	// Utilise un bufio.Reader pour "jeter un oeil" (Peek) aux premières données
	// sans les consommer du flux de la connexion.
	reader := bufio.NewReader(conn)
	
	// On regarde les 4 premiers octets pour détecter une requête HTTP (ex: "GET ").
	peekedBytes, err := reader.Peek(4)
	if err == nil && string(peekedBytes) == "GET " {
		s.logger.Debug("Detected HTTP GET request, likely a health check. Responding with 200 OK.")
		// Répond avec un simple 200 OK pour satisfaire le health check.
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\n\r\n")
		return // On ferme la connexion et on arrête le traitement.
	}

	// Si ce n'est pas une requête GET, on continue normalement.
	// Le reader contient toujours l'intégralité du message original.
	stream := NewStreamWithReader(conn, reader)

	msg, err := stream.Recv()
	if err != nil {
		// On ignore les erreurs EOF qui sont normales lors de la déconnexion
		// et les erreurs "unexpected EOF" qui peuvent arriver si un client se déconnecte abruptement.
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

// --- FIN DE LA MODIFICATION PRINCIPALE ---

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

// Client implementation
type Client struct {
	localPort  int
	serverAddr string
	logger     *Logger
	connCount  int64
	mu         sync.Mutex
}

func NewClient(localPort int, serverAddr string, logger *Logger) *Client {
	return &Client{
		localPort:  localPort,
		serverAddr: serverAddr,
		logger:     logger,
	}
}

func (c *Client) Connect(ctx context.Context) error {
	address := fmt.Sprintf("%s:%d", c.serverAddr, DefaultServerPort)

	conn, err := net.DialTimeout("tcp", address, NetworkTimeout)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %v", err)
	}

	stream := NewStream(conn)

	helloData, err := marshalMessage(HelloData{Port: c.localPort})
	if err != nil {
		stream.Close()
		return fmt.Errorf("failed to marshal hello data: %v", err)
	}

	hello := Message{Type: "hello", Data: helloData}
	if err := stream.Send(hello); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send hello: %v", err)
	}

	response, err := stream.Recv()
	if err != nil {
		stream.Close()
		return fmt.Errorf("failed to receive hello response: %v", err)
	}

	if response.Type != "hello" {
		stream.Close()
		return fmt.Errorf("unexpected response type: %s", response.Type)
	}

	var helloResponseData HelloData
	if err := json.Unmarshal(response.Data, &helloResponseData); err != nil {
		stream.Close()
		return fmt.Errorf("invalid hello response: %v", err)
	}

	c.logger.Info("Tunnel established",
		"public_url", fmt.Sprintf("%s:%d", c.serverAddr, helloResponseData.Port),
		"local_port", c.localPort)

	return c.listen(ctx, stream)
}

func (c *Client) listen(ctx context.Context, stream *Stream) error {
	defer stream.Close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				c.logger.Info("Server disconnected. Will attempt to reconnect...")
				return nil
			}
			return fmt.Errorf("failed to receive message: %v", err)
		}

		if msg.Type == "connection" {
			var connData ConnectionData
			if err := json.Unmarshal(msg.Data, &connData); err != nil {
				c.logger.Error("Invalid connection data", "error", err)
				continue
			}
			go c.handleConnection(ctx, connData.ID)
		}
	}
}

func (c *Client) handleConnection(ctx context.Context, id string) {
	c.mu.Lock()
	c.connCount++
	count := c.connCount
	c.mu.Unlock()

	c.logger.Connection("Handling connection", "id", id[:8], "count", count)

	address := fmt.Sprintf("%s:%d", c.serverAddr, DefaultServerPort)

	serverConn, err := net.DialTimeout("tcp", address, NetworkTimeout)
	if err != nil {
		c.logger.Error("Failed to connect to server", "error", err)
		return
	}
	defer serverConn.Close()

	serverStream := NewStream(serverConn)

	acceptData, err := marshalMessage(ConnectionData{ID: id})
	if err != nil {
		c.logger.Error("Failed to marshal accept data", "error", err)
		return
	}

	accept := Message{Type: "accept", Data: acceptData}
	if err := serverStream.Send(accept); err != nil {
		c.logger.Error("Failed to send accept", "error", err)
		return
	}

	localConn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", c.localPort), NetworkTimeout)
	if err != nil {
		c.logger.Error("Failed to connect to local service", "error", err)
		return
	}
	defer localConn.Close()

	copyBidirectional(serverConn, localConn)
	c.logger.Connection("Connection closed", "id", id[:8])
}

// CLI
func main() {
	var logLevel string

	rootCmd := &cobra.Command{
		Use:   "tunnel",
		Short: "A TCP tunnel application",
		Long:  "A simple and efficient TCP tunnel application designed to work behind a reverse proxy.",
	}

	serverCmd := &cobra.Command{
		Use:   "server [service_port]",
		Short: "Run as server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			servicePort, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid service port: %v", err)
			}

			logger := NewLogger(logLevel)
			server := NewServer(servicePort, logger)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go func() {
				if err := server.listenForService(ctx); err != nil && err != context.Canceled {
					logger.Error("Service listener failed", "error", err)
					cancel()
				}
			}()

			return server.listenForClients(ctx)
		},
	}

	clientCmd := &cobra.Command{
		Use:   "client [local_port] [server_address]",
		Short: "Run as client",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPort, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid local port: %v", err)
			}

			logger := NewLogger(logLevel)
			
			for {
				client := NewClient(localPort, args[1], logger)
				ctx, cancel := context.WithCancel(context.Background())
				
				err := client.Connect(ctx)
				if err != nil && err != context.Canceled {
					logger.Error("Client error", "error", err)
				}
				
				cancel()
				
				select {
				case <-time.After(5 * time.Second):
					logger.Info("Attempting to reconnect...")
				}
			}
		},
	}

	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.AddCommand(serverCmd, clientCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
