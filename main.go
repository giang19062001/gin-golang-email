package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Định nghĩa struct khớp với payload JSON
type EmailMessage struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func main() {
	// Khởi tạo Gin
	r := gin.Default()

	// Khởi động consumer RabbitMQ trong goroutine
	go initRabbitConsumer()

	// Ví dụ route test
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	// Chạy server Gin
	if err := r.Run(":8001"); err != nil {
		log.Fatalf("Không thể chạy Gin: %v", err)
	}
}

func initRabbitConsumer() {
	rabbitUrl := "amqp://guest:123@localhost:5672/"

	// Kết nối RabbitMQ
	conn, err := amqp.Dial(rabbitUrl)
	if err != nil {
		log.Fatalf("Không thể kết nối RabbitMQ: %v", err)
	}
	defer conn.Close()

	// Mở channel
	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("Không thể mở channel: %v", err)
	}
	defer ch.Close()

	// Đăng ký consumer từ rabbit channel
	queueName := "send-email"
	msgs, err := ch.Consume(
		queueName,
		"",    // Nếu để "", RabbitMQ sẽ tự tạo ID ngẫu nhiên.
		true,  // durable -> true: queue sẽ được lưu trên disk, không mất khi RabbitMQ restart.
		false, // false: queue không bị xóa tự động khi không có consumer
		false, // exclusive -> false: queue không bị giới hạn chỉ cho 1 connection, nhiều consumer có thể dùng
		false, // no-wait -> false: phải chờ RabbitMQ phản hồi việc khai báo queue
		nil,   // arguments -> nil : không truyền thêm option đặc biệt
	)
	if err != nil {
		log.Fatalf("Không thể đăng ký consumer: %v", err)
	}

	// Goroutine xử lý message
	go func() {
		for d := range msgs {
			var email EmailMessage
			if err := json.Unmarshal(d.Body, &email); err != nil {
				log.Printf("Lỗi parse JSON: %v", err)
				continue
			}
			fmt.Printf("Nhận email:\n- To: %s\n- Subject: %s\n- Body: %s\n", email.To, email.Subject, email.Body)
		}
	}()

	log.Println("Consumer RabbitMQ đang chạy...")
	// ! giữ goroutine sống mãi để bắt được event - bằng cách tạo 1 channel ( vì chanel sẽ sống mãi trong chương trình đến khi nhận được giá trị )
	sig := make(chan os.Signal, 1)                    // tạo 1 channel với duy nhất 1 giá trị sẽ được nhận
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM) // Interrupt: Ctrl+C, SIGTERM: tắt chương trình -> gửi giá trị cho channel
	<-sig                                             // nhận giá trị cho channel -> đồng nghĩa thoát block goroutine -> thoát

	log.Println("Consumer RabbitMQ đang dừng...")
}
