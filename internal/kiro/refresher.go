package kiro

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

)

type TokenRefresher struct {
	Creds     *KiroCredentials
	Transport http.RoundTripper // optional proxy transport
}

func (r *TokenRefresher) GetRefreshURL() string {
	region := r.Creds.GetRegion()
	authMethod := r.Creds.GetAuthMethod()

	if authMethod == "idc" {
		return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
	}
	return fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)
}

func (r *TokenRefresher) ValidateRefreshToken() (bool, string) {
	rt := r.Creds.RefreshToken
	if rt == "" {
		return false, "missing refresh_token"
	}
	if len(strings.TrimSpace(rt)) == 0 {
		return false, "refresh_token is empty"
	}
	if len(rt) < 100 || strings.HasSuffix(rt, "...") {
		return false, fmt.Sprintf("refresh_token appears truncated (len=%d)", len(rt))
	}
	return true, ""
}

func (r *TokenRefresher) Refresh() (bool, string) {
	ok, errMsg := r.ValidateRefreshToken()
	if !ok {
		return false, errMsg
	}

	refreshURL := r.GetRefreshURL()
	authMethod := r.Creds.GetAuthMethod()

	machineID := GenerateMachineID(r.Creds.ProfileArn, r.Creds.ClientID)

	var bodyMap map[string]string
	headers := map[string]string{}

	if authMethod == "idc" {
		if r.Creds.ClientID == "" || r.Creds.ClientSecret == "" {
			return false, "IdC auth requires clientId and clientSecret"
		}
		bodyMap = map[string]string{
			"refreshToken": r.Creds.RefreshToken,
			"clientId":     r.Creds.ClientID,
			"clientSecret": r.Creds.ClientSecret,
			"grantType":    "refresh_token",
		}
		headers["Content-Type"] = "application/json"
		headers["x-amz-user-agent"] = fmt.Sprintf("aws-sdk-js/3.738.0 KiroIDE-%s-%s", DefaultKiroVersion, machineID)
		headers["User-Agent"] = "node"
	} else {
		bodyMap = map[string]string{
			"refreshToken": r.Creds.RefreshToken,
		}
		headers["Content-Type"] = "application/json"
		headers["User-Agent"] = fmt.Sprintf("KiroIDE-%s-%s", DefaultKiroVersion, machineID)
		headers["Accept"] = "application/json, text/plain, */*"
	}

	bodyBytes, _ := json.Marshal(bodyMap)

	transport := r.Transport
	if transport == nil {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	req, err := http.NewRequest("POST", refreshURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return false, fmt.Sprintf("create request failed: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("refresh request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 {
			return false, "credentials expired or invalid, need re-login"
		}
		if resp.StatusCode == 429 {
			return false, "too many requests, try later"
		}
		return false, fmt.Sprintf("refresh failed: %d - %s", resp.StatusCode, TruncStr(string(respBody), 200))
	}

	var data map[string]any
	if err := json.Unmarshal(respBody, &data); err != nil {
		return false, fmt.Sprintf("parse response failed: %v", err)
	}

	newToken := getStr(data, "accessToken")
	if newToken == "" {
		newToken = getStr(data, "access_token")
	}
	if newToken == "" {
		return false, "no access_token in response"
	}

	r.Creds.AccessToken = newToken

	if rt := getStr(data, "refreshToken"); rt != "" {
		r.Creds.RefreshToken = rt
	} else if rt := getStr(data, "refresh_token"); rt != "" {
		r.Creds.RefreshToken = rt
	}

	if arn := getStr(data, "profileArn"); arn != "" {
		r.Creds.ProfileArn = arn
		log.Printf("[TokenRefresh] 获取到 profileArn: %s", arn[:min(len(arn), 60)]+"...")
	} else {
		log.Printf("[TokenRefresh] 响应中无 profileArn，当前值: %q", r.Creds.ProfileArn)
	}

	if expiresIn := getFloat(data, "expiresIn"); expiresIn > 0 {
		r.Creds.ExpiresAt = float64(time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli())
	} else if expiresIn := getFloat(data, "expires_in"); expiresIn > 0 {
		r.Creds.ExpiresAt = float64(time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli())
	}

	r.Creds.LastRefresh = time.Now().UTC().Format(time.RFC3339)

	log.Printf("[TokenRefresh] Token refreshed successfully for %s", r.Creds.ClientID)
	return true, newToken
}

func getStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getFloat(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int64:
			return float64(n)
		case json.Number:
			f, _ := n.Float64()
			return f
		}
	}
	return 0
}


