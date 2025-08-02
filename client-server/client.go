package main

import (
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
)

// Constantes pour le client
const (
	NetworkTimeout = 5 * time.Second
	// Le port par défaut du serveur est nécessaire pour la connexion initiale
	DefaultServerPort = 8080
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

// --- DEBUT DU CODE SPÉCIFIQUE AU CLIENT ---

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

// --- FIN DU CODE SPÉCIFIQUE AU CLIENT ---

func main() {
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	localPort := flag.Int("local-port", 0, "The local port to forward connections to")
	serverAddr := flag.String("server-addr", "", "The address of the tunnel server")
	flag.Parse()

	if *localPort == 0 || *serverAddr == "" {
		fmt.Fprintln(os.Stderr, "Error: -local-port and -server-addr are required")
		flag.Usage()
		os.Exit(1)
	}

	logger := NewLogger(*logLevel)

	for {
		client := NewClient(*localPort, *serverAddr, logger)
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
}
