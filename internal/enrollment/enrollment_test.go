package enrollment

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const secretMarker = "-----BEGIN PRIVATE KEY-----\n仅属于节点的测试私钥\n-----END PRIVATE KEY-----"

func privateTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pairingBytes(t *testing.T, mutate func(string, *tar.Header) string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"agent.json", "ca.pem", "server.pem", "server-key.pem"} {
		body := []byte("测试叶子文件")
		if name == "server-key.pem" {
			body = []byte(secretMarker)
		}
		header := &tar.Header{Name: "edge-a/" + name, Mode: 0600, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if mutate != nil {
			header.Name = mutate(name, header)
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func pairingFile(t *testing.T, base string) ([]byte, string) {
	t.Helper()
	data := pairingBytes(t, nil)
	p := filepath.Join(base, "pairing.tar.gz")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data, p
}

func tokenOf(t *testing.T, endpoint string) string {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return pathLast(u.Path)
}
func pathLast(p string) string { parts := strings.Split(p, "/"); return parts[len(parts)-1] }

func TestHTTPFetchKeepsKeyOffWireAndOnlyPersistsAuthenticatedPairing(t *testing.T) {
	base := privateTemp(t)
	plain, pairing := pairingFile(t, base)
	directory := filepath.Join(base, "enrollments")
	var receipt Receipt
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Fragment != "" || strings.Contains(r.RequestURI, receipt.Key) || strings.Contains(r.Header.Get("Referer"), receipt.Key) || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("解密密钥或会话进入了HTTP请求")
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Error("接入POST不应发送密钥或正文")
		}
		Serve(directory, w, r, pathLast(r.URL.Path))
	}))
	defer server.Close()
	var err error
	receipt, err = Create(server.URL, "/ns-test0123456789", pairing, directory, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(directory, tokenOf(t, receipt.URL)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(secretMarker)) || bytes.Contains(stored, []byte("PRIVATE KEY")) || bytes.Contains(stored, []byte(receipt.Key)) || bytes.Contains(stored, plain) {
		t.Fatal("服务目录出现明文凭据/解密密钥")
	}
	endpoint, key, err := ParseInvitation(receipt.Invitation)
	if err != nil || strings.Contains(endpoint, "#") || endpoint != receipt.URL || key != receipt.Key {
		t.Fatal("接入串片段未正确剥离", err)
	}
	out := filepath.Join(base, "fetched")
	if err = Fetch(context.Background(), endpoint, key, out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "pairing.tar.gz"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("认证后的包内容不匹配", err)
	}
	for _, p := range []string{out, filepath.Join(out, "pairing.tar.gz")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatal("接入输出权限错误")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("下载应只有一次请求")
	}
	if err = Fetch(context.Background(), endpoint, key, filepath.Join(base, "replay")); err == nil {
		t.Fatal("接入码不能重放")
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 0 {
		t.Fatal("一次性密文领取后未清理")
	}
}

func TestConcurrentClaimIsExactlyOnceAndGETDoesNotConsume(t *testing.T) {
	base := privateTemp(t)
	_, pairing := pairingFile(t, base)
	directory := filepath.Join(base, "enrollments")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Serve(directory, w, r, pathLast(r.URL.Path)) }))
	defer server.Close()
	receipt, err := Create(server.URL, "", pairing, directory, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(receipt.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("GET不应领取")
	}
	var good, bad atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := http.Post(receipt.URL, "application/json", nil)
			if e != nil {
				bad.Add(1)
				return
			}
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				good.Add(1)
			} else if r.StatusCode != 404 {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if good.Load() != 1 || bad.Load() != 0 {
		t.Fatalf("原子领取结果错误：成功%d 异常%d", good.Load(), bad.Load())
	}
}

func TestExpiryWrongKeyAndTamperingLeaveNoPlaintext(t *testing.T) {
	for _, kind := range []string{"过期", "错误密钥", "篡改密文", "篡改到期", "篡改nonce"} {
		t.Run(kind, func(t *testing.T) {
			base := privateTemp(t)
			_, pairing := pairingFile(t, base)
			directory := filepath.Join(base, "enrollments")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Serve(directory, w, r, pathLast(r.URL.Path)) }))
			defer server.Close()
			receipt, err := Create(server.URL, "", pairing, directory, 15*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			stored := filepath.Join(directory, tokenOf(t, receipt.URL)+".json")
			data, _ := os.ReadFile(stored)
			var payload envelope
			json.Unmarshal(data, &payload)
			switch kind {
			case "过期":
				payload.ExpiresAt = time.Now().Add(-time.Second)
			case "错误密钥":
				receipt.Key = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			case "篡改密文":
				ciphertext, _ := base64.RawURLEncoding.DecodeString(payload.Ciphertext)
				ciphertext[0] ^= 1
				payload.Ciphertext = base64.RawURLEncoding.EncodeToString(ciphertext)
			case "篡改到期":
				payload.ExpiresAt = payload.ExpiresAt.Add(time.Minute)
			case "篡改nonce":
				nonce, _ := base64.RawURLEncoding.DecodeString(payload.Nonce)
				nonce[0] ^= 1
				payload.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
			}
			data, _ = json.Marshal(payload)
			os.WriteFile(stored, data, 0600)
			out := filepath.Join(base, "rejected")
			err = Fetch(context.Background(), receipt.URL, receipt.Key, out)
			if err == nil {
				t.Fatal("应拒绝无效认证资料")
			}
			if strings.Contains(err.Error(), receipt.Key) || strings.Contains(err.Error(), tokenOf(t, receipt.URL)) {
				t.Fatal("错误消息泄漏接入秘密")
			}
			if _, e := os.Stat(out); !os.IsNotExist(e) {
				t.Fatal("认证失败不得落明文半包")
			}
			entries, _ := os.ReadDir(directory)
			if len(entries) != 0 {
				t.Fatal("失败领取也不能重放")
			}
		})
	}
}

func TestPairingArchiveAndDirectoryGuards(t *testing.T) {
	for _, kind := range []string{"穿越", "符号链接", "其它节点", "额外私钥", "重复文件", "gzip损坏"} {
		t.Run(kind, func(t *testing.T) {
			data := pairingBytes(t, func(name string, h *tar.Header) string {
				switch kind {
				case "穿越":
					if name == "agent.json" {
						return "../agent.json"
					}
				case "符号链接":
					if name == "agent.json" {
						h.Typeflag = tar.TypeSymlink
						h.Linkname = "/etc/passwd"
						h.Size = 0
					}
				case "其它节点":
					if name == "agent.json" {
						return "edge-b/agent.json"
					}
				case "额外私钥":
					if name == "ca.pem" {
						return "edge-a/ca-key.pem"
					}
				case "重复文件":
					if name == "ca.pem" {
						return "edge-a/agent.json"
					}
				}
				return h.Name
			})
			if kind == "gzip损坏" {
				data[len(data)-1] ^= 1
			}
			if err := validatePairing(data); err == nil {
				t.Fatal("应拒绝危险归档")
			}
		})
	}
	base := privateTemp(t)
	_, pairing := pairingFile(t, base)
	public := filepath.Join(base, "public")
	os.Mkdir(public, 0755)
	if _, err := Create("http://192.0.2.10:18440", "", pairing, public, 15*time.Minute); err == nil {
		t.Fatal("应拒绝公开权限服务目录")
	}
	link := filepath.Join(base, "link")
	os.Symlink(public, link)
	if _, err := Create("http://192.0.2.10:18440", "", pairing, link, 15*time.Minute); err == nil {
		t.Fatal("应拒绝符号链接服务目录")
	}
	if _, err := Create("http://192.0.2.10:18440", "", pairing, filepath.Join(base, "too-long"), 2*time.Hour); err == nil {
		t.Fatal("应限制邀请有效期")
	}
	if _, _, err := ParseInvitation("http://192.0.2.10:18440/api/enroll/invalid#bad"); err == nil {
		t.Fatal("应拒绝无效接入串")
	}
}
