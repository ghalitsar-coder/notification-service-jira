package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Notification struct {
	ID              uint      `json:"notification_id" gorm:"primaryKey"`
	RecipientUserID uint      `json:"recipient_user_id" gorm:"index;not null"`
	Type            string    `json:"type" gorm:"size:50;not null"`
	ReferenceID     string    `json:"reference_id,omitempty" gorm:"size:100"`
	ActorName       string    `json:"actor_name,omitempty" gorm:"size:255"`
	Message         string    `json:"message" gorm:"size:500;not null"`
	IsRead          bool      `json:"is_read" gorm:"default:false"`
	CreatedAt       time.Time `json:"created_at"`
}

type Event struct {
	RecipientUserID uint      `json:"recipient_user_id"`
	ActorName       string    `json:"actor_name"`
	Type            string    `json:"type"`
	ReferenceID     string    `json:"reference_id"`
	Message         string    `json:"message"`
	CreatedAt       time.Time `json:"created_at"`
}

type wsPayload struct {
	NotificationID uint      `json:"notification_id"`
	Type           string    `json:"type"`
	Message        string    `json:"message"`
	IsRead         bool      `json:"is_read"`
	CreatedAt      time.Time `json:"created_at"`
}

type hub struct {
	mu      sync.RWMutex
	clients map[uint]map[*websocket.Conn]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[uint]map[*websocket.Conn]struct{})}
}

func (h *hub) add(userID uint, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[userID] == nil {
		h.clients[userID] = make(map[*websocket.Conn]struct{})
	}
	h.clients[userID][conn] = struct{}{}
}

func (h *hub) remove(userID uint, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.clients[userID]; ok {
		delete(m, conn)
		if len(m) == 0 {
			delete(h.clients, userID)
		}
	}
}

func (h *hub) send(userID uint, payload wsPayload) {
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0)
	for c := range h.clients[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.WriteJSON(payload); err != nil {
			_ = c.Close()
			h.remove(userID, c)
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	// Membaca file .env jika ada (ignore error jika file tidak ada)
	_ = godotenv.Load()

	port := env("PORT", "8081")
	rabbitURL := env("RABBITMQ_URL", "amqp://guest:guest@rabbitmq:5672/")
	exchange := env("NOTIFICATION_EXCHANGE", "notification_events")
	queueName := env("NOTIFICATION_QUEUE", "notification_queue")
	jwtSecret := env("JWT_SECRET", "")
	allowInsecureUserID := strings.EqualFold(env("ALLOW_INSECURE_USER_ID", "true"), "true")
	dbPath := env("DB_PATH", "./data/notifications.db")

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&Notification{}); err != nil {
		log.Fatalf("migrate db: %v", err)
	}

	h := newHub()
	stopConsumer := make(chan struct{})
	go consumeLoop(db, h, rabbitURL, exchange, queueName, stopConsumer)

	r := gin.Default()
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/api/notifications", func(c *gin.Context) {
		userID, ok := extractUserID(c, jwtSecret, allowInsecureUserID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var rows []Notification
		if err := db.Where("recipient_user_id = ?", userID).Order("created_at DESC").Limit(100).Find(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch notifications"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": rows})
	})

	r.POST("/api/notifications/:id/read", func(c *gin.Context) {
		userID, ok := extractUserID(c, jwtSecret, allowInsecureUserID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		tx := db.Model(&Notification{}).Where("id = ? AND recipient_user_id = ?", id, userID).Update("is_read", true)
		if tx.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark as read"})
			return
		}
		if tx.RowsAffected == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "notification not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	r.GET("/ws", func(c *gin.Context) {
		userID, ok := extractUserID(c, jwtSecret, allowInsecureUserID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		h.add(userID, conn)
		defer func() {
			h.remove(userID, conn)
			_ = conn.Close()
		}()

		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		done := make(chan struct{})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					close(done)
					return
				}
			}
		}()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}
	go func() {
		log.Printf("notification-service listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	close(stopConsumer)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func consumeLoop(db *gorm.DB, h *hub, rabbitURL, exchange, queueName string, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		conn, err := amqp.Dial(rabbitURL)
		if err != nil {
			log.Printf("rabbit connect failed: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		ch, err := conn.Channel()
		if err != nil {
			_ = conn.Close()
			time.Sleep(2 * time.Second)
			continue
		}

		err = ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil)
		if err != nil {
			_ = ch.Close()
			_ = conn.Close()
			time.Sleep(2 * time.Second)
			continue
		}

		q, err := ch.QueueDeclare(queueName, true, false, false, false, nil)
		if err != nil {
			_ = ch.Close()
			_ = conn.Close()
			time.Sleep(2 * time.Second)
			continue
		}
		if err := ch.QueueBind(q.Name, "notification.*", exchange, false, nil); err != nil {
			_ = ch.Close()
			_ = conn.Close()
			time.Sleep(2 * time.Second)
			continue
		}

		msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
		if err != nil {
			_ = ch.Close()
			_ = conn.Close()
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("rabbit consumer online queue=%s exchange=%s", q.Name, exchange)

		closed := make(chan *amqp.Error, 1)
		ch.NotifyClose(closed)
	loop:
		for {
			select {
			case <-stop:
				_ = ch.Close()
				_ = conn.Close()
				return
			case err := <-closed:
				if err != nil {
					log.Printf("rabbit channel closed: %v", err)
				}
				break loop
			case d, ok := <-msgs:
				if !ok {
					break loop
				}
				var ev Event
				if err := json.Unmarshal(d.Body, &ev); err != nil {
					_ = d.Nack(false, false)
					continue
				}
				if ev.CreatedAt.IsZero() {
					ev.CreatedAt = time.Now().UTC()
				}
				row := Notification{
					RecipientUserID: ev.RecipientUserID,
					Type:            ev.Type,
					ReferenceID:     ev.ReferenceID,
					ActorName:       ev.ActorName,
					Message:         ev.Message,
					IsRead:          false,
					CreatedAt:       ev.CreatedAt,
				}
				if err := db.Create(&row).Error; err != nil {
					_ = d.Nack(false, true)
					continue
				}
				h.send(ev.RecipientUserID, wsPayload{
					NotificationID: row.ID,
					Type:           row.Type,
					Message:        row.Message,
					IsRead:         row.IsRead,
					CreatedAt:      row.CreatedAt,
				})
				_ = d.Ack(false)
			}
		}
		_ = ch.Close()
		_ = conn.Close()
		time.Sleep(2 * time.Second)
	}
}

func extractUserID(c *gin.Context, jwtSecret string, allowInsecure bool) (uint, bool) {
	token := c.Query("token")
	if token != "" && jwtSecret != "" {
		parsed, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, errors.New("invalid signing method")
			}
			return []byte(jwtSecret), nil
		})
		if err == nil && parsed != nil && parsed.Valid {
			if claims, ok := parsed.Claims.(jwt.MapClaims); ok {
				if uidRaw, ok := claims["user_id"]; ok {
					switch v := uidRaw.(type) {
					case float64:
						return uint(v), true
					case string:
						if n, err := strconv.ParseUint(v, 10, 32); err == nil {
							return uint(n), true
						}
					}
				}
			}
		}
	}
	if !allowInsecure {
		return 0, false
	}
	userIDStr := c.Query("user_id")
	n, err := strconv.ParseUint(userIDStr, 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint(n), true
}

func env(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
