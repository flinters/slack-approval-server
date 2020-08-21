package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

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
	TimeoutEpoch int `json:"timeout_epoch"`
}

type Event struct {
	TimeoutEpoch int      `json:"timeout_epoch"`
	Approvers    []string `json:"approvers"`
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
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := MakeRandomStr(16)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		conn := pool.Get()
		defer conn.Close()

		event := Event{
			TimeoutEpoch: req.TimeoutEpoch,
			Approvers:    []string{},
		}

		bytes, err := json.Marshal(&event)
		eventString := string(bytes)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		_, err = conn.Do("SET", id, eventString)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, eventString)
	})

	// router.GET("/events/:id", func(c *gin.Context) {
	// 	conn := pool.Get()
	// 	defer conn.Close()
	// 	id := c.Param("id")
	// 	conn.
	// })

	router.Run(":" + port)
}
