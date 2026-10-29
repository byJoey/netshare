// Package registration 提供面板发起、节点自报的接入协议。
package registration

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"

	"netshare/internal/model"
)

var tokenPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Envelope struct {
	Nonce []byte `json:"nonce"`
	Data  []byte `json:"data"`
}

type Info struct {
	Peers        []string `json:"peers"`
	ControllerCA []byte   `json:"controller_ca"`
	IssuerCA     []byte   `json:"issuer_ca"`
	SourceIP     string   `json:"source_ip,omitempty"`
}

type Report struct {
	Node                model.Node `json:"node"`
	CSR                 []byte     `json:"csr"`
	PreviousCertificate []byte     `json:"previous_certificate,omitempty"`
	Problem             string     `json:"problem,omitempty"`
}

type Reply struct {
	Node           model.Node `json:"node"`
	Certificate    []byte     `json:"certificate"`
	TrustBundle    []byte     `json:"trust_bundle"`
	AllowedTargets []string   `json:"allowed_targets"`
}

func NewSecret() (string, string, error) {
	id, key := make([]byte, 16), make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(key); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(id), base64.RawURLEncoding.EncodeToString(key), nil
}

func ValidToken(id string) bool { return tokenPattern.MatchString(id) }

func ParseInvitation(value string) (string, string, error) {
	if len(value) > 8192 || strings.ContainsAny(value, "\r\n\\") {
		return "", "", fmt.Errorf("接入命令不完整，请回面板重新复制")
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Opaque != "" || u.EscapedPath() != u.Path || path.Clean(u.Path) != u.Path {
		return "", "", fmt.Errorf("接入地址无效，请回面板重新复制")
	}
	parts := strings.Split(u.Path, "/api/join/")
	if len(parts) != 2 || !ValidToken(parts[1]) {
		return "", "", fmt.Errorf("接入地址无效，请回面板重新生成")
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(u.Fragment)
	if err != nil || len(key) != 32 || len(u.Fragment) != 43 {
		return "", "", fmt.Errorf("接入码不完整，请复制整条命令")
	}
	secret := u.Fragment
	u.Fragment = ""
	return u.String(), secret, nil
}

func aead(secret string) (cipher.AEAD, error) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("接入码无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Seal(secret, context string, value any) (Envelope, error) {
	box, err := aead(secret)
	if err != nil {
		return Envelope{}, err
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, box.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	return Envelope{Nonce: nonce, Data: box.Seal(nil, nonce, plain, []byte(context))}, nil
}

func Open(secret, context string, envelope Envelope, value any) error {
	box, err := aead(secret)
	if err != nil || len(envelope.Nonce) != 12 || len(envelope.Data) > 128<<10 {
		return fmt.Errorf("接入认证失败，请回面板重新生成命令")
	}
	plain, err := box.Open(nil, envelope.Nonce, envelope.Data, []byte(context))
	if err != nil {
		return fmt.Errorf("接入认证失败，请回面板重新生成命令")
	}
	return Decode(plain, value)
}

func Decode(data []byte, value any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("接入数据格式无效")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("接入数据含多余内容")
	}
	return nil
}
