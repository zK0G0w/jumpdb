package session

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/secdb"
)

func fixture(t *testing.T) (secdb.Key, string) {
	t.Helper()
	raw, err := os.ReadFile("../secdb/testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Keys struct {
			DB string `json:"db"`
		} `json:"keys"`
		Cases []struct {
			Format string `json:"format"`
			Input  string `json:"input"`
			Expect struct {
				YxzIP     string `json:"yxzIp"`
				SessionID string `json:"sessionId"`
				Username  string `json:"username"`
			} `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	key, err := secdb.ParseKey(vectors.Keys.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range vectors.Cases {
		if item.Format == "dbfield" && item.Expect.YxzIP != "" && item.Expect.SessionID != "" && item.Expect.Username != "" {
			return key, item.Input
		}
	}
	t.Fatal("没有完整的合成向量")
	return secdb.Key{}, ""
}

func TestLoginProtocolAndClock(t *testing.T) {
	key, encrypted := fixture(t)
	var mutex sync.Mutex
	var ids []any
	var nonces []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/3.0/hostOpsService/loginResource" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json, text/plain, */*" || request.Header.Get("Origin") != "http://"+request.Host || request.Header.Get("Referer") != "http://"+request.Host+"/" {
			t.Error("请求头或路径不符合协议")
		}
		cookie, err := request.Cookie("YAB_AUTH_TOKEN")
		if err != nil || cookie.Value != "synthetic-token" {
			t.Error("Token Cookie 不符合协议")
		}
		if cookie, err := request.Cookie("YAB_LANGUAGE"); err != nil || cookie.Value != "1" {
			t.Error("语言 Cookie 不符合协议")
		}
		var body struct {
			C map[string]any `json:"c"`
			B struct {
				IDs []any `json:"ids"`
			} `json:"b"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.C == nil {
			t.Error("请求体无效")
		}
		timestamp, err := strconv.ParseInt(request.Header.Get("Timestamp"), 10, 64)
		if err != nil {
			t.Error("timestamp 无效")
		}
		nonce := request.Header.Get("Nonce")
		mutex.Lock()
		defer mutex.Unlock()
		if len(body.B.IDs) == 0 {
			probe := sha256.Sum256([]byte("probe"))
			if nonce != hex.EncodeToString(probe[:]) || abs(timestamp-time.Now().UnixMilli()) > 2000 {
				t.Error("探测 nonce 或时间不正确")
			}
			fmt.Fprintf(writer, `{"heartCount":{"timestamp":%d}}`, time.Now().UnixMilli()+60000)
			return
		}
		offset := int64(60000)
		if len(ids) > 0 {
			offset = 90000
		}
		if abs(timestamp-(time.Now().UnixMilli()+offset)) > 2000 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(nonce) {
			t.Error("校准后的 timestamp 或 nonce 不正确")
		}
		ids = append(ids, body.B.IDs[0])
		nonces = append(nonces, nonce)
		json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": map[string]any{"db": encrypted}, "heartCount": map[string]any{"timestamp": time.Now().UnixMilli() + 90000}})
	}))
	defer server.Close()
	client, err := New(context.Background(), Options{Portal: server.URL, Token: "synthetic-token", DBKey: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []ResourceID{NumberID(856), StringID("00856")} {
		params, err := client.Login(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if params.Username == "" || params.YxzIP == "" || params.SessionID == "" {
			t.Fatal("缺少连接参数")
		}
	}
	if len(ids) != 2 || ids[0] != float64(856) || ids[1] != "00856" || nonces[0] == nonces[1] {
		t.Fatal("ID 类型或随机 nonce 不正确")
	}
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func TestNonceAlgorithm(t *testing.T) {
	salt, timestamp := []byte("ABCDEFGH23456789"), "1700000000123"
	expected := sha256.Sum256([]byte("ABCDEFGH234567891700000000123"))
	if nonceFor(salt, timestamp) != hex.EncodeToString(expected[:]) {
		t.Fatal("nonce 不是 salt 与 timestamp 的 SHA-256")
	}
}

func encryptedObject(t *testing.T, key secdb.Key, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	data := append([]byte(plaintext), bytes.Repeat([]byte{byte(padding)}, padding)...)
	iv := []byte("synthetic-iv-123")
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	encoded := base64.StdEncoding.EncodeToString(data)
	return string(iv[:8]) + encoded[:8] + string(iv[8:]) + encoded[8:]
}

func TestErrorsAndCancellation(t *testing.T) {
	key, encrypted := fixture(t)
	missingFields := encryptedObject(t, key, `{"username":"synthetic-user"}`)
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"http", 503, `synthetic-password synthetic-token`, ErrHTTP},
		{"business", 200, `{"code":901,"msg":"synthetic-password synthetic-token"}`, ErrBusiness},
		{"clock-rejected", 200, `{"code":44,"heartCount":{"timestamp":1700000000000}}`, ErrBusiness},
		{"invalid-json", 200, `synthetic-password synthetic-token`, ErrResponse},
		{"invalid-data", 200, `{"code":0,"data":{"db":42},"msg":"synthetic-password"}`, ErrResponse},
		{"invalid-cipher", 200, `{"code":0,"data":{"db":"synthetic-password"}}`, secdb.ErrFormat},
		{"invalid-clock", 200, `{"code":0,"heartCount":{"timestamp":"synthetic-password"}}`, ErrResponse},
		{"missing-fields", 200, `{"code":0,"data":{"db":"` + missingFields + `"}}`, ErrResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body struct {
					B struct {
						IDs []any `json:"ids"`
					} `json:"b"`
				}
				json.NewDecoder(request.Body).Decode(&body)
				if len(body.B.IDs) == 0 {
					fmt.Fprint(writer, `{}`)
					return
				}
				writer.WriteHeader(test.status)
				fmt.Fprint(writer, test.body)
			}))
			defer server.Close()
			client, err := New(context.Background(), Options{Portal: server.URL, Token: "synthetic-token", DBKey: key})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Login(context.Background(), StringID("resource"))
			if !errors.Is(err, test.want) {
				t.Fatalf("错误分类不符: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-token") || strings.Contains(err.Error(), "synthetic-password") || strings.Contains(err.Error(), encrypted) {
				t.Fatal("错误泄露敏感内容")
			}
		})
	}
	t.Run("cancel", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var body struct {
				B struct {
					IDs []any `json:"ids"`
				} `json:"b"`
			}
			json.NewDecoder(request.Body).Decode(&body)
			if len(body.B.IDs) == 0 {
				fmt.Fprint(writer, `{}`)
				return
			}
			close(started)
			<-request.Context().Done()
		}))
		defer server.Close()
		client, err := New(context.Background(), Options{Portal: server.URL, Token: "synthetic-token", DBKey: key})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := client.Login(ctx, NumberID(1)); done <- err }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("取消未传播: %v", err)
		}
	})
}

func TestProbeAndTLSFailures(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { fmt.Fprint(writer, `{}`) }))
	defer server.Close()
	_, err := New(context.Background(), Options{Portal: server.URL, Token: "synthetic-token"})
	if !errors.Is(err, ErrTLS) {
		t.Fatalf("未知根应返回 TLS 错误: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = New(ctx, Options{Portal: server.URL, Token: "synthetic-token"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("探测取消未传播: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"heartCount":{"timestamp":"bad"}}`)
	}))
	defer bad.Close()
	_, err = New(context.Background(), Options{Portal: bad.URL, Token: "synthetic-token"})
	if !errors.Is(err, ErrResponse) {
		t.Fatalf("无效探测响应应报错: %v", err)
	}
}

func TestConcurrentLogin(t *testing.T) {
	key, encrypted := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": map[string]any{"db": encrypted}, "heartCount": map[string]any{"timestamp": time.Now().UnixMilli()}})
	}))
	defer server.Close()
	client, err := New(context.Background(), Options{Portal: server.URL, Token: "synthetic-token", DBKey: key})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := client.Login(context.Background(), NumberID(1)); err != nil {
				t.Errorf("并发申请失败: %v", err)
			}
		}()
	}
	group.Wait()
}
