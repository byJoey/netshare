// Package enrollment 以短时、一次性密文交换节点叶子凭据，解密密钥不进入 HTTP 请求。
package enrollment

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const maxPairingSize = 1 << 20
const maxEnvelopeSize = 2 << 20

var tokenPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var nodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Receipt 只交给 root 管理脚本；服务目录不保存 Key 或 Invitation。
type Receipt struct {
	URL        string    `json:"url"`
	Key        string    `json:"key"`
	Invitation string    `json:"invitation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type envelope struct {
	Version    int       `json:"version"`
	ExpiresAt  time.Time `json:"expires_at"`
	Nonce      string    `json:"nonce"`
	Ciphertext string    `json:"ciphertext"`
}

// Directory 与控制台状态位于同一服务私有根，CA 则仍位于独立的 root 目录。
func Directory(stateFile string) string { return filepath.Join(filepath.Dir(stateFile), "enrollments") }

func aad(token string, expires time.Time) []byte {
	return []byte("netshare-enrollment/v1|" + token + "|" + expires.UTC().Format(time.RFC3339Nano))
}

// Create 验证四叶子包后加密，只在服务目录落密文和到期元数据。
func Create(origin, prefix, pairingPath, directory string, ttl time.Duration) (Receipt, error) {
	if ttl <= 0 || ttl > time.Hour {
		return Receipt{}, fmt.Errorf("接入码有效期必须大于 0 且不超过 1 小时")
	}
	u, err := url.Parse(strings.TrimRight(origin, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return Receipt{}, fmt.Errorf("控制台访问地址无效")
	}
	if prefix != "" && (strings.ContainsAny(prefix, "?#%\\") || !strings.HasPrefix(prefix, "/") || path.Clean(prefix) != prefix) {
		return Receipt{}, fmt.Errorf("控制台管理路径无效")
	}
	if err := rejectSymlinks(pairingPath); err != nil {
		return Receipt{}, err
	}
	info, err := os.Lstat(pairingPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPairingSize || info.Mode().Perm()&0077 != 0 {
		return Receipt{}, fmt.Errorf("节点安装包必须为仅管理员可读的普通文件，且不超过 1 MiB")
	}
	plain, err := os.ReadFile(pairingPath)
	if err != nil {
		return Receipt{}, fmt.Errorf("读取节点安装包失败")
	}
	if err = validatePairing(plain); err != nil {
		return Receipt{}, err
	}
	key := make([]byte, 32)
	tokenBytes := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return Receipt{}, fmt.Errorf("生成接入密钥失败")
	}
	if _, err = rand.Read(tokenBytes); err != nil {
		return Receipt{}, fmt.Errorf("生成接入码失败")
	}
	token := hex.EncodeToString(tokenBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		return Receipt{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Receipt{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return Receipt{}, fmt.Errorf("生成接入随机数失败")
	}
	expires := time.Now().UTC().Add(ttl).Truncate(time.Millisecond)
	payload := envelope{Version: 1, ExpiresAt: expires, Nonce: base64.RawURLEncoding.EncodeToString(nonce), Ciphertext: base64.RawURLEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, aad(token, expires)))}
	serialized, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}
	root, err := openDirectory(directory, true)
	if err != nil {
		return Receipt{}, err
	}
	defer root.Close()
	file, err := root.OpenFile(token+".json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Receipt{}, fmt.Errorf("保存一次性接入资料失败")
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			root.Remove(token + ".json")
		}
	}()
	if _, err = file.Write(serialized); err != nil {
		return Receipt{}, fmt.Errorf("写入一次性接入资料失败")
	}
	if err = file.Sync(); err != nil {
		return Receipt{}, fmt.Errorf("同步一次性接入资料失败")
	}
	if err = file.Close(); err != nil {
		return Receipt{}, fmt.Errorf("关闭一次性接入资料失败")
	}
	success = true
	keyString := base64.RawURLEncoding.EncodeToString(key)
	endpoint := u.String() + prefix + "/api/enroll/" + token
	return Receipt{URL: endpoint, Key: keyString, Invitation: endpoint + "#" + keyString, ExpiresAt: expires}, nil
}

// Serve 单次原子领取密文。无效、过期和已领取统一返回 404，不泄漏接入状态。
func Serve(directory string, w http.ResponseWriter, r *http.Request, token string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodPost || !tokenPattern.MatchString(token) || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	root, err := openDirectory(directory, false)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	original := token + ".json"
	claimed := ".claimed-" + token + ".json"
	info, err := root.Lstat(original)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxEnvelopeSize {
		http.NotFound(w, r)
		return
	}
	// 同一源文件只能有一个并发请求成功移动；领取后失败也不再重放。
	if err = root.Rename(original, claimed); err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Remove(claimed)
	file, err := root.OpenFile(claimed, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxEnvelopeSize+1))
	if err != nil || len(data) > maxEnvelopeSize {
		http.NotFound(w, r)
		return
	}
	var payload envelope
	if err = decodeEnvelope(data, &payload); err != nil || !time.Now().Before(payload.ExpiresAt) || payload.ExpiresAt.After(time.Now().Add(time.Hour)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func parseURL(value string) (*url.URL, string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Opaque != "" || u.RawPath != "" {
		return nil, "", fmt.Errorf("接入地址格式无效")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 4 || parts[len(parts)-3] != "api" || parts[len(parts)-2] != "enroll" || !tokenPattern.MatchString(parts[len(parts)-1]) || path.Clean(u.Path) != u.Path || strings.Contains(u.Path, "\\") {
		return nil, "", fmt.Errorf("接入地址路径无效")
	}
	return u, parts[len(parts)-1], nil
}

// ParseInvitation 只在本机解析片段内的密钥。调用方不得把完整串传给 curl。
func ParseInvitation(value string) (string, string, error) {
	if len(value) > 8192 || strings.TrimSpace(value) != value {
		return "", "", fmt.Errorf("接入码格式无效")
	}
	u, _, err := parseURL(value)
	if err != nil {
		return "", "", err
	}
	key, err := base64.RawURLEncoding.DecodeString(u.Fragment)
	if err != nil || len(key) != 32 {
		return "", "", fmt.Errorf("接入码缺少有效解密密钥")
	}
	secret := u.Fragment
	u.Fragment, u.RawFragment = "", ""
	return u.String(), secret, nil
}

// Fetch 不发送密钥、不跟随重定向；完整认证解密和归档校验后才落明文安装包。
func Fetch(ctx context.Context, endpoint, keyString, directory string) error {
	u, token, err := parseURL(endpoint)
	if err != nil || u.Fragment != "" {
		return fmt.Errorf("接入地址格式无效；密钥请独立传入或使用 --invitation")
	}
	key, err := base64.RawURLEncoding.DecodeString(keyString)
	if err != nil || len(key) != 32 {
		return fmt.Errorf("接入密钥格式无效")
	}
	abs, err := filepath.Abs(directory)
	if err != nil || directory == "" {
		return fmt.Errorf("请指定新的私密输出目录")
	}
	if _, err = os.Lstat(abs); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("接入输出目录必须尚不存在")
	}
	if err = rejectSymlinks(filepath.Dir(abs)); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return fmt.Errorf("创建接入请求失败")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("接入下载不允许重定向") }}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("接入下载失败，请确认控制台可达；已领取的接入码需重新生成")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("接入码不可用或已过期/领取，请在控制台重新生成")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxEnvelopeSize+1))
	if err != nil || len(data) > maxEnvelopeSize {
		return fmt.Errorf("接入资料下载不完整或过大，请重新生成接入码")
	}
	var payload envelope
	if err = decodeEnvelope(data, &payload); err != nil || !time.Now().Before(payload.ExpiresAt) || payload.ExpiresAt.After(time.Now().Add(time.Hour)) {
		return fmt.Errorf("接入资料无效或已过期")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(payload.Nonce)
	if err != nil {
		return fmt.Errorf("接入资料随机数无效")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(payload.Ciphertext)
	if err != nil {
		return fmt.Errorf("接入密文无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("接入解密失败")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return fmt.Errorf("接入资料随机数无效")
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, aad(token, payload.ExpiresAt))
	if err != nil {
		return fmt.Errorf("接入资料认证失败，密钥错误或密文被修改；请重新生成接入码")
	}
	if err = validatePairing(plain); err != nil {
		return err
	}
	if err = os.Mkdir(abs, 0700); err != nil {
		return fmt.Errorf("创建私密接入目录失败")
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(abs)
		}
	}()
	file, err := os.OpenFile(filepath.Join(abs, "pairing.tar.gz"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("保存节点安装包失败")
	}
	if _, err = file.Write(plain); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("保存节点安装包未完成")
	}
	success = true
	return nil
}

func decodeEnvelope(data []byte, out *envelope) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF || out.Version != 1 {
		return fmt.Errorf("接入资料格式无效")
	}
	return nil
}

func openDirectory(directory string, create bool) (*os.Root, error) {
	if directory == "" {
		return nil, fmt.Errorf("接入资料目录不能为空")
	}
	if create {
		if err := rejectSymlinks(filepath.Dir(directory)); err != nil {
			return nil, err
		}
		if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建接入资料目录失败")
		}
	}
	if err := rejectSymlinks(directory); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("接入资料目录必须是无符号链接的私密目录（0700）")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("打开接入资料目录失败")
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return nil, fmt.Errorf("接入资料目录在操作中发生变化")
	}
	return root, nil
}

func rejectSymlinks(value string) error {
	abs, err := filepath.Abs(value)
	if err != nil {
		return fmt.Errorf("资料路径无效")
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("资料路径不存在或包含符号链接")
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

func validatePairing(data []byte) error {
	if len(data) == 0 || len(data) > maxPairingSize {
		return fmt.Errorf("节点安装包大小无效")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("节点安装包不是有效 gzip 归档")
	}
	defer gz.Close()
	uncompressed, err := io.ReadAll(io.LimitReader(gz, maxPairingSize+1))
	if err != nil || len(uncompressed) > maxPairingSize {
		return fmt.Errorf("节点安装包解压后过大或 gzip 校验失败")
	}
	content := bytes.NewReader(uncompressed)
	reader := tar.NewReader(content)
	seen := map[string]bool{}
	rootName := ""
	var total int64
	allowed := map[string]bool{"agent.json": true, "ca.pem": true, "server.pem": true, "server-key.pem": true}
	for count := 0; ; count++ {
		entry, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil || count >= 8 || entry.Size < 0 || entry.Size > maxPairingSize || entry.Name != path.Clean(entry.Name) || strings.Contains(entry.Name, "\\") {
			return fmt.Errorf("节点安装包归档结构无效")
		}
		parts := strings.Split(entry.Name, "/")
		if len(parts) != 2 || !nodePattern.MatchString(parts[0]) || !allowed[parts[1]] || entry.Typeflag != tar.TypeReg || seen[parts[1]] {
			return fmt.Errorf("节点安装包必须只包含单节点的四个普通叶子文件")
		}
		if rootName == "" {
			rootName = parts[0]
		} else if rootName != parts[0] {
			return fmt.Errorf("节点安装包不能混入其他节点")
		}
		total += entry.Size
		if total > maxPairingSize {
			return fmt.Errorf("节点安装包解压后过大")
		}
		if _, err = io.Copy(io.Discard, reader); err != nil {
			return fmt.Errorf("节点安装包内容不完整")
		}
		seen[parts[1]] = true
	}
	if len(seen) != 4 {
		return fmt.Errorf("节点安装包缺少必要叶子文件")
	}
	// tar 结束之后只能有填充零，拒绝夹带另一份归档或非归档数据。
	trailing, err := io.ReadAll(content)
	if err != nil || len(bytes.Trim(trailing, "\x00")) != 0 {
		return fmt.Errorf("节点安装包含有归档外数据")
	}
	return nil
}
