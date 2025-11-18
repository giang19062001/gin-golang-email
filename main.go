package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

type EmailMessage struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

var (
	oauthConfig *oauth2.Config
	gmailSrv    *gmail.Service
)

func main() {
	r := gin.Default()

	// Load credentials.json
	b, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Không đọc được credentials.json: %v", err)
	}
	oauthConfig, err = google.ConfigFromJSON(b, gmail.GmailSendScope)
	if err != nil {
		log.Fatalf("Không tạo được config OAuth2: %v", err)
	}

	// Cố gắng đọc token từ file
	gmailSrv, err = getGmailServiceFromFile()
	if err != nil {
		log.Println("Chưa có token hoặc token lỗi, cần login OAuth2")
		fmt.Println("Mở URL này để login:",
			oauthConfig.AuthCodeURL(
				"state-token",
				oauth2.AccessTypeOffline, // → “Tôi muốn refresh_token để dùng API lâu dài mà không cần login lại”
			))
	}

	// Endpoint xác thực OAuth2
	// ! Đảm bảo Authorized redirect URIs: http://localhost:8001/oauth2/callback  trên google cloud phải trùng path callback này
	r.GET("/oauth2/callback", handleOAuthCallback)

	// Route test
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	// Khởi động consumer RabbitMQ
	go initRabbitConsumer()

	// Chạy server
	if err := r.Run(":8001"); err != nil {
		log.Fatalf("Không thể chạy Gin: %v", err)
	}
}

// ! bắt buộc xác thực đăng nhập trước khi chạy gửi email
func handleOAuthCallback(c *gin.Context) {
	// lấy mã code từ query callback
	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Không có code"})
		return
	}

	// Đổi code lấy token
	token, err := oauthConfig.Exchange(context.Background(), code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi đổi code lấy token: " + err.Error()})
		return
	}

	// lưu token vào file local
	saveToken(token)

	// Tạo Gmail service
	srv, err := gmail.NewService(context.Background(), option.WithTokenSource(oauthConfig.TokenSource(context.Background(), token)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tạo được Gmail service: " + err.Error()})
		return
	}

	// gán gmail service toàn cục
	gmailSrv = srv
	c.JSON(http.StatusOK, gin.H{"message": "Xác thực thành công, Gmail API đã sẵn sàng"})
}

func saveToken(token *oauth2.Token) {
	// /tạo file (nếu đã có thì ghi đè)
	f, err := os.Create("token.json")
	if err != nil {
		log.Printf("Không thể tạo file token.json: %v", err)
		return
	}
	defer f.Close()
	// ghi token vào file dưới dạng JSON
	json.NewEncoder(f).Encode(token)
}

func getGmailServiceFromFile() (*gmail.Service, error) {
	ctx := context.Background()
	// Mở file chứa token đã lưu trước đó
	f, err := os.Open("token.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// JSON trong file và nhét vào struct oauth2.Token
	var token oauth2.Token
	if err := json.NewDecoder(f).Decode(&token); err != nil {
		return nil, err
	}

	// ? Khi token hết hạn → tự động lấy refresh_token để xin access_token mới -> Không cần người dùng đăng nhập thủ công lại lần hai.
	tokenSource := oauthConfig.TokenSource(ctx, &token)

	// tạo ra Gmail Service
	return gmail.NewService(ctx, option.WithTokenSource(tokenSource))
}

func initRabbitConsumer() {
	// Kết nối RabbitMQ
	rabbitUrl := "amqp://guest:123@localhost:5672/"
	conn, err := amqp.Dial(rabbitUrl)
	if err != nil {
		log.Fatalf("Không thể kết nối RabbitMQ: %v", err)
	}
	defer conn.Close()

	// Mở RabbitMQ channel
	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("Không thể mở channel: %v", err)
	}
	defer ch.Close()

	// Đăng ký consumer
	queueName := "send-email"
	msgs, err := ch.Consume(queueName, "", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Không thể đăng ký consumer: %v", err)
	}

	// Goroutine xử lý message
	// ? Goroutine xử lý message để chạy song song với chương trình chính với nhiệm vụ bắt message từ rabbit chanel
	// ? nếu ko dùng goroutin để bắt message từ rabbit chanel thì nó sẽ block chương trình chính
	go func() {
		for d := range msgs {
			var email EmailMessage
			if err := json.Unmarshal(d.Body, &email); err != nil {
				log.Printf("Lỗi parse JSON: %v", err)
				continue
			}
			fmt.Printf("Nhận email:\n- To: %s\n- Subject: %s\n- Body: %s\n", email.To, email.Subject, email.Body)

			if gmailSrv == nil {
				log.Println("Gmail API chưa sẵn sàng, cần login OAuth2 trước")
				continue
			}
			// Gửi email qua Gmail API
			if err := sendEmail(gmailSrv, email); err != nil {
				log.Printf("Lỗi gửi email: %v", err)
			}
		}
	}()

	log.Println("Consumer RabbitMQ đang chạy...")
	// ? giữ goroutine sống mãi để bắt được event - bằng cách tạo 1 channel ( vì chanel sẽ sống mãi trong chương trình đến khi nhận được giá trị )

	sig := make(chan os.Signal, 1)                    // tạo 1 channel với duy nhất 1 giá trị sẽ được nhận
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM) // Interrupt: Ctrl+C, SIGTERM: tắt chương trình -> gửi giá trị cho channel
	<-sig                                             // nhận giá trị cho channel -> đồng nghĩa thoát block goroutine -> thoát

	log.Println("Consumer RabbitMQ đang dừng...")
}

func sendEmail(srv *gmail.Service, email EmailMessage) error {
	msg := []byte(
		"From: me\n" +
			"To: " + email.To + "\n" +
			"Subject: " + email.Subject + "\n\n" +
			email.Body,
	)

	var message gmail.Message
	message.Raw = base64.URLEncoding.EncodeToString(msg)

	// Gửi email
	_, err := srv.Users.Messages.Send("me", &message).Do()
	return err
}
