// Package session 申请并解码数据库运维会话。
package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zK0G0w/jumpdb/internal/secdb"
)

var (
	ErrRequest  = errors.New("session: 请求失败")
	ErrHTTP     = errors.New("session: HTTP 状态错误")
	ErrTLS      = errors.New("session: TLS 证书校验失败")
	ErrBusiness = errors.New("session: 业务请求被拒绝")
	ErrResponse = errors.New("session: 响应格式错误")
)

const saltChars = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"

// Options 是调用方提供的门户地址、授权和数据库解密密钥。
type Options struct {
	Portal string
	Token  string
	DBKey  secdb.Key
}

// ResourceID 保留资源 ID 的数字或字符串类型。
type ResourceID struct{ value any }

// NumberID 构造数字资源 ID。
func NumberID(id int64) ResourceID { return ResourceID{value: id} }

// StringID 构造字符串资源 ID，不转换数字字符串。
func StringID(id string) ResourceID { return ResourceID{value: id} }

// Client 管理门户请求与并发安全的时钟偏差。
type Client struct {
	portal string
	token  string
	key    secdb.Key
	http   *http.Client
	mu     sync.Mutex
	offset int64
}

// New 创建客户端并以空资源列表探测服务器时间。
func New(ctx context.Context, options Options) (*Client, error) {
	portal, err := url.Parse(options.Portal)
	if err != nil || portal == nil || (portal.Scheme != "https" && portal.Scheme != "http") || portal.Host == "" || portal.User != nil || portal.RawQuery != "" || portal.Fragment != "" || (portal.Path != "" && portal.Path != "/") || options.Token == "" {
		return nil, fmt.Errorf("%w: 门户地址或 Token 无效", ErrRequest)
	}
	client, err := newHTTPClient()
	if err != nil {
		return nil, err
	}
	result := &Client{portal: strings.TrimSuffix(options.Portal, "/"), token: options.Token, key: options.DBKey, http: client}
	before := time.Now().UnixMilli()
	sum := sha256.Sum256([]byte("probe"))
	response, err := result.post(ctx, []ResourceID{}, strconv.FormatInt(time.Now().UnixMilli(), 10), hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, err
	}
	if timestamp, err := response.timestamp(); err != nil {
		return nil, err
	} else if timestamp != 0 {
		result.offset = timestamp - before
	}
	return result, nil
}

// Login 为资源申请一次新会话；不会自动重试。
func (client *Client) Login(ctx context.Context, id ResourceID) (secdb.Params, error) {
	if id.value == nil {
		return secdb.Params{}, fmt.Errorf("%w: 资源 ID 未设置", ErrRequest)
	}
	salt := make([]byte, 16)
	for index := range salt {
		number, err := rand.Int(rand.Reader, big.NewInt(int64(len(saltChars))))
		if err != nil {
			return secdb.Params{}, fmt.Errorf("%w: 无法生成 nonce", ErrRequest)
		}
		salt[index] = saltChars[number.Int64()]
	}
	client.mu.Lock()
	timestamp := strconv.FormatInt(time.Now().UnixMilli()+client.offset, 10)
	client.mu.Unlock()
	response, err := client.post(ctx, []ResourceID{id}, timestamp, nonceFor(salt, timestamp))
	if err != nil {
		return secdb.Params{}, err
	}
	serverTime, err := response.timestamp()
	if err != nil {
		return secdb.Params{}, err
	}
	if serverTime != 0 {
		client.mu.Lock()
		client.offset = serverTime - time.Now().UnixMilli()
		client.mu.Unlock()
	}
	var code int
	if len(response.Code) == 0 || bytes.Equal(response.Code, []byte("null")) || json.Unmarshal(response.Code, &code) != nil {
		return secdb.Params{}, fmt.Errorf("%w: code 无效", ErrResponse)
	}
	if code != 0 {
		if code == 901 {
			return secdb.Params{}, fmt.Errorf("%w: 资源 ID 无效或无权限 (901)", ErrBusiness)
		}
		if code == 44 {
			return secdb.Params{}, fmt.Errorf("%w: 服务器拒绝请求 (44)", ErrBusiness)
		}
		return secdb.Params{}, ErrBusiness
	}
	var data struct {
		DB *string `json:"db"`
	}
	if len(response.Data) == 0 || json.Unmarshal(response.Data, &data) != nil || data.DB == nil {
		return secdb.Params{}, fmt.Errorf("%w: data.db 无效", ErrResponse)
	}
	params, err := secdb.DecodeDBField(client.key, *data.DB)
	if err != nil {
		return secdb.Params{}, err
	}
	if params.YxzIP == "" || params.SessionID == "" || params.Username == "" {
		return secdb.Params{}, fmt.Errorf("%w: 缺少代理握手必需字段", ErrResponse)
	}
	return params, nil
}

func nonceFor(salt []byte, timestamp string) string {
	hasher := sha256.New()
	hasher.Write(salt)
	hasher.Write([]byte(timestamp))
	return hex.EncodeToString(hasher.Sum(nil))
}

type response struct {
	Code       json.RawMessage `json:"code"`
	Data       json.RawMessage `json:"data"`
	HeartCount json.RawMessage `json:"heartCount"`
}

func (result response) timestamp() (int64, error) {
	if len(result.HeartCount) == 0 || bytes.Equal(result.HeartCount, []byte("null")) {
		return 0, nil
	}
	var heart struct {
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if json.Unmarshal(result.HeartCount, &heart) != nil {
		return 0, fmt.Errorf("%w: heartCount 无效", ErrResponse)
	}
	if len(heart.Timestamp) == 0 || bytes.Equal(heart.Timestamp, []byte("null")) {
		return 0, nil
	}
	var timestamp int64
	if json.Unmarshal(heart.Timestamp, &timestamp) != nil {
		return 0, fmt.Errorf("%w: timestamp 无效", ErrResponse)
	}
	return timestamp, nil
}

func (client *Client) post(ctx context.Context, ids []ResourceID, timestamp, nonce string) (response, error) {
	values := make([]any, len(ids))
	for index, id := range ids {
		values[index] = id.value
	}
	body, err := json.Marshal(struct {
		C struct{} `json:"c"`
		B struct {
			IDs []any `json:"ids"`
		} `json:"b"`
	}{B: struct {
		IDs []any `json:"ids"`
	}{IDs: values}})
	if err != nil {
		return response{}, fmt.Errorf("%w: 无法编码请求", ErrRequest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.portal+"/3.0/hostOpsService/loginResource", bytes.NewReader(body))
	if err != nil {
		return response{}, fmt.Errorf("%w: 无法构造请求", ErrRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", client.portal)
	req.Header.Set("Referer", client.portal+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")
	req.Header.Set("Nonce", nonce)
	req.Header.Set("Timestamp", timestamp)
	req.AddCookie(&http.Cookie{Name: "YAB_AUTH_TOKEN", Value: client.token})
	req.AddCookie(&http.Cookie{Name: "YAB_LANGUAGE", Value: "1"})
	res, err := client.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return response{}, fmt.Errorf("%w: %w", ErrRequest, ctx.Err())
		}
		var verification *tls.CertificateVerificationError
		if errors.As(err, &verification) {
			return response{}, ErrTLS
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return response{}, fmt.Errorf("%w: %w", ErrRequest, context.DeadlineExceeded)
		}
		return response{}, ErrRequest
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return response{}, fmt.Errorf("%w: %d", ErrHTTP, res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil {
		if ctx.Err() != nil {
			return response{}, fmt.Errorf("%w: %w", ErrRequest, ctx.Err())
		}
		return response{}, ErrRequest
	}
	if len(raw) > 1<<20 {
		return response{}, ErrResponse
	}
	var result response
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &result) != nil {
		return response{}, ErrResponse
	}
	return result, nil
}
