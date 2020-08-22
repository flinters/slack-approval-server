package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"bytes"

	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	_ "github.com/heroku/x/hmetrics/onload"
)

func newPool(server string) *redis.Pool {

	return &redis.Pool{

		MaxIdle:     3,
		IdleTimeout: 240 * time.Second,

		Dial: func() (redis.Conn, error) {
			u, err := url.Parse(server)
			if err != nil {
				return nil, err
			}
			// ignore username
			if u.User != nil {
				p, s := u.User.Password()
				if s {
					u.User = url.UserPassword("", p)
				}
			}
			c, err := redis.DialURL(u.String())
			if err != nil {
				return nil, err
			}
			return c, err
		},

		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
}

type EventCreateRequest struct {
	TimeoutEpoch int64 `json:"timeout_epoch"`
}

type Status string

const (
	InProgress = Status("in-progress")
	Approved   = Status("approved")
	Rejected   = Status("rejected")
	Timeout    = Status("timeout")
)

type Event struct {
	ID           string   `json:"id"`
	TimeoutEpoch int64    `json:"timeout_epoch"`
	Approvers    []string `json:"approvers"`
	Rejecters    []string `json:"rejecters"`
	Status       Status   `json:"status"`
}

func NewEvent(timeoutEpoch int64) (*Event, error) {
	id, err := MakeRandomStr(16)
	if err != nil {
		return nil, err
	}

	event := Event{
		ID:           id,
		TimeoutEpoch: timeoutEpoch,
		Approvers:    []string{},
		Status:       InProgress,
	}

	event.refreshStatus()

	return &event, nil
}

func (event *Event) refreshStatus() {
	if event.Status == "" {
		event.Status = InProgress
	}
	if len(event.Rejecters) > 0 {
		event.Status = Rejected
	} else if len(event.Approvers) > 0 {
		event.Status = Approved
	} else if time.Now().After(time.Unix(event.TimeoutEpoch, 0)) {
		event.Status = Timeout
	}
}

func (event *Event) Approve(user string) {
	event.Approvers = append(event.Approvers, user)
	event.refreshStatus()
}

func (event *Event) Reject(user string) {
	event.Rejecters = append(event.Rejecters, user)
	event.refreshStatus()
}

// slack からくるメッセージのうち興味のある部分だけ
type CallbackMessage struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
		TeamID   string `json:"team_id"`
	} `json:"user"`
	Message     map[string]interface{} `json:"message"`
	ResponseURL string                 `json:"response_url"`
	Actions     []struct {
		BlockID string `json:"block_id"`
		Value   string `json:"value"`
	} `json:"actions"`
}

func (msg *CallbackMessage) IsApproved() bool {
	return msg.Actions[0].Value == "1"
}

func (msg *CallbackMessage) EventID() string {
	return msg.Actions[0].BlockID
}

func (msg *CallbackMessage) Validate() error {
	if len(msg.Actions) == 0 {
		return errors.New("actions is empty")
	}
	v := msg.Actions[0].Value
	if v != "0" && v != "1" {
		return fmt.Errorf("Unknown value: %s", v)
	}
	blocks, ok := msg.Message["blocks"]
	if !ok {
		return errors.New("message.blocks is not found")
	}
	switch blocks := blocks.(type) {
	case []interface{}:
		actionsCnt := 0
		for _, elem := range blocks {
			switch elem := elem.(type) {
			case map[string]interface{}:
				if elem["type"] == "actions" {
					actionsCnt++
				}
			default:
				return errors.New("A blocks element is not a map")
			}
		}
		if actionsCnt == 0 {
			return errors.New("No actions block found")
		}
		if actionsCnt > 1 {
			return errors.New("2 or more actions blocks found")
		}
	default:
		return errors.New("A blocks element is not an array")
	}
	return nil
}

func MakeRandomStr(digit uint32) (string, error) {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 乱数を生成
	b := make([]byte, digit)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("unexpected error...")
	}

	// letters からランダムに取り出して文字列を生成
	var result string
	for _, v := range b {
		// index が letters の長さに収まるように調整
		result += string(letters[int(v)%len(letters)])
	}
	return result, nil
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		log.Fatal("$PORT must be set")
	}

	redisURL := os.Getenv("REDISTOGO_URL")
	if redisURL == "" {
		log.Fatal("REDISTOGO_URL must be set")
	}

	pool := newPool(redisURL)

	router := gin.New()
	router.Use(gin.Logger())
	router.LoadHTMLGlob("templates/*.tmpl.html")
	router.Static("/static", "static")

	router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.tmpl.html", nil)
	})

	router.POST("/events", func(c *gin.Context) {
		var req EventCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request to create an event"})
			return
		}
		event, err := NewEvent(req.TimeoutEpoch)

		conn := pool.Get()
		defer conn.Close()
		err = storeEvent(event, conn)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, event)
	})

	router.GET("/events/:id", func(c *gin.Context) {
		conn := pool.Get()
		defer conn.Close()
		id := c.Param("id")
		event, err := fetchEvent(id, conn)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, event)
	})

	router.POST("/callback", func(c *gin.Context) {
		payloadJson := c.PostForm("payload")
		log.Println("payload", payloadJson)
		var msg CallbackMessage
		err := json.Unmarshal([]byte(payloadJson), &msg)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "payload is not JSON: " + payloadJson})
			return
		}
		log.Println(msg)

		if err = msg.Validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error() + ": " + payloadJson})
			return
		}
		go processCallback(msg, pool)
		c.String(http.StatusOK, "")
	})

	router.Run(":" + port)
}

func storeEvent(event *Event, conn redis.Conn) error {
	bytes, err := json.Marshal(event)
	eventString := string(bytes)
	if err != nil {
		return err
	}
	_, err = conn.Do("SET", event.ID, eventString)
	if err != nil {
		return err
	}
	return nil
}

func fetchEvent(id string, conn redis.Conn) (*Event, error) {
	bytes, err := redis.Bytes(conn.Do("GET", id))
	if err != nil {
		return nil, err
	}
	var event Event
	err = json.Unmarshal(bytes, &event)
	if err != nil {
		return nil, err
	}
	event.refreshStatus()
	return &event, nil
}

func formatMessage(msg CallbackMessage, event *Event) []byte {
	return nil
}

func processCallback(msg CallbackMessage, pool *redis.Pool) {
	conn := pool.Get()
	defer conn.Close()
	event, err := fetchEvent(msg.EventID(), conn)
	if err != nil {
		log.Println("Failed to get event:", msg)
		return
	}
	userID := msg.User.ID
	if msg.IsApproved() {
		event.Approve(userID)
	} else {
		event.Reject(userID)
	}
	err = storeEvent(event, conn)
	if err != nil {
		log.Println("Failed to store event:", event)
		return
	}

	json := formatMessage(msg, event)

	res, err := http.Post(msg.ResponseURL, "application/json", bytes.NewBuffer(json))
	if err != nil {
		log.Println("Failed to post message:", err.Error())
		return
	}
	if (res.StatusCode % 100) != 2 {
		log.Println("Response status is not good:", res.Status)
		return
	}
	log.Println("processCallback done")
}
