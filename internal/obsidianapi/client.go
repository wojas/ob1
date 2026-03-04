package obsidianapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.obsidian.md"

const SupportedEncryptionVersion = 3

type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

type SignInRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	MFA      string `json:"mfa"`
}

type UserSession struct {
	Token string
	User  json.RawMessage
}

type UserInfo struct {
	Name  string
	Email string
	Raw   json.RawMessage
}

type Vault struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Region            string `json:"region"`
	Host              string `json:"host"`
	Salt              string `json:"salt"`
	Password          string `json:"password"`
	EncryptionVersion int    `json:"encryption_version"`
}

type VaultList struct {
	Vaults []Vault
	Shared []Vault
}

type APIError struct {
	StatusCode int
	Message    string
}

type exchangeLog struct {
	method          string
	url             string
	requestHeaders  http.Header
	requestBody     []byte
	statusCode      int
	responseHeaders http.Header
	responseBody    []byte
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("api request failed with status %d", e.StatusCode)
	}

	return fmt.Sprintf("api request failed with status %d: %s", e.StatusCode, e.Message)
}

func New(baseURL string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		logger: logger,
	}
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) SignIn(ctx context.Context, req SignInRequest) (UserSession, error) {
	if _, _, err := c.options(ctx, "/user/signin", map[string]string{
		"Origin":                        "https://obsidian.md",
		"Access-Control-Request-Method": http.MethodPost,
	}); err != nil {
		return UserSession{}, err
	}

	body, exchange, err := c.doRequest(ctx, http.MethodPost, "/user/signin", req, "", map[string]string{
		"Origin": "https://obsidian.md",
	})
	if err != nil {
		return UserSession{}, err
	}

	session, err := extractSession(body)
	if err != nil {
		c.logUnexpectedError("unexpected signin response", exchange, err)
		return UserSession{}, err
	}

	return session, nil
}

func (c *Client) SignOut(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("missing auth token")
	}

	_, _, err := c.postJSON(ctx, "/user/signout", map[string]string{"token": token}, token)

	return err
}

func (c *Client) UserInfo(ctx context.Context, token string) (UserInfo, error) {
	if strings.TrimSpace(token) == "" {
		return UserInfo{}, errors.New("missing auth token")
	}

	body, exchange, err := c.postJSON(ctx, "/user/info", map[string]string{"token": token}, token)
	if err != nil {
		return UserInfo{}, err
	}

	info, err := extractUserInfo(body)
	if err != nil {
		c.logUnexpectedError("unexpected user info response", exchange, err)
		return UserInfo{}, err
	}

	return info, nil
}

func (c *Client) ListVaults(ctx context.Context, token string) (VaultList, error) {
	if strings.TrimSpace(token) == "" {
		return VaultList{}, errors.New("missing auth token")
	}

	body, exchange, err := c.postJSON(ctx, "/vault/list", map[string]any{
		"token":                        token,
		"supported_encryption_version": SupportedEncryptionVersion,
	}, token)
	if err != nil {
		return VaultList{}, err
	}

	list, err := extractVaultList(body)
	if err != nil {
		c.logUnexpectedError("unexpected vault list response", exchange, err)
		return VaultList{}, err
	}

	return list, nil
}

func (c *Client) AccessVault(ctx context.Context, token string, vault Vault, keyHash string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("missing auth token")
	}

	_, _, err := c.postJSON(ctx, "/vault/access", map[string]any{
		"token":              token,
		"vault_uid":          vault.ID,
		"keyhash":            keyHash,
		"host":               vault.Host,
		"encryption_version": vault.EncryptionVersion,
	}, token)

	return err
}

func (c *Client) options(ctx context.Context, endpoint string, headers map[string]string) ([]byte, exchangeLog, error) {
	return c.doRequest(ctx, http.MethodOptions, endpoint, nil, "", headers)
}

func (c *Client) postJSON(ctx context.Context, endpoint string, payload any, token string) ([]byte, exchangeLog, error) {
	return c.doRequest(ctx, http.MethodPost, endpoint, payload, token, nil)
}

func (c *Client) doRequest(ctx context.Context, method string, endpoint string, payload any, token string, extraHeaders map[string]string) ([]byte, exchangeLog, error) {
	exchange := exchangeLog{}
	var requestBody io.Reader
	var requestBodyBytes []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, exchange, fmt.Errorf("encode request: %w", err)
		}
		requestBodyBytes = encoded
		requestBody = bytes.NewReader(encoded)
	}

	requestURL, err := c.endpointURL(endpoint)
	if err != nil {
		return nil, exchange, err
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, requestBody)
	if err != nil {
		return nil, exchange, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}

	exchange = exchangeLog{
		method:         req.Method,
		url:            requestURL,
		requestHeaders: req.Header.Clone(),
		requestBody:    append([]byte(nil), requestBodyBytes...),
	}

	c.logDebugRequest(exchange)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logUnexpectedError("api request failed", exchange, err)
		return nil, exchange, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		exchange.statusCode = resp.StatusCode
		exchange.responseHeaders = resp.Header.Clone()
		c.logUnexpectedError("api response read failed", exchange, err)
		return nil, exchange, fmt.Errorf("read response: %w", err)
	}

	exchange.statusCode = resp.StatusCode
	exchange.responseHeaders = resp.Header.Clone()
	exchange.responseBody = append([]byte(nil), body...)

	c.logDebugResponse(exchange)

	if semanticErr := extractSemanticAPIError(body); semanticErr != nil {
		c.logUnexpectedError("api request returned semantic error", exchange, semanticErr)
		return nil, exchange, semanticErr
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Message:    extractAPIErrorMessage(body),
		}
		c.logUnexpectedError("api request returned non-success status", exchange, apiErr)
		return nil, exchange, apiErr
	}

	return body, exchange, nil
}

func (c *Client) endpointURL(endpoint string) (string, error) {
	baseURL, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse api base URL: %w", err)
	}

	baseURL.Path = path.Join(baseURL.Path, endpoint)

	return baseURL.String(), nil
}

func extractSession(body []byte) (UserSession, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return UserSession{}, fmt.Errorf("decode signin response: %w", err)
	}

	token, ok := findStringField(root, "token", "authToken", "auth_token")
	if !ok || strings.TrimSpace(token) == "" {
		return UserSession{}, errors.New("signin response did not include a token")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return UserSession{
			Token: token,
			User:  body,
		}, nil
	}

	user := fields["user"]
	if len(user) == 0 {
		if nestedUser, ok := findField(root, "user"); ok {
			encoded, err := json.Marshal(nestedUser)
			if err == nil {
				user = encoded
			}
		}
	}

	if len(user) == 0 {
		filtered := make(map[string]json.RawMessage, len(fields))
		for key, value := range fields {
			if isTokenField(key) {
				continue
			}
			filtered[key] = value
		}

		if len(filtered) > 0 {
			encoded, err := json.Marshal(filtered)
			if err != nil {
				return UserSession{}, fmt.Errorf("encode user metadata: %w", err)
			}
			user = encoded
		}
	}

	return UserSession{
		Token: token,
		User:  user,
	}, nil
}

func extractUserInfo(body []byte) (UserInfo, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return UserInfo{}, fmt.Errorf("decode user info response: %w", err)
	}

	info := UserInfo{
		Raw: append(json.RawMessage(nil), body...),
	}

	if name, ok := findStringField(root, "name"); ok {
		info.Name = name
	}
	if email, ok := findStringField(root, "email"); ok {
		info.Email = email
	}

	return info, nil
}

func extractVaultList(body []byte) (VaultList, error) {
	var response struct {
		Vaults []Vault `json:"vaults"`
		Shared []Vault `json:"shared"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return VaultList{}, fmt.Errorf("decode vault list response: %w", err)
	}

	return VaultList{
		Vaults: response.Vaults,
		Shared: response.Shared,
	}, nil
}

func rawString(body json.RawMessage) (string, bool) {
	if len(body) == 0 {
		return "", false
	}

	var value string
	if err := json.Unmarshal(body, &value); err == nil {
		return value, true
	}

	return "", false
}

func findStringField(root any, names ...string) (string, bool) {
	if value, ok := findField(root, names...); ok {
		if text, ok := value.(string); ok {
			return text, true
		}
	}

	return "", false
}

func findField(root any, names ...string) (any, bool) {
	nameSet := make(map[string]struct{}, len(names))
	for _, name := range names {
		nameSet[name] = struct{}{}
	}

	return findFieldRecursive(root, nameSet)
}

func findFieldRecursive(root any, names map[string]struct{}) (any, bool) {
	switch value := root.(type) {
	case map[string]any:
		for key, item := range value {
			if _, ok := names[key]; ok {
				return item, true
			}
		}
		for _, item := range value {
			if found, ok := findFieldRecursive(item, names); ok {
				return found, true
			}
		}
	case []any:
		for _, item := range value {
			if found, ok := findFieldRecursive(item, names); ok {
				return found, true
			}
		}
	}

	return nil, false
}

func isTokenField(key string) bool {
	switch key {
	case "token", "authToken", "auth_token":
		return true
	default:
		return false
	}
}

func extractAPIErrorMessage(body []byte) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err == nil {
		for _, key := range []string{"message", "error", "status"} {
			if value, ok := rawString(fields[key]); ok && strings.TrimSpace(value) != "" {
				return value
			}
		}
	}

	message := strings.TrimSpace(string(body))
	if len(message) > 240 {
		message = message[:240]
	}

	return message
}

func extractSemanticAPIError(body []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil
	}

	for _, key := range []string{"error"} {
		if value, ok := rawString(fields[key]); ok && strings.TrimSpace(value) != "" {
			return errors.New(value)
		}
	}

	return nil
}

func (c *Client) logDebugRequest(exchange exchangeLog) {
	c.logger.LogAttrs(context.Background(), slog.LevelDebug, "api request",
		slog.String("method", exchange.method),
		slog.String("url", exchange.url),
		slog.String("request_headers", headerString(exchange.requestHeaders)),
		slog.String("request_body", payloadString(exchange.url, exchange.requestBody)),
	)
}

func (c *Client) logDebugResponse(exchange exchangeLog) {
	c.logger.LogAttrs(context.Background(), slog.LevelDebug, "api response",
		slog.String("method", exchange.method),
		slog.String("url", exchange.url),
		slog.Int("status", exchange.statusCode),
		slog.String("response_headers", headerString(exchange.responseHeaders)),
		slog.String("response_body", payloadString(exchange.url, exchange.responseBody)),
	)
}

func (c *Client) logUnexpectedError(message string, exchange exchangeLog, err error) {
	c.logger.LogAttrs(context.Background(), slog.LevelError, message,
		slog.Any("err", err),
		slog.String("method", exchange.method),
		slog.String("url", exchange.url),
		slog.String("request_headers", headerString(exchange.requestHeaders)),
		slog.String("request_body", payloadString(exchange.url, exchange.requestBody)),
		slog.Int("status", exchange.statusCode),
		slog.String("response_headers", headerString(exchange.responseHeaders)),
		slog.String("response_body", payloadString(exchange.url, exchange.responseBody)),
	)
}

func headerString(header http.Header) string {
	if len(header) == 0 {
		return "{}"
	}

	return indentJSON(header)
}

func payloadString(url string, body []byte) string {
	if len(body) == 0 {
		return ""
	}

	if shouldOmitPayload(url) {
		return "[omitted payload]"
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err == nil {
		return indentJSON(decoded)
	}

	return string(body)
}

func shouldOmitPayload(url string) bool {
	return strings.Contains(url, "/notes/")
}

func indentJSON(value any) string {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}

	return string(body)
}
