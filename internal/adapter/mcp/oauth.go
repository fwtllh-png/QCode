package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var ErrTokenNotFound = errors.New("MCP OAuth token not found")

type OAuthProvider interface {
	Authorization(context.Context) (string, error)
}

type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

type TokenStore interface {
	Load(context.Context, string) (OAuthToken, error)
	Save(context.Context, string, OAuthToken) error
	Delete(context.Context, string) error
}

type BrowserOpener interface {
	Open(context.Context, string) error
}

type BrowserOpenFunc func(context.Context, string) error

func (f BrowserOpenFunc) Open(ctx context.Context, target string) error {
	return f(ctx, target)
}

type FileTokenStore struct {
	directory string
}

func NewFileTokenStore(directory string) (*FileTokenStore, error) {
	if directory == "" {
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return nil, errors.New("resolve MCP OAuth token directory")
		}
		directory = filepath.Join(configDirectory, "qcode", "mcp-oauth")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, errors.New("create MCP OAuth token directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("secure MCP OAuth token directory")
	}
	return &FileTokenStore{directory: directory}, nil
}

func (s *FileTokenStore) Load(ctx context.Context, key string) (OAuthToken, error) {
	if err := ctx.Err(); err != nil {
		return OAuthToken{}, err
	}
	data, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return OAuthToken{}, ErrTokenNotFound
	}
	if err != nil {
		return OAuthToken{}, errors.New("read MCP OAuth token")
	}
	var token OAuthToken
	if err := DecodeStrict(data, &token); err != nil {
		return OAuthToken{}, errors.New("decode MCP OAuth token")
	}
	return token, nil
}

func (s *FileTokenStore) Save(ctx context.Context, key string, token OAuthToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(token)
	if err != nil {
		return errors.New("encode MCP OAuth token")
	}
	temporary, err := os.CreateTemp(s.directory, ".token-*")
	if err != nil {
		return errors.New("create MCP OAuth token")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("secure MCP OAuth token")
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return errors.New("write MCP OAuth token")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("sync MCP OAuth token")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close MCP OAuth token")
	}
	if err := os.Rename(temporaryName, s.path(key)); err != nil {
		return errors.New("replace MCP OAuth token")
	}
	return nil
}

func (s *FileTokenStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("delete MCP OAuth token")
	}
	return nil
}

func (s *FileTokenStore) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.directory, hex.EncodeToString(sum[:])+".json")
}

type OAuthManager struct {
	config OAuthConfig
	client *http.Client
	store  TokenStore
	opener BrowserOpener
	key    string

	mu sync.Mutex
}

func NewOAuthManager(
	config OAuthConfig,
	client *http.Client,
	store TokenStore,
	opener BrowserOpener,
) (*OAuthManager, error) {
	if err := validateOAuth(&config); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{}
	}
	if store == nil {
		var err error
		store, err = NewFileTokenStore(config.StorePath)
		if err != nil {
			return nil, err
		}
	}
	if opener == nil {
		opener = BrowserOpenFunc(openBrowser)
	}
	identity := strings.Join([]string{
		"mcp-oauth-v1",
		config.ClientID,
		config.AuthorizationEndpoint,
		config.TokenEndpoint,
	}, "\x00")
	return &OAuthManager{
		config: config,
		client: client,
		store:  store,
		opener: opener,
		key:    identity,
	}, nil
}

func (m *OAuthManager) Authorization(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token, err := m.store.Load(ctx, m.key)
	if err != nil && !errors.Is(err, ErrTokenNotFound) {
		return "", err
	}
	if err == nil && token.AccessToken != "" && tokenValid(token) {
		return authorizationValue(token), nil
	}
	if err == nil && token.RefreshToken != "" {
		refreshed, refreshErr := m.exchange(ctx, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {token.RefreshToken},
			"client_id":     {m.config.ClientID},
		})
		if refreshErr == nil {
			if refreshed.RefreshToken == "" {
				refreshed.RefreshToken = token.RefreshToken
			}
			if err := m.store.Save(ctx, m.key, refreshed); err != nil {
				return "", err
			}
			return authorizationValue(refreshed), nil
		}
		_ = m.store.Delete(ctx, m.key)
	}
	token, err = m.authorize(ctx)
	if err != nil {
		return "", err
	}
	if err := m.store.Save(ctx, m.key, token); err != nil {
		return "", err
	}
	return authorizationValue(token), nil
}

func tokenValid(token OAuthToken) bool {
	return token.Expiry.IsZero() || time.Until(token.Expiry) > 30*time.Second
}

func authorizationValue(token OAuthToken) string {
	tokenType := strings.TrimSpace(token.TokenType)
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return tokenType + " " + token.AccessToken
}

func (m *OAuthManager) authorize(ctx context.Context) (OAuthToken, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return OAuthToken{}, errors.New("start MCP OAuth callback")
	}
	defer listener.Close()

	verifier, err := randomURLValue(48)
	if err != nil {
		return OAuthToken{}, err
	}
	state, err := randomURLValue(24)
	if err != nil {
		return OAuthToken{}, err
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	redirectURI := "http://" + listener.Addr().String() + "/callback"

	callback := make(chan struct {
		code string
		err  error
	}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(writer http.ResponseWriter, request *http.Request) {
		result := struct {
			code string
			err  error
		}{}
		query := request.URL.Query()
		switch {
		case query.Get("state") != state:
			result.err = errors.New("MCP OAuth callback state mismatch")
		case query.Get("error") != "":
			result.err = errors.New("MCP OAuth authorization denied")
		case query.Get("code") == "":
			result.err = errors.New("MCP OAuth callback omitted code")
		default:
			result.code = query.Get("code")
		}
		select {
		case callback <- result:
		default:
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(writer, "Authorization received. You may close this window.")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.Shutdown(shutdownCtx)
		cancel()
		<-serverDone
	}()

	authorizationURL, err := url.Parse(m.config.AuthorizationEndpoint)
	if err != nil {
		return OAuthToken{}, errors.New("parse MCP OAuth authorization endpoint")
	}
	query := authorizationURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", m.config.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	if len(m.config.Scopes) != 0 {
		query.Set("scope", strings.Join(m.config.Scopes, " "))
	}
	authorizationURL.RawQuery = query.Encode()
	if err := m.opener.Open(ctx, authorizationURL.String()); err != nil {
		return OAuthToken{}, errors.New("open MCP OAuth authorization")
	}

	waitCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		waitCtx, cancel = context.WithTimeout(ctx, m.config.CallbackTimeout)
	}
	defer cancel()
	select {
	case result := <-callback:
		if result.err != nil {
			return OAuthToken{}, result.err
		}
		return m.exchange(ctx, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {result.code},
			"client_id":     {m.config.ClientID},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
		})
	case <-waitCtx.Done():
		return OAuthToken{}, waitCtx.Err()
	}
}

func (m *OAuthManager) exchange(ctx context.Context, values url.Values) (OAuthToken, error) {
	if m.config.ClientSecretEnv != "" {
		secret := os.Getenv(m.config.ClientSecretEnv)
		if secret == "" {
			return OAuthToken{}, errors.New("MCP OAuth client secret environment variable is empty")
		}
		values.Set("client_secret", secret)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		m.config.TokenEndpoint,
		strings.NewReader(values.Encode()),
	)
	if err != nil {
		return OAuthToken{}, errors.New("create MCP OAuth token request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := m.client.Do(request)
	if err != nil {
		return OAuthToken{}, errors.New("execute MCP OAuth token request")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return OAuthToken{}, errors.New("read MCP OAuth token response")
	}
	if len(body) > 1<<20 {
		return OAuthToken{}, errors.New("MCP OAuth token response exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return OAuthToken{}, fmt.Errorf("MCP OAuth token request failed with status %d", response.StatusCode)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := DecodeStrict(body, &payload); err != nil {
		return OAuthToken{}, errors.New("decode MCP OAuth token response")
	}
	if payload.AccessToken == "" {
		return OAuthToken{}, errors.New("MCP OAuth token response omitted access token")
	}
	token := OAuthToken{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		TokenType:    payload.TokenType,
	}
	if payload.ExpiresIn > 0 {
		token.Expiry = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	return token, nil
}

func randomURLValue(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("generate MCP OAuth secret")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func openBrowser(ctx context.Context, target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", target)
	default:
		return fmt.Errorf("opening a browser is unsupported on %s", runtime.GOOS)
	}
	if err := command.Start(); err != nil {
		return err
	}
	return nil
}
