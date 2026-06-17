package main

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/email"
	"github.com/shepard-labs/go-clients/kms"
	"github.com/shepard-labs/go-clients/storage"
)

// server holds the constructed clients shared across requests. The client
// interfaces are safe for concurrent use.
type server struct {
	logger    *zap.Logger
	sender    email.Sender
	store     storage.Storage
	encryptor kms.Encryptor
}

// abortError writes a generic JSON error and stops the handler chain. The
// message is intentionally coarse; detailed causes are logged server-side, not
// returned to the caller.
func abortError(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg})
}

func (s *server) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// --- email ---

type sendEmailRequest struct {
	From     string   `json:"from"`
	To       []string `json:"to"`
	Subject  string   `json:"subject"`
	HTMLBody string   `json:"html_body"`
	TextBody string   `json:"text_body"`
}

func (s *server) sendEmail(c *gin.Context) {
	var req sendEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	msg := &email.Message{
		From:     req.From,
		To:       req.To,
		Subject:  req.Subject,
		HTMLBody: req.HTMLBody,
		TextBody: req.TextBody,
	}

	res, err := s.sender.Send(c.Request.Context(), msg)
	if err != nil {
		// Validation errors are caller's fault (400); everything else is a 502
		// from the upstream provider. Neither path echoes provider detail.
		if errors.Is(err, email.ErrInvalidMessage) {
			abortError(c, http.StatusBadRequest, "invalid email message")
			return
		}
		s.logger.Error("email send failed", zap.Error(err))
		abortError(c, http.StatusBadGateway, "failed to send email")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message_id": res.MessageID})
}

// --- storage ---

type uploadRequest struct {
	ObjectName  string `json:"object_name"`
	ContentB64  string `json:"content_base64"`
	ContentType string `json:"content_type"`
}

func (s *server) upload(c *gin.Context) {
	var req uploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	content, err := base64.StdEncoding.DecodeString(req.ContentB64)
	if err != nil {
		abortError(c, http.StatusBadRequest, "content_base64 is not valid base64")
		return
	}

	if err := s.store.Upload(c.Request.Context(), req.ObjectName, content, req.ContentType); err != nil {
		if errors.Is(err, storage.ErrInvalidObjectName) {
			abortError(c, http.StatusBadRequest, "invalid object name")
			return
		}
		s.logger.Error("storage upload failed", zap.Error(err), zap.String("object", req.ObjectName))
		abortError(c, http.StatusBadGateway, "failed to upload object")
		return
	}

	c.JSON(http.StatusOK, gin.H{"object_name": req.ObjectName})
}

func (s *server) download(c *gin.Context) {
	objectName := c.Query("object_name")

	content, err := s.store.Download(c.Request.Context(), objectName)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrInvalidObjectName):
			abortError(c, http.StatusBadRequest, "invalid object name")
		case errors.Is(err, storage.ErrObjectTooLarge):
			abortError(c, http.StatusRequestEntityTooLarge, "object too large")
		default:
			s.logger.Error("storage download failed", zap.Error(err), zap.String("object", objectName))
			abortError(c, http.StatusBadGateway, "failed to download object")
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"content_base64": base64.StdEncoding.EncodeToString(content)})
}

// --- kms ---

type encryptRequest struct {
	Plaintext string `json:"plaintext"`
}

func (s *server) encrypt(c *gin.Context) {
	var req encryptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	ciphertext, err := s.encryptor.EncryptCredentials(c.Request.Context(), req.Plaintext)
	if err != nil {
		if errors.Is(err, kms.ErrPlaintextTooLarge) {
			abortError(c, http.StatusRequestEntityTooLarge, "plaintext too large")
			return
		}
		s.logger.Error("kms encrypt failed", zap.Error(err))
		abortError(c, http.StatusBadGateway, "failed to encrypt")
		return
	}

	c.JSON(http.StatusOK, gin.H{"ciphertext": ciphertext})
}

type decryptRequest struct {
	Ciphertext string `json:"ciphertext"`
}

func (s *server) decrypt(c *gin.Context) {
	var req decryptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	plaintext, err := s.encryptor.DecryptCredentials(c.Request.Context(), req.Ciphertext)
	if err != nil {
		s.logger.Error("kms decrypt failed", zap.Error(err))
		abortError(c, http.StatusBadGateway, "failed to decrypt")
		return
	}

	c.JSON(http.StatusOK, gin.H{"plaintext": plaintext})
}
