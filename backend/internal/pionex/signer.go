package pionex

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signer handles Pionex API HMAC SHA256 signature generation.
type Signer struct {
	apiKey    string
	apiSecret string
}

// NewSigner creates a new Pionex API Signer instance.
func NewSigner(apiKey, apiSecret string) *Signer {
	return &Signer{
		apiKey:    apiKey,
		apiSecret: apiSecret,
	}
}

// SignRequest computes the HMAC SHA256 signature for HTTP requests.
// Signature format: HMAC_SHA256(path + "?" + sortedQueryString + bodyString, apiSecret)
func (s *Signer) SignRequest(method, path string, queryParams url.Values, body []byte, timestamp int64) (string, error) {
	if timestamp <= 0 {
		timestamp = time.Now().UnixMilli()
	}

	params := url.Values{}
	for k, v := range queryParams {
		params[k] = v
	}
	params.Set("timestamp", strconv.FormatInt(timestamp, 10))

	var keys []string
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var queryParts []string
	for _, k := range keys {
		for _, v := range params[k] {
			queryParts = append(queryParts, fmt.Sprintf("%s=%s", k, v))
		}
	}
	queryString := strings.Join(queryParts, "&")

	rawToSign := fmt.Sprintf("%s?%s%s", path, queryString, string(body))

	mac := hmac.New(sha256.New, []byte(s.apiSecret))
	_, err := mac.Write([]byte(rawToSign))
	if err != nil {
		return "", fmt.Errorf("failed to write hmac: %w", err)
	}

	return hex.EncodeToString(mac.Sum(nil)), nil
}
