package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

const maxWebhookBody = 1 << 20 // 1 MB

// ComputeSignature returns a GitHub-compatible HMAC-SHA256 signature for body.
func ComputeSignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func validateHMAC(body []byte, secret, signature string) bool {
	expected := ComputeSignature(body, secret)
	return hmac.Equal([]byte(expected), []byte(signature))
}

func webhookHandler(trigger func(), secretPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		if secretPath != "" {
			secretData, err := os.ReadFile(secretPath)
			if err != nil {
				slog.Error("reading webhook secret file", "path", secretPath, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			secret := strings.TrimSpace(string(secretData))

			signature := r.Header.Get("X-Hub-Signature-256")
			if !validateHMAC(body, secret, signature) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		slog.Info("webhook triggered")
		trigger()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "accepted")
	})
}
