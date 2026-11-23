package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

func signCoveWebhook(request *http.Request, secret string, payload []byte) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(payload)
	request.Header.Set("X-Cove-Timestamp", timestamp)
	request.Header.Set("X-Cove-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
}
